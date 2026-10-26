package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

type playbackConcurrencyGrants struct {
	ticket   *models.PlaybackTicket
	media    *models.PlaybackMediaSessionGrant
	subtitle *models.PlaybackSubtitleSessionGrant
}

type playbackConcurrencyResult struct {
	version         int64
	state           string
	lastUsedAt      *time.Time
	idleExpiresAt   time.Time
	absoluteExpiry  time.Time
	err             error
	operation       string
	expectedUsedAt  time.Time
	expectedIdleCap time.Time
}

func openPlaybackConcurrencyStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "playback-concurrency.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open first file-backed playback store: %v", err)
	}
	t.Cleanup(func() { _ = first.DB.Close() })

	second, err := Open(path)
	if err != nil {
		t.Fatalf("open second file-backed playback store: %v", err)
	}
	t.Cleanup(func() { _ = second.DB.Close() })

	for _, store := range []*Store{first, second} {
		store.DB.SetMaxOpenConns(8)
		if store.DB.Stats().MaxOpenConnections < 2 {
			t.Fatal("playback concurrency test requires multiple SQLite connections")
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file-backed playback database is missing: %v", err)
	}
	var journalMode string
	if err := first.DB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read playback journal mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("playback journal mode = %q, want WAL", journalMode)
	}
	return first, second
}

func createPlaybackConcurrencyGrants(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	now time.Time,
) playbackConcurrencyGrants {
	t.Helper()
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/playback-concurrency/caption.srt", "subrip", false, 0)
	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerProxy},
		MediaSummary:        []byte(`{}`),
	}, now)
	if err != nil {
		t.Fatalf("create concurrency ticket: %v", err)
	}
	ready, err := store.FinishPlaybackTicketProbe(
		fixture.bindingWith(ticket.TicketID),
		ticket.Version,
		models.PlaybackTicketProbeResult{
			State:               models.PlaybackTicketReady,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        []byte(`{}`),
			SubtitleIDs:         []int64{subtitleID},
			VideoIndex:          playbackVideoIndexPtr(0),
		},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("finish concurrency probe: %v", err)
	}
	claimed, err := store.ClaimNextPlaybackTransport(
		context.Background(),
		fixture.bindingWith(ticket.TicketID),
		ready.Version,
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("claim concurrency transport: %v", err)
	}
	media, err := store.CreatePlaybackMediaSession(
		context.Background(),
		models.PlaybackMediaSessionRequest{
			Ticket:       fixture.bindingWith(claimed.TicketID),
			Transport:    models.PlaybackTransportServerProxy,
			Mode:         "remux",
			AudioOrdinal: 0,
			StartSeconds: 0,
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatalf("create concurrency media session: %v", err)
	}
	subtitle, err := store.CreatePlaybackSubtitleSession(models.PlaybackSubtitleSessionRequest{
		Ticket:       playbackSubtitleTicketBinding(claimed),
		Transport:    models.PlaybackTransportServerProxy,
		SubtitleID:   subtitleID,
		StartSeconds: 0,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("create concurrency subtitle session: %v", err)
	}
	return playbackConcurrencyGrants{
		ticket: claimed, media: media, subtitle: subtitle,
	}
}

func assertPlaybackVersionsMonotonic(
	t *testing.T,
	results []playbackConcurrencyResult,
	startVersion int64,
) {
	t.Helper()
	versions := make([]int64, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("%s returned an error: %v (busy=%v)",
				result.operation, result.err, isSQLiteBusy(result.err))
		}
		versions = append(versions, result.version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for i, version := range versions {
		want := startVersion + int64(i) + 1
		if version != want {
			t.Fatalf("version sequence at index %d = %d, want %d (all=%v)",
				i, version, want, versions)
		}
	}
}

func assertPlaybackConcurrencyTTL(
	t *testing.T,
	results []playbackConcurrencyResult,
	absoluteExpiry time.Time,
	expectedIdleCap time.Time,
) {
	t.Helper()
	for _, result := range results {
		if result.state != models.PlaybackMediaSessionActive &&
			result.state != models.PlaybackSubtitleSessionActive {
			t.Errorf("%s returned state %q", result.operation, result.state)
		}
		if !result.absoluteExpiry.Equal(absoluteExpiry) {
			t.Errorf("%s absolute expiry = %v, want %v",
				result.operation, result.absoluteExpiry, absoluteExpiry)
		}
		if result.idleExpiresAt.After(absoluteExpiry) {
			t.Errorf("%s idle expiry %v exceeds absolute %v",
				result.operation, result.idleExpiresAt, absoluteExpiry)
		}
		if !result.idleExpiresAt.Equal(expectedIdleCap) {
			t.Errorf("%s idle expiry = %v, want %v",
				result.operation, result.idleExpiresAt, expectedIdleCap)
		}
		if result.lastUsedAt == nil || !result.lastUsedAt.Equal(result.expectedUsedAt) {
			t.Errorf("%s last_used_at = %v, want %v",
				result.operation, result.lastUsedAt, result.expectedUsedAt)
		}
	}
}

func TestPlaybackTicketCreateConcurrentFileSQLite(t *testing.T) {
	first, second := openPlaybackConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, first, "concurrent-create", "concurrent-create", now)

	const calls = 32
	start := make(chan struct{})
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := stores[i%len(stores)].CreatePlaybackTicket(
				models.PlaybackTicketRequest{
					UserID: fixture.user.ID, DeviceID: fixture.device.ID,
					FileID:              fixture.fileID,
					CandidateTransports: []string{models.PlaybackTransportServerProxy},
				},
				now,
			)
			if err != nil {
				errs <- fmt.Errorf("create ticket call %d: %w", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ticket create error: %v (busy=%v)", err, isSQLiteBusy(err))
	}

	var count int
	if err := first.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
		WHERE user_id = ? AND device_id = ? AND file_id = ? AND state = 'probing'
			AND version = 1`,
		fixture.user.ID, fixture.device.ID, fixture.fileID).Scan(&count); err != nil {
		t.Fatalf("count concurrently created tickets: %v", err)
	}
	if count != calls {
		t.Fatalf("concurrently created tickets = %d, want %d", count, calls)
	}
}

func TestPlaybackMediaSessionConcurrentAuthorizeTouchFileSQLite(t *testing.T) {
	first, second := openPlaybackConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, first, "concurrent-media", "concurrent-media", now)
	grants := createPlaybackConcurrencyGrants(t, first, fixture, now)
	binding := fixture.sessionBinding(
		grants.ticket.TicketID, models.PlaybackTransportServerProxy)

	const authorizeCalls = 24
	const touchCalls = 24
	totalCalls := authorizeCalls + touchCalls
	start := make(chan struct{})
	resultsCh := make(chan playbackConcurrencyResult, totalCalls)
	var wg sync.WaitGroup
	for i := 0; i < totalCalls; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := stores[i%len(stores)]
			at := now.Add(time.Minute)
			if i < authorizeCalls {
				access, err := store.AuthorizePlaybackMediaSession(
					grants.media.Capability, false, at)
				result := playbackConcurrencyResult{
					operation:       fmt.Sprintf("authorize media call %d", i),
					expectedUsedAt:  at,
					expectedIdleCap: at.Add(models.PlaybackMediaSessionIdleTTL),
				}
				if err != nil {
					result.err = err
				} else {
					result.version = access.Session.Version
					result.state = access.Session.State
					result.lastUsedAt = access.Session.LastUsedAt
					result.idleExpiresAt = access.Session.IdleExpiresAt
					result.absoluteExpiry = access.Session.AbsoluteExpiresAt
				}
				resultsCh <- result
				return
			}
			session, err := store.TouchPlaybackMediaSession(binding, grants.media.Capability, at)
			result := playbackConcurrencyResult{
				operation:       fmt.Sprintf("touch media call %d", i),
				expectedUsedAt:  at,
				expectedIdleCap: at.Add(models.PlaybackMediaSessionIdleTTL),
			}
			if err != nil {
				result.err = err
			} else {
				result.version = session.Version
				result.state = session.State
				result.lastUsedAt = session.LastUsedAt
				result.idleExpiresAt = session.IdleExpiresAt
				result.absoluteExpiry = session.AbsoluteExpiresAt
			}
			resultsCh <- result
		}()
	}
	close(start)
	wg.Wait()
	close(resultsCh)

	results := make([]playbackConcurrencyResult, 0, totalCalls)
	for result := range resultsCh {
		results = append(results, result)
	}
	assertPlaybackVersionsMonotonic(t, results, grants.media.Session.Version)
	assertPlaybackConcurrencyTTL(
		t, results, grants.media.Session.AbsoluteExpiresAt,
		now.Add(time.Minute).Add(models.PlaybackMediaSessionIdleTTL))

	var version int64
	var state string
	if err := first.DB.QueryRow(`SELECT version, state FROM playback_media_sessions
		WHERE id = ?`, grants.media.Session.ID).Scan(&version, &state); err != nil {
		t.Fatalf("read media session after concurrent touches: %v", err)
	}
	if want := grants.media.Session.Version + int64(totalCalls); version != want ||
		state != models.PlaybackMediaSessionActive {
		t.Fatalf("media session after concurrency = version %d state %q, want %d active",
			version, state, want)
	}

	if _, err := first.DB.Exec(`UPDATE playback_media_sessions
		SET idle_expires_at = ?, version = version + 1 WHERE id = ?`,
		grants.media.Session.AbsoluteExpiresAt, grants.media.Session.ID); err != nil {
		t.Fatalf("place media session near absolute expiry: %v", err)
	}
	atAbsoluteEdge := grants.media.Session.AbsoluteExpiresAt.Add(-time.Second)
	edgeResults := make(chan playbackConcurrencyResult, touchCalls)
	start = make(chan struct{})
	for i := 0; i < touchCalls; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := stores[i%len(stores)]
			session, err := store.TouchPlaybackMediaSession(
				binding, grants.media.Capability, atAbsoluteEdge)
			result := playbackConcurrencyResult{
				operation:       fmt.Sprintf("edge touch media call %d", i),
				expectedUsedAt:  atAbsoluteEdge,
				expectedIdleCap: grants.media.Session.AbsoluteExpiresAt,
			}
			if err != nil {
				result.err = err
			} else {
				result.version = session.Version
				result.state = session.State
				result.lastUsedAt = session.LastUsedAt
				result.idleExpiresAt = session.IdleExpiresAt
				result.absoluteExpiry = session.AbsoluteExpiresAt
			}
			edgeResults <- result
		}()
	}
	close(start)
	wg.Wait()
	close(edgeResults)

	edge := make([]playbackConcurrencyResult, 0, touchCalls)
	for result := range edgeResults {
		edge = append(edge, result)
	}
	assertPlaybackVersionsMonotonic(
		t, edge, grants.media.Session.Version+int64(totalCalls)+1)
	assertPlaybackConcurrencyTTL(
		t, edge, grants.media.Session.AbsoluteExpiresAt,
		grants.media.Session.AbsoluteExpiresAt)
	if _, err := first.TouchPlaybackMediaSession(
		binding, grants.media.Capability, grants.media.Session.AbsoluteExpiresAt); !errors.Is(
		err, ErrPlaybackMediaSessionExpired) {
		t.Fatalf("touch media at absolute expiry error = %v", err)
	}
}

func TestPlaybackSubtitleSessionConcurrentAuthorizeFileSQLite(t *testing.T) {
	first, second := openPlaybackConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, first, "concurrent-subtitle", "concurrent-subtitle", now)
	grants := createPlaybackConcurrencyGrants(t, first, fixture, now)

	const authorizeCalls = 32
	start := make(chan struct{})
	resultsCh := make(chan playbackConcurrencyResult, authorizeCalls)
	var wg sync.WaitGroup
	for i := 0; i < authorizeCalls; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			at := now.Add(time.Minute)
			access, err := stores[i%len(stores)].AuthorizePlaybackSubtitleSession(
				grants.subtitle.Capability, false, at)
			result := playbackConcurrencyResult{
				operation:       fmt.Sprintf("authorize subtitle call %d", i),
				expectedUsedAt:  at,
				expectedIdleCap: at.Add(models.PlaybackSubtitleSessionIdleTTL),
			}
			if err != nil {
				result.err = err
			} else {
				result.version = access.Session.Version
				result.state = access.Session.State
				result.lastUsedAt = access.Session.LastUsedAt
				result.idleExpiresAt = access.Session.IdleExpiresAt
				result.absoluteExpiry = access.Session.AbsoluteExpiresAt
			}
			resultsCh <- result
		}()
	}
	close(start)
	wg.Wait()
	close(resultsCh)

	results := make([]playbackConcurrencyResult, 0, authorizeCalls)
	for result := range resultsCh {
		results = append(results, result)
	}
	assertPlaybackVersionsMonotonic(t, results, grants.subtitle.Session.Version)
	assertPlaybackConcurrencyTTL(
		t, results, grants.subtitle.Session.AbsoluteExpiresAt,
		now.Add(time.Minute).Add(models.PlaybackSubtitleSessionIdleTTL))

	if _, err := first.DB.Exec(`UPDATE playback_subtitle_sessions
		SET idle_expires_at = ?, version = version + 1 WHERE id = ?`,
		grants.subtitle.Session.AbsoluteExpiresAt, grants.subtitle.Session.ID); err != nil {
		t.Fatalf("place subtitle session near absolute expiry: %v", err)
	}
	atAbsoluteEdge := grants.subtitle.Session.AbsoluteExpiresAt.Add(-time.Second)
	const edgeCalls = 24
	start = make(chan struct{})
	edgeResultsCh := make(chan playbackConcurrencyResult, edgeCalls)
	for i := 0; i < edgeCalls; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			access, err := stores[i%len(stores)].AuthorizePlaybackSubtitleSession(
				grants.subtitle.Capability, false, atAbsoluteEdge)
			result := playbackConcurrencyResult{
				operation:       fmt.Sprintf("edge authorize subtitle call %d", i),
				expectedUsedAt:  atAbsoluteEdge,
				expectedIdleCap: grants.subtitle.Session.AbsoluteExpiresAt,
			}
			if err != nil {
				result.err = err
			} else {
				result.version = access.Session.Version
				result.state = access.Session.State
				result.lastUsedAt = access.Session.LastUsedAt
				result.idleExpiresAt = access.Session.IdleExpiresAt
				result.absoluteExpiry = access.Session.AbsoluteExpiresAt
			}
			edgeResultsCh <- result
		}()
	}
	close(start)
	wg.Wait()
	close(edgeResultsCh)

	edge := make([]playbackConcurrencyResult, 0, edgeCalls)
	for result := range edgeResultsCh {
		edge = append(edge, result)
	}
	assertPlaybackVersionsMonotonic(
		t, edge, grants.subtitle.Session.Version+int64(authorizeCalls)+1)
	assertPlaybackConcurrencyTTL(
		t, edge, grants.subtitle.Session.AbsoluteExpiresAt,
		grants.subtitle.Session.AbsoluteExpiresAt)
	if _, err := first.AuthorizePlaybackSubtitleSession(
		grants.subtitle.Capability, false,
		grants.subtitle.Session.AbsoluteExpiresAt); !errors.Is(
		err, ErrPlaybackSubtitleSessionExpired) {
		t.Fatalf("authorize subtitle at absolute expiry error = %v", err)
	}
}

type playbackAuthorizationWorkers struct {
	start   chan struct{}
	started chan struct{}
	stop    chan struct{}
	errs    chan error
	wg      sync.WaitGroup
	once    sync.Once
}

func startPlaybackAuthorizationWorkers(
	stores []*Store,
	mediaCapability, subtitleCapability string,
	workerCount int,
) *playbackAuthorizationWorkers {
	workers := &playbackAuthorizationWorkers{
		start:   make(chan struct{}),
		started: make(chan struct{}, workerCount),
		stop:    make(chan struct{}),
		errs:    make(chan error, workerCount),
	}
	for i := 0; i < workerCount; i++ {
		i := i
		workers.wg.Add(1)
		go func() {
			defer workers.wg.Done()
			<-workers.start
			workers.started <- struct{}{}
			store := stores[i%len(stores)]
			for {
				select {
				case <-workers.stop:
					return
				default:
				}
				if _, err := store.AuthorizePlaybackMediaSession(
					mediaCapability, false, time.Now().UTC()); err != nil {
					if errors.Is(err, ErrPlaybackMediaSessionRevoked) {
						return
					}
					workers.errs <- fmt.Errorf("authorize media worker %d: %w", i, err)
					return
				}
				if _, err := store.AuthorizePlaybackSubtitleSession(
					subtitleCapability, false, time.Now().UTC()); err != nil {
					if errors.Is(err, ErrPlaybackSubtitleSessionRevoked) {
						return
					}
					workers.errs <- fmt.Errorf("authorize subtitle worker %d: %w", i, err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	return workers
}

func (w *playbackAuthorizationWorkers) begin(t *testing.T) {
	t.Helper()
	close(w.start)
	for i := 0; i < cap(w.started); i++ {
		select {
		case <-w.started:
		case <-time.After(5 * time.Second):
			t.Fatal("playback authorization workers did not start")
		}
	}
}

func (w *playbackAuthorizationWorkers) finish(t *testing.T) {
	t.Helper()
	w.once.Do(func() { close(w.stop) })
	w.wg.Wait()
	close(w.errs)
	for err := range w.errs {
		t.Errorf("concurrent authorization error: %v (busy=%v)", err, isSQLiteBusy(err))
	}
}

func assertNoActivePlaybackSessions(
	t *testing.T,
	store *Store,
	fixture playbackTestFixture,
	grants playbackConcurrencyGrants,
) {
	t.Helper()
	for _, tc := range []struct {
		table string
		where string
		args  []any
	}{
		{
			table: "playback_media_sessions",
			where: `ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
			args: []any{
				grants.ticket.TicketID, fixture.user.ID, fixture.device.ID, fixture.fileID,
			},
		},
		{
			table: "playback_subtitle_sessions",
			where: `ticket_id = ? AND user_id = ? AND device_id = ? AND file_id = ?`,
			args: []any{
				grants.ticket.TicketID, fixture.user.ID, fixture.device.ID, fixture.fileID,
			},
		},
	} {
		var active int
		query := `SELECT COUNT(*) FROM ` + tc.table +
			` WHERE ` + tc.where + ` AND state = 'active'`
		if err := store.DB.QueryRow(query, tc.args...).Scan(&active); err != nil {
			t.Fatalf("count active %s: %v", tc.table, err)
		}
		if active != 0 {
			t.Errorf("%s still has %d active rows", tc.table, active)
		}
	}
	if _, err := store.AuthorizePlaybackMediaSession(
		grants.media.Capability, false, time.Now().UTC().Add(time.Minute)); !errors.Is(
		err, ErrPlaybackMediaSessionRevoked) {
		t.Errorf("media session still authorizes after revocation: %v", err)
	}
	if _, err := store.AuthorizePlaybackSubtitleSession(
		grants.subtitle.Capability, false, time.Now().UTC().Add(time.Minute)); !errors.Is(
		err, ErrPlaybackSubtitleSessionRevoked) {
		t.Errorf("subtitle session still authorizes after revocation: %v", err)
	}
}

func TestPlaybackTicketRevocationSerializesWithAuthorization(t *testing.T) {
	first, second := openPlaybackConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Now().UTC()
	fixture := addPlaybackTestFixture(t, first, "concurrent-ticket-revoke", "concurrent-ticket-revoke", now)
	grants := createPlaybackConcurrencyGrants(t, first, fixture, now)
	workers := startPlaybackAuthorizationWorkers(
		stores, grants.media.Capability, grants.subtitle.Capability, 12)
	workers.begin(t)
	time.Sleep(10 * time.Millisecond)

	if _, err := first.RevokePlaybackTicket(
		fixture.bindingWith(grants.ticket.TicketID),
		grants.ticket.Version,
		now.Add(time.Minute),
	); err != nil {
		workers.finish(t)
		t.Fatalf("revoke playback ticket during authorization: %v (busy=%v)",
			err, isSQLiteBusy(err))
	}
	workers.finish(t)
	assertNoActivePlaybackSessions(t, first, fixture, grants)
}

func TestPlaybackDeviceRevocationSerializesWithAuthorization(t *testing.T) {
	first, second := openPlaybackConcurrencyStores(t)
	stores := []*Store{first, second}
	now := time.Now().UTC()
	fixture := addPlaybackTestFixture(t, first, "concurrent-device-revoke", "concurrent-device-revoke", now)
	grants := createPlaybackConcurrencyGrants(t, first, fixture, now)
	workers := startPlaybackAuthorizationWorkers(
		stores, grants.media.Capability, grants.subtitle.Capability, 12)
	workers.begin(t)
	time.Sleep(10 * time.Millisecond)

	revoked, err := first.RevokeAllDevicesForUser(fixture.user.ID, now.Add(time.Minute))
	if err != nil {
		workers.finish(t)
		t.Fatalf("revoke playback devices during authorization: %v (busy=%v)",
			err, isSQLiteBusy(err))
	}
	if revoked != 1 {
		workers.finish(t)
		t.Fatalf("revoked devices = %d, want 1", revoked)
	}
	workers.finish(t)
	assertNoActivePlaybackSessions(t, first, fixture, grants)
}
