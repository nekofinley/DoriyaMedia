package bangumi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/metadata"
	"doriyamedia/internal/models"
)

const (
	jobPending    = "pending"
	jobProcessing = "processing"
	jobDone       = "done"
	jobFailed     = "failed"
)

var errNotSyncable = errors.New("内容未匹配到 Bangumi 番剧条目")

// Service owns per-user Bangumi credentials and a durable outbox. Progress
// saves only enqueue local work; the worker performs network calls serially so
// playback never waits on Bangumi and a restart can resume unfinished syncs.
type Service struct {
	Store    *db.Store
	Metadata *metadata.Manager
	Cfg      *config.Config

	clientFor   func(token string) *Client
	coverClient func() *http.Client
	wake        chan struct{}
	stop        chan struct{}
	startOnce   sync.Once
	stopOnce    sync.Once
	userLocks   sync.Map // local user ID -> *sync.Mutex
}

func NewService(store *db.Store, meta *metadata.Manager, cfg *config.Config) *Service {
	s := &Service{
		Store: store, Metadata: meta, Cfg: cfg,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	s.clientFor = func(token string) *Client {
		return NewClient(token, cfg.BGM.APIBase, bgmHTTPClient(cfg, 35*time.Second))
	}
	s.coverClient = func() *http.Client {
		return bgmHTTPClient(cfg, 20*time.Second)
	}
	return s
}

func bgmHTTPClient(cfg *config.Config, timeout time.Duration) *http.Client {
	c := cfg.BGM
	if !c.UseProxy {
		client, _ := metadata.HTTPClientFor("", timeout)
		return client
	}
	client, err := metadata.HTTPClientFor(c.Proxy, timeout)
	if err != nil {
		log.Printf("Bangumi 同步代理配置无效(%v),本次直连", err)
		client, _ = metadata.HTTPClientFor("", timeout)
	}
	return client
}

func (s *Service) Start() {
	s.startOnce.Do(func() {
		go s.loop()
	})
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) loop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		// A crash can leave a lease behind while the process keeps running,
		// for example after an interrupted database write. Reclaim expired
		// claims on every wake/tick instead of only once at startup.
		if err := s.Store.RecoverBangumiSyncJobs(time.Now()); err != nil {
			log.Printf("Bangumi 同步队列恢复失败: %v", err)
		}
		s.processDue(8)
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

func (s *Service) processDue(limit int) {
	for i := 0; i < limit; i++ {
		more, err := s.processOne(context.Background())
		if err != nil {
			log.Printf("Bangumi 进度同步失败: %v", err)
		}
		if !more {
			return
		}
	}
}

// Status returns the safe account view used by the settings dialog. The token
// itself never leaves the server.
func (s *Service) Status(userID int64) (*models.BangumiSyncStatus, error) {
	st := &models.BangumiSyncStatus{}
	a, err := s.Store.BangumiAccountByUser(userID)
	switch {
	case err == nil:
		connectedAt := a.ConnectedAt
		st.Connected = true
		st.Username = a.Username
		st.Nickname = a.Nickname
		st.AvatarURL = a.AvatarURL
		st.SyncEnabled = a.SyncEnabled
		st.ConnectedAt = &connectedAt
		st.LastSyncAt = a.LastSyncAt
		st.LastError = a.LastError
	case errors.Is(err, sql.ErrNoRows):
		// Not connected is a valid state, not an error.
	default:
		return nil, err
	}
	pending, failed, synced, err := s.Store.BangumiSyncStats(userID)
	if err != nil {
		return nil, err
	}
	st.Pending, st.Failed, st.Synced = pending, failed, synced
	return st, nil
}

func (s *Service) Connect(ctx context.Context, userID int64, token string) (*models.BangumiSyncStatus, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("请填写 Bangumi 个人访问令牌")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	me, err := s.clientFor(token).Me(ctx)
	if err != nil {
		return nil, friendlyAuthError(err)
	}
	enc, err := encryptToken(s.Cfg.Secret, userID, token)
	if err != nil {
		return nil, fmt.Errorf("保存令牌失败: %w", err)
	}
	a := &models.BangumiAccount{
		UserID: userID, Username: me.Username, Nickname: me.Nickname,
		AvatarURL: me.Avatar.Large, TokenEnc: enc, SyncEnabled: true,
		ConnectedAt: time.Now(),
	}
	if a.AvatarURL == "" {
		a.AvatarURL = me.Avatar.Medium
	}
	unlock := s.lockUser(userID)
	err = s.Store.SaveBangumiCredential(a)
	unlock()
	if err != nil {
		return nil, err
	}
	s.Wake()
	return s.Status(userID)
}

func friendlyAuthError(err error) error {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("Bangumi 令牌无效、已过期或没有收藏写入权限")
		case http.StatusTooManyRequests:
			return fmt.Errorf("Bangumi 请求过于频繁,请稍后再试")
		}
	}
	return fmt.Errorf("验证 Bangumi 账户失败: %w", err)
}

func (s *Service) Disconnect(userID int64) error {
	unlock := s.lockUser(userID)
	defer unlock()
	return s.Store.DeleteBangumiAccount(userID)
}

func (s *Service) SetSyncEnabled(userID int64, enabled bool) error {
	unlock := s.lockUser(userID)
	defer unlock()
	if _, err := s.Store.BangumiAccountByUser(userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("请先连接 Bangumi 账户")
		}
		return err
	}
	if err := s.Store.SetBangumiSyncEnabled(userID, enabled); err != nil {
		return err
	}
	if enabled {
		_ = s.Store.ResumePausedBangumiJobs(userID, time.Now())
		s.Wake()
	} else {
		// A paused queue is intentional; an old network error should not keep
		// looking like a broken connection while sync is off.
		_ = s.Store.SetBangumiAccountError(userID, "")
	}
	return nil
}

// lockUser serializes a credential change with the worker's remote writes for
// the same local account. The worker holds this through its final database
// update, so a token or identity switch cannot race an in-flight write.
func (s *Service) lockUser(userID int64) func() {
	value, _ := s.userLocks.LoadOrStore(userID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnqueueWatched is called after local progress reaches the watched threshold.
// It performs only local reads and a local insert, so the player's save
// response never waits for Bangumi. The worker resolves the chapter later.
func (s *Service) EnqueueWatched(userID, fileID int64) error {
	a, err := s.Store.BangumiAccountByUser(userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !a.SyncEnabled {
		return nil
	}
	target, err := s.resolveTarget(fileID)
	if err != nil {
		if errors.Is(err, errNotSyncable) {
			return nil
		}
		return err
	}
	if err := s.Store.EnqueueBangumiSync(userID, fileID, target.SubjectID, target.EpisodeNumber, time.Now()); err != nil {
		return err
	}
	s.Wake()
	return nil
}

// Backfill re-queues every local episode the viewer has finished (>=90%) and
// also revives failed jobs, so it doubles as a retry button.
func (s *Service) Backfill(userID int64) (int64, error) {
	a, err := s.Store.BangumiAccountByUser(userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("请先连接 Bangumi 账户")
	}
	if err != nil {
		return 0, err
	}
	if !a.SyncEnabled {
		return 0, fmt.Errorf("请先开启自动同步")
	}
	files, err := s.Store.CompletedProgressFiles(userID)
	if err != nil {
		return 0, err
	}
	beforePending, beforeFailed, beforeSynced, err := s.Store.BangumiSyncStats(userID)
	if err != nil {
		return 0, err
	}
	queued := int64(0)
	for _, fileID := range files {
		target, resolveErr := s.resolveTarget(fileID)
		if resolveErr != nil {
			if errors.Is(resolveErr, errNotSyncable) {
				continue
			}
			return queued, resolveErr
		}
		if err := s.Store.EnqueueBangumiSync(userID, fileID, target.SubjectID, target.EpisodeNumber, time.Now()); err != nil {
			return queued, err
		}
	}
	if _, err := s.Store.RetryBangumiSyncJobs(userID, time.Now()); err != nil {
		return queued, err
	}
	afterPending, afterFailed, afterSynced, err := s.Store.BangumiSyncStats(userID)
	if err != nil {
		return queued, err
	}
	queued = int64((afterPending + afterFailed + afterSynced) -
		(beforePending + beforeFailed + beforeSynced))
	if queued < 0 {
		queued = 0
	}
	s.Wake()
	return queued, nil
}

type syncTarget struct {
	SubjectID     int
	EpisodeNumber int
}

func (s *Service) resolveTarget(fileID int64) (*syncTarget, error) {
	f, err := s.Store.FileByID(fileID)
	if err != nil {
		return nil, err
	}
	it, err := s.Store.ItemByID(f.ItemID)
	if err != nil {
		return nil, err
	}
	if it.Type != "episode" || it.Episode <= 0 {
		return nil, errNotSyncable
	}
	cur := it
	for depth := 0; depth < 6 && cur != nil; depth++ {
		if cur.BGMID > 0 {
			return &syncTarget{SubjectID: cur.BGMID, EpisodeNumber: it.Episode}, nil
		}
		if cur.ParentID == nil {
			break
		}
		cur, err = s.Store.ItemByID(*cur.ParentID)
		if err != nil {
			return nil, err
		}
	}
	return nil, errNotSyncable
}

// bangumiEpisodeForNumber maps a local episode number to a Bangumi episode ID.
// The worker calls it after the progress save has returned.
func (s *Service) bangumiEpisodeForNumber(ctx context.Context, subjectID, number int) (int, error) {
	eps, err := s.Metadata.BangumiEpisodes(ctx, subjectID)
	if err != nil {
		return 0, err
	}
	if id := bangumiEpisodeNumberID(eps, number); id > 0 {
		return id, nil
	}
	if fresh, refreshErr := s.Metadata.RefreshBangumiEpisodes(ctx, subjectID); refreshErr == nil {
		if id := bangumiEpisodeNumberID(fresh, number); id > 0 {
			return id, nil
		}
	}
	return 0, fmt.Errorf("Bangumi 条目 #%d 中找不到第 %d 集", subjectID, number)
}

func bangumiEpisodeNumberID(eps []metadata.BgmEpisodeMeta, number int) int {
	for _, ep := range eps {
		if ep.Number == number && ep.ID > 0 {
			return ep.ID
		}
	}
	return 0
}

func (s *Service) processOne(parent context.Context) (bool, error) {
	now := time.Now()
	job, err := s.Store.ClaimBangumiSyncJob(now)
	if err != nil || job == nil {
		return false, err
	}
	unlock := s.lockUser(job.UserID)
	defer unlock()
	a, err := s.Store.BangumiAccountByUser(job.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		s.finishJob(job, errors.New("Bangumi 账户已断开"), now, true)
		return true, nil
	}
	if err != nil {
		s.retryJob(job, err, now)
		return true, err
	}
	if a.Generation != job.AccountGen {
		// Switching back to the old account must not replay the old identity's
		// queue. Delete the stale job; a fresh local completion can enqueue a
		// new one under the current generation.
		_ = s.Store.DeleteBangumiJobIfLeased(job.ID, job.LeaseToken)
		return true, nil
	}
	if !a.SyncEnabled {
		_ = s.Store.RequeueBangumiJobIfLeased(job.ID, job.LeaseToken, now.Add(6*time.Hour), "自动同步已关闭")
		return true, nil
	}
	token, err := decryptToken(s.Cfg.Secret, a.UserID, a.TokenEnc)
	if err != nil {
		s.finishJob(job, err, now, true)
		return true, nil
	}
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	client := s.clientFor(token)
	episodeNumber := job.EpisodeNumber
	episodeID := job.EpisodeID
	// A legacy job may predate the local episode snapshot. Recover it before
	// resolving the remote chapter so a rescan cannot silently point the job at
	// a different episode.
	if episodeNumber <= 0 {
		target, resolveErr := s.resolveTarget(job.FileID)
		if resolveErr != nil {
			s.handleSyncError(job, resolveErr, now)
			return true, nil
		}
		episodeNumber = target.EpisodeNumber
	}
	if episodeID == 0 {
		episodeID, err = s.bangumiEpisodeForNumber(ctx, job.SubjectID, episodeNumber)
		if err != nil {
			s.handleSyncError(job, err, now)
			return true, nil
		}
	}
	if err := s.Store.SetBangumiJobTarget(job.ID, job.SubjectID, episodeID, episodeNumber, job.AccountGen, job.LeaseToken); err != nil {
		if errors.Is(err, db.ErrBangumiJobLeaseLost) {
			return true, nil
		}
		return true, err
	}
	job.EpisodeID = episodeID
	job.EpisodeNumber = episodeNumber
	// Validate the episode before creating a collection: a wrong subject must
	// not leave a stray 在看 entry behind.
	if err := client.EnsureCollection(ctx, a.Username, job.SubjectID); err != nil {
		s.handleSyncError(job, err, now)
		return true, nil
	}
	if err := client.MarkWatched(ctx, job.SubjectID, []int{episodeID}); err != nil {
		s.handleSyncError(job, err, now)
		return true, nil
	}

	job.Status = jobDone
	job.Attempts++
	job.LastError = ""
	job.NextAttemptAt = now
	job.UpdatedAt = now
	if err := s.Store.UpdateBangumiSyncJob(job); err != nil {
		if errors.Is(err, db.ErrBangumiJobLeaseLost) {
			return true, nil
		}
		return true, err
	}
	if err := s.Store.MarkBangumiSyncSuccess(job.UserID, now); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Service) handleSyncError(job *models.BangumiSyncJob, err error, now time.Time) {
	permanent := isPermanent(err)
	s.finishJob(job, err, now, permanent)
}

func isPermanent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func (s *Service) finishJob(job *models.BangumiSyncJob, err error, now time.Time, permanent bool) {
	if !permanent {
		s.retryJob(job, err, now)
		return
	}
	job.Attempts++
	job.LastError = err.Error()
	job.UpdatedAt = now
	job.Status = jobFailed
	job.NextAttemptAt = now
	if err := s.Store.UpdateBangumiSyncJob(job); err == nil {
		_ = s.Store.SetBangumiAccountError(job.UserID, job.LastError)
	}
}

func (s *Service) retryJob(job *models.BangumiSyncJob, err error, now time.Time) {
	job.Attempts++
	job.LastError = err.Error()
	job.UpdatedAt = now
	if job.Attempts >= 6 {
		job.Status = jobFailed
		job.NextAttemptAt = now
	} else {
		job.Status = jobPending
		job.NextAttemptAt = now.Add(retryDelay(job.Attempts))
	}
	if err := s.Store.UpdateBangumiSyncJob(job); err == nil {
		_ = s.Store.SetBangumiAccountError(job.UserID, job.LastError)
	}
}

func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	case 4:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}
