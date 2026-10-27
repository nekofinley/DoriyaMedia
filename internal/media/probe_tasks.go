package media

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// errNoVideoStream marks a source whose streams contain no video track.
var errNoVideoStream = errors.New("未识别到视频流(可能是音频或图片文件)")

// This file makes probing asynchronous and observable.
//
// Probing is the first thing every playback needs, and on a cloud drive it is
// also the slowest: ffprobe has to pull a window of the file before the
// decision (direct / proxy / remux / transcode) can be made. Blocking the
// /api/play response on it meant the player sat on a spinner for minutes with
// no way to tell "still working" from "dead". Now the request answers
// immediately with state=probing, a background job does the work, and the
// client polls /api/play/{id}/status until it is ready.

// ProbeStatus is the observable state of one file's probe job.
type ProbeStatus struct {
	State   string  // idle | probing | ready | failed
	Elapsed float64 // seconds since the job started
	Error   string
}

// probeJob is one in-flight (or recently finished) probe of one file.
//
// probe/err are written by the leader goroutine before done is closed and read
// by followers only after <-done, so they need no lock. state/errMsg/finished
// are read concurrently by status polls and are guarded by Engine.probeMu.
type probeJob struct {
	file *models.MediaFile
	done chan struct{}

	state    string // probing | ready | failed
	started  time.Time
	finished time.Time
	errMsg   string

	probe *Probe
	err   error
}

type ticketProbeJob struct {
	done           chan struct{}
	finished       time.Time
	probe          *Probe
	err            error
	leaderCanceled bool
}

type ticketProbeKey struct {
	fileID int64
	size   int64
	mtime  int64
}

// ProbeFromCache parses a stored probe when it is complete enough to decide
// playback. Mirrors the acceptance rule used by EnsureProbe.
func ProbeFromCache(file *models.MediaFile) *Probe {
	if file == nil || file.Probe == "" {
		return nil
	}
	var p Probe
	if err := jsonUnmarshal(file.Probe, &p); err != nil {
		return nil
	}
	if p.Video() != nil || len(p.Streams) == 0 {
		return &p
	}
	return nil
}

// probeTimeoutMs bounds one background probe. A shallow window on a slow link
// can still take a while, but the client should get a verdict either way.
const probeTimeout = 150 * time.Second

// failedProbeCooldown stops repeated /api/play calls from hammering a source
// whose probe just failed.
const failedProbeCooldown = 30 * time.Second

// ticketProbeResultTTL keeps a pure probe result long enough for a burst of
// ticket requests to share it. It is memory-only and never populates file
// metadata.
const ticketProbeResultTTL = 30 * time.Second

// ticketProbeMaxJobs bounds the memory-only probe registry. Callers queue for
// an entry instead of bypassing single-flight when every entry is in flight.
const ticketProbeMaxJobs = 1024

// ticketProbeMaxConcurrent bounds actual ticket-driven ffprobe passes. The
// larger registry still lets many callers join their per-file in-flight job
// while only a few processes touch a source at once.
const ticketProbeMaxConcurrent = 4

// ProbeStatus reports the state of the probe job for a file.
// A file whose probe is already persisted reports ready.
func (e *Engine) ProbeStatus(fileID int64) ProbeStatus {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	job, ok := e.probeJobs[fileID]
	if !ok {
		return ProbeStatus{State: "idle"}
	}
	st := ProbeStatus{State: job.state, Error: job.errMsg}
	start := job.started
	if job.state != "probing" && !job.finished.IsZero() {
		st.Elapsed = job.finished.Sub(start).Seconds()
		return st
	}
	st.Elapsed = time.Since(start).Seconds()
	return st
}

// StartProbe begins a background probe for a file. It never blocks: callers
// answer the client immediately and let the job run. A file already being
// probed is left alone; one whose probe just failed is retried only after
// failedProbeCooldown, so a retry loop cannot hammer the source.
func (e *Engine) StartProbe(file *models.MediaFile, drv source.Driver) {
	if file == nil || ProbeFromCache(file) != nil {
		return
	}
	e.probeMu.Lock()
	e.gcProbeJobsLocked()
	if prev, ok := e.probeJobs[file.ID]; ok {
		if prev.state == "probing" {
			e.probeMu.Unlock()
			return
		}
		retryAfterGenerationChange := errors.Is(prev.err, db.ErrMediaFileChanged)
		if !retryAfterGenerationChange &&
			time.Since(prev.finished) < failedProbeCooldown {
			e.probeMu.Unlock()
			return
		}
	}
	job := &probeJob{
		file:    file,
		done:    make(chan struct{}),
		state:   "probing",
		started: time.Now(),
	}
	e.probeJobs[file.ID] = job
	e.probeMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		e.runProbeJob(ctx, file, drv, job)
	}()
}

// EnsureProbe returns a cached probe or probes the file synchronously, joining
// an in-flight background job when one exists (single-flight per file).
// The HLS/subtitle handlers use it as their fallback path.
func (e *Engine) EnsureProbe(ctx context.Context, file *models.MediaFile, drv source.Driver) (*Probe, error) {
	if drv == nil {
		var err error
		drv, err = e.DriverFor(file)
		if err != nil {
			return nil, err
		}
	}
	// ProbeFor answers from the files table for an ordinary file and from the
	// source's own metadata for one whose media is several streams, which no
	// single ffprobe pass could describe.
	if p, err := e.ProbeFor(ctx, file, drv); err != nil || p != nil {
		return p, err
	}

	job, leader := e.acquireProbeJob(file)
	if !leader {
		select {
		case <-job.done:
			return job.probe, job.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e.runProbeJob(ctx, file, drv, job)
	return job.probe, job.err
}

// ProbeTransient returns a probe for playback-ticket finalization without
// persisting files.probe or subtitles. Concurrent callers for the same file
// share one ffprobe pass, but never join the legacy probeJobs registry because
// that path commits shared metadata before the ticket can check revocation.
func (e *Engine) ProbeTransient(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
) (*Probe, error) {
	if e == nil || file == nil {
		return nil, errors.New("media probe unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if probe := ProbeFromCache(file); probe != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return probe, nil
	}
	key := ticketProbeJobKey(file)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		job, leader, err := e.acquireTicketProbeJob(ctx, key)
		if err != nil {
			return nil, err
		}
		if !leader {
			select {
			case <-job.done:
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				// Only a leader whose own context ended is retried. A driver
				// may return context.DeadlineExceeded or context.Canceled
				// while its caller is still live; that is an ordinary source
				// failure and stays cached for ticketProbeResultTTL.
				if job.leaderCanceled {
					continue
				}
				return job.probe, job.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		return e.runTicketProbeLeader(ctx, key, job, file, drv)
	}
}

func (e *Engine) runTicketProbeLeader(
	ctx context.Context,
	key ticketProbeKey,
	job *ticketProbeJob,
	file *models.MediaFile,
	drv source.Driver,
) (*Probe, error) {
	slots := e.ticketProbeSlotPool()
	if err := ctx.Err(); err != nil {
		e.finishTicketProbeJob(key, job, nil, err, true)
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		err := ctx.Err()
		e.finishTicketProbeJob(key, job, nil, err, true)
		return nil, err
	}
	// ProbeFor can call a source driver (MultiPart), so it belongs inside the
	// shared slot/registry critical section: repeated source failures then share
	// the same TTL and cannot bypass the ticket-probe concurrency bound.
	probe, err := e.ProbeFor(ctx, file, drv)
	if err == nil && probe == nil {
		probe, err = e.probeWithoutPersisting(ctx, file, drv)
	}
	if probe != nil && probe.Video() == nil && len(probe.Streams) > 0 {
		err = errNoVideoStream
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		e.finishTicketProbeJob(key, job, nil, ctxErr, true)
		return nil, ctxErr
	}
	leaderCanceled := false
	e.finishTicketProbeJob(key, job, probe, err, leaderCanceled)
	return probe, err
}

func ticketProbeJobKey(file *models.MediaFile) ticketProbeKey {
	return ticketProbeKey{fileID: file.ID, size: file.Size, mtime: file.MTime}
}

// acquireTicketProbeJob returns an existing job, or installs the caller as its
// leader. When the registry is full of in-flight jobs, it waits for capacity or
// the caller's cancellation instead of bypassing the single-flight registry.
func (e *Engine) acquireTicketProbeJob(
	ctx context.Context,
	key ticketProbeKey,
) (*ticketProbeJob, bool, error) {
	for {
		e.ticketProbeMu.Lock()
		if e.ticketProbeJobs == nil {
			e.ticketProbeJobs = map[ticketProbeKey]*ticketProbeJob{}
		}
		if e.ticketProbeWake == nil {
			e.ticketProbeWake = make(chan struct{})
		}
		if job, ok := e.ticketProbeJobs[key]; ok {
			if job.finished.IsZero() {
				e.ticketProbeMu.Unlock()
				return job, false, nil
			}
			if !job.leaderCanceled &&
				time.Since(job.finished) < ticketProbeResultTTL {
				e.ticketProbeMu.Unlock()
				return job, false, nil
			}
			delete(e.ticketProbeJobs, key)
		}
		e.gcTicketProbeJobsLocked()
		if len(e.ticketProbeJobs) < ticketProbeMaxJobs ||
			e.evictOldestFinishedTicketProbeJobLocked() {
			job := &ticketProbeJob{done: make(chan struct{})}
			e.ticketProbeJobs[key] = job
			e.ticketProbeMu.Unlock()
			return job, true, nil
		}
		wake := e.ticketProbeWake
		e.ticketProbeMu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

func (e *Engine) finishTicketProbeJob(
	key ticketProbeKey,
	job *ticketProbeJob,
	probe *Probe,
	err error,
	leaderCanceled bool,
) {
	e.ticketProbeMu.Lock()
	defer e.ticketProbeMu.Unlock()
	job.probe = probe
	job.err = err
	job.finished = time.Now()
	job.leaderCanceled = leaderCanceled
	if leaderCanceled {
		if current, ok := e.ticketProbeJobs[key]; ok && current == job {
			delete(e.ticketProbeJobs, key)
		}
	}
	close(job.done)
	e.notifyTicketProbeWaitersLocked()
}

func (e *Engine) gcTicketProbeJobsLocked() {
	now := time.Now()
	for id, job := range e.ticketProbeJobs {
		if !job.finished.IsZero() &&
			now.Sub(job.finished) >= ticketProbeResultTTL {
			delete(e.ticketProbeJobs, id)
		}
	}
}

func (e *Engine) evictOldestFinishedTicketProbeJobLocked() bool {
	var oldestKey ticketProbeKey
	var oldest time.Time
	found := false
	for key, job := range e.ticketProbeJobs {
		if job.finished.IsZero() {
			continue
		}
		if !found || job.finished.Before(oldest) ||
			job.finished.Equal(oldest) && ticketProbeKeyLess(key, oldestKey) {
			oldestKey, oldest, found = key, job.finished, true
		}
	}
	if found {
		delete(e.ticketProbeJobs, oldestKey)
	}
	return found
}

func ticketProbeKeyLess(a, b ticketProbeKey) bool {
	if a.fileID != b.fileID {
		return a.fileID < b.fileID
	}
	if a.size != b.size {
		return a.size < b.size
	}
	return a.mtime < b.mtime
}

func (e *Engine) notifyTicketProbeWaitersLocked() {
	if e.ticketProbeWake == nil {
		e.ticketProbeWake = make(chan struct{})
		return
	}
	close(e.ticketProbeWake)
	e.ticketProbeWake = make(chan struct{})
}

func (e *Engine) ticketProbeSlotPool() chan struct{} {
	e.ticketProbeMu.Lock()
	defer e.ticketProbeMu.Unlock()
	if e.ticketProbeSlots == nil {
		e.ticketProbeSlots = make(chan struct{}, ticketProbeMaxConcurrent)
	}
	return e.ticketProbeSlots
}

// probeWithoutPersisting resolves and probes a source without touching the
// store. The caller decides whether and how to commit the result.
func (e *Engine) probeWithoutPersisting(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
) (*Probe, error) {
	target, headers, err := e.ProbeTarget(ctx, file, drv)
	if err != nil {
		return nil, err
	}
	proxy, err := sourceProxyFor(drv)
	if err != nil {
		return nil, err
	}
	return e.probeWithFallback(ctx, target, headers, proxy)
}

// acquireProbeJob returns the in-flight job for a file, or creates one and
// reports that the caller is the leader responsible for running it.
func (e *Engine) acquireProbeJob(file *models.MediaFile) (*probeJob, bool) {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	e.gcProbeJobsLocked()
	if job, ok := e.probeJobs[file.ID]; ok && job.state == "probing" {
		return job, false
	}
	job := &probeJob{
		file:    file,
		done:    make(chan struct{}),
		state:   "probing",
		started: time.Now(),
	}
	e.probeJobs[file.ID] = job
	return job, true
}

// gcProbeJobsLocked drops finished jobs nobody is polling anymore.
func (e *Engine) gcProbeJobsLocked() {
	if len(e.probeJobs) < 512 {
		return
	}
	for id, job := range e.probeJobs {
		if job.state != "probing" && time.Since(job.finished) > 10*time.Minute {
			delete(e.probeJobs, id)
		}
	}
}

// runProbeJob performs the probe, persists the result and settles the job.
func (e *Engine) runProbeJob(ctx context.Context, file *models.MediaFile, drv source.Driver, job *probeJob) {
	defer close(job.done)

	err := e.doProbe(ctx, file, drv, job)

	e.probeMu.Lock()
	job.finished = time.Now()
	if err != nil {
		job.state, job.errMsg = "failed", safeProbeStatusError(err)
	} else {
		job.state = "ready"
	}
	e.probeMu.Unlock()

	if err != nil {
		log.Printf("probe failed for file %d (%s)", file.ID, safeMediaLogError(err))
	}
}

// doProbe resolves the source, probes it and stores the result on the file row.
func (e *Engine) doProbe(ctx context.Context, file *models.MediaFile, drv source.Driver, job *probeJob) error {
	probe, err := e.probeWithoutPersisting(ctx, file, drv)
	if err != nil {
		job.err = err
		return err
	}
	raw, err := jsonMarshal(probe)
	if err != nil {
		job.err = err
		return err
	}
	err = e.Store.UpdateFileProbeAndEmbeddedSubtitlesForVersionContext(
		ctx, file.ID, file.Size, file.MTime, string(raw),
		EmbeddedSubtitleRows(file.ID, probe))
	switch {
	case errors.Is(err, db.ErrMediaFileChanged):
		job.err = err
		return err
	case err != nil:
		log.Printf("persist probe for file %d failed (%s)",
			file.ID, safeMediaLogError(err))
	}
	file.Probe = string(raw)

	// Streams were found but none of them is video (an audio-only or still-file
	// source sitting behind a video extension). Report it instead of leaving the
	// caller to re-probe forever: ProbeFromCache() rejects this probe precisely
	// because there is nothing to play.
	if probe.Video() == nil && len(probe.Streams) > 0 {
		err := errNoVideoStream
		job.err = err
		return err
	}
	job.probe = probe
	return nil
}

// probeWithFallback probes with the configured (small) window and retries with
// the deep window when that found nothing usable — some muxes hide streams past
// a short probe.
func (e *Engine) probeWithFallback(ctx context.Context, target string, headers map[string]string, proxy string) (*Probe, error) {
	quick := *e.currentProber()
	probe, err := e.execProbe(ctx, &quick, target, headers, proxy)
	if err == nil && (probe.Video() != nil || len(probe.Streams) > 0) {
		return probe, nil
	}
	if ctx.Err() != nil {
		if err != nil {
			return nil, err
		}
		return nil, ctx.Err()
	}

	deep := *e.currentProber()
	deep.ProbeSizeMB, deep.AnalyzeSec = deepProbeSizeMB, deepProbeAnalyzeSec
	deepProbe, deepErr := e.execProbe(ctx, &deep, target, headers, proxy)
	if deepErr == nil && (deepProbe.Video() != nil || len(deepProbe.Streams) > 0) {
		return deepProbe, nil
	}
	if err != nil {
		return nil, err
	}
	// Shallow probe succeeded but found no video: an audio-only or still-file
	// source is a legitimate answer, so keep it rather than failing.
	if deepErr == nil {
		return probe, nil
	}
	return probe, nil
}

// ---------- background pre-probe (warms the cache after a scan) ----------

// PreProbeStatus summarizes the background pre-probe queue.
type PreProbeStatus struct {
	Queued  int    `json:"queued"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Current string `json:"current,omitempty"`
	// LastError is the reason the most recent file failed, for diagnostics.
	LastError string `json:"last_error,omitempty"`
	Running   bool   `json:"running"`
}

type preProbeQueue struct {
	once      sync.Once
	ch        chan *models.MediaFile
	mu        sync.Mutex
	queued    map[int64]bool
	pending   int
	done      int
	failed    int
	current   string
	lastError string
	active    int
	workers   int
}

// PreProbeFiles enqueues files for background probing. It returns immediately;
// duplicates and files that already carry a probe are skipped.
func (e *Engine) PreProbeFiles(files []*models.MediaFile) {
	if len(files) == 0 {
		return
	}
	workers := 1
	if e.Cfg != nil && e.Cfg.Playback.PreProbeConcurrency > 0 {
		workers = e.Cfg.Playback.PreProbeConcurrency
	}
	e.pre.once.Do(func() {
		e.pre.workers = workers
		e.pre.ch = make(chan *models.MediaFile, 4096)
		e.pre.queued = map[int64]bool{}
		for i := 0; i < workers; i++ {
			go e.preProbeWorker()
		}
	})

	added := 0
	e.pre.mu.Lock()
	for _, f := range files {
		if f == nil || ProbeFromCache(f) != nil || e.pre.queued[f.ID] {
			continue
		}
		e.pre.queued[f.ID] = true
		e.pre.pending++
		added++
		select {
		case e.pre.ch <- f:
		default: // queue full: stop adding, the rest can wait for the next scan
			delete(e.pre.queued, f.ID)
			e.pre.pending--
			added--
		}
	}
	e.pre.mu.Unlock()
	if added > 0 {
		log.Printf("pre-probe: queued %d file(s) for background probing (%d worker(s))", added, workers)
	}
}

// PreProbeStatus reports queue progress (for the settings/system UI).
func (e *Engine) PreProbeStatus() PreProbeStatus {
	e.pre.mu.Lock()
	defer e.pre.mu.Unlock()
	return PreProbeStatus{
		Queued:    e.pre.pending,
		Done:      e.pre.done,
		Failed:    e.pre.failed,
		Current:   e.pre.current,
		LastError: e.pre.lastError,
		Running:   e.pre.active > 0 || e.pre.pending > 0,
	}
}

func (e *Engine) preProbeWorker() {
	for file := range e.pre.ch {
		e.pre.mu.Lock()
		e.pre.active++
		e.pre.current = file.Path
		e.pre.mu.Unlock()

		err := e.preProbeOne(file)

		e.pre.mu.Lock()
		e.pre.active--
		e.pre.current = ""
		e.pre.pending--
		delete(e.pre.queued, file.ID)
		if err != nil {
			e.pre.failed++
			e.pre.lastError = err.Error()
		} else {
			e.pre.done++
		}
		e.pre.mu.Unlock()

		// Be gentle with cloud drives: probing is background work and must not
		// compete with playback for bandwidth.
		time.Sleep(400 * time.Millisecond)
	}
}

func (e *Engine) preProbeOne(file *models.MediaFile) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	drv, err := e.DriverFor(file)
	if err != nil {
		return err
	}
	// Refresh the row: a scan may have updated it since it was enqueued.
	if fresh, err := e.Store.FileByID(file.ID); err == nil && fresh.Probe != "" {
		return nil
	}
	_, err = e.EnsureProbe(ctx, file, drv)
	return err
}
