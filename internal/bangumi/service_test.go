package bangumi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/metadata"
	"doriyamedia/internal/models"
)

func TestConnectAndSyncWatchedEpisode(t *testing.T) {
	var mu sync.Mutex
	collectionCreated := false
	var watched []int

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/episodes" && r.Header.Get("Authorization") != "Bearer personal-token" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/me":
			writeTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
				"avatar": map[string]string{"large": "https://example.test/a.jpg"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/users/alice/collections/42":
			mu.Lock()
			created := collectionCreated
			mu.Unlock()
			if !created {
				http.NotFound(w, r)
				return
			}
			writeTestJSON(w, map[string]any{"subject_id": 42, "type": 3})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/users/-/collections/42":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["type"] != float64(3) {
				t.Errorf("collection type = %v, want 3", body["type"])
			}
			mu.Lock()
			collectionCreated = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/episodes":
			if r.URL.Query().Get("subject_id") != "42" {
				t.Errorf("subject_id = %q, want 42", r.URL.Query().Get("subject_id"))
			}
			writeTestJSON(w, map[string]any{"data": []map[string]any{{
				"id": 101, "type": 0, "ep": 1, "sort": 1,
				"name": "第一话", "name_cn": "第一话", "airdate": "2026-01-01",
			}}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/users/-/collections/42/episodes":
			var body struct {
				EpisodeIDs []int `json:"episode_id"`
				Type       int   `json:"type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode episode body: %v", err)
			}
			watched = body.EpisodeIDs
			if body.Type != 2 {
				t.Errorf("episode collection type = %d, want 2", body.Type)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()

	user, episode, fileID := seedBangumiAnime(t, store)
	cfg := &config.Config{
		Secret: "test-secret",
		BGM: config.BGMTV{
			APIBase:  api.URL,
			UseProxy: false,
		},
	}
	svc := NewService(store, metadata.NewManager(store, cfg), cfg)

	st, err := svc.Connect(t.Context(), user.ID, "personal-token")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !st.Connected || st.Username != "alice" || st.Nickname != "Alice" {
		t.Fatalf("Connect status = %+v", st)
	}
	account, err := store.BangumiAccountByUser(user.ID)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	if account.TokenEnc == "personal-token" {
		t.Fatal("token was stored in plaintext")
	}
	if got, err := decryptToken(cfg.Secret, user.ID, account.TokenEnc); err != nil || got != "personal-token" {
		t.Fatalf("stored token round trip = %q, %v", got, err)
	}
	if _, err := decryptToken(cfg.Secret, user.ID+1, account.TokenEnc); err == nil {
		t.Fatal("token decrypted under a different local user ID")
	}

	if err := svc.EnqueueWatched(user.ID, fileID); err != nil {
		t.Fatalf("EnqueueWatched: %v", err)
	}
	if err := svc.EnqueueWatched(user.ID, fileID); err != nil {
		t.Fatalf("EnqueueWatched duplicate: %v", err)
	}
	pending, failed, synced, err := store.BangumiSyncStats(user.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if pending != 1 || failed != 0 || synced != 0 {
		t.Fatalf("queue stats after duplicate = pending %d failed %d synced %d", pending, failed, synced)
	}

	more, err := svc.processOne(t.Context())
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !more {
		t.Fatal("processOne reported no job")
	}
	if len(watched) != 1 || watched[0] != 101 {
		t.Fatalf("watched episode IDs = %v, want [101]", watched)
	}
	pending, failed, synced, _ = store.BangumiSyncStats(user.ID)
	if pending != 0 || failed != 0 || synced != 1 {
		t.Fatalf("queue stats after sync = pending %d failed %d synced %d", pending, failed, synced)
	}
	reloaded, err := store.BangumiAccountByUser(user.ID)
	if err != nil || reloaded.LastSyncAt == nil {
		t.Fatalf("account last sync = %v, err %v", reloaded.LastSyncAt, err)
	}

	// A second completed local file is picked up by the explicit backfill, which
	// also exercises the queue's local-episode idempotency.
	extra := &models.MediaFile{
		ItemID: episode.ID, SourceID: 99, Path: "/anime/episode-01-copy.mkv",
	}
	extraID, err := store.UpsertFile(extra)
	if err != nil {
		t.Fatalf("seed extra file: %v", err)
	}
	// The remote write is keyed by Bangumi episode, not local file. A duplicate
	// file for the same episode must be a no-op.
	if err := svc.EnqueueWatched(user.ID, extraID); err != nil {
		t.Fatalf("EnqueueWatched duplicate episode file: %v", err)
	}
	if err := store.SaveProgress(&models.Progress{
		UserID: user.ID, FileID: extraID, ItemID: episode.ID,
		Position: 96, Duration: 100,
	}); err != nil {
		t.Fatalf("save progress: %v", err)
	}
	n, err := svc.Backfill(user.ID)
	if err != nil || n != 0 {
		t.Fatalf("Backfill = %d, %v; want 0 new jobs (the episode is already done)", n, err)
	}
	pending, _, _, _ = store.BangumiSyncStats(user.ID)
	if pending != 0 {
		t.Fatalf("pending after backfill = %d, want 0", pending)
	}

	// A second copy of the same episode maps to the same Bangumi episode, so it
	// must not create a second remote write. A later progress save after a
	// permanent failure must not revive that failed job either.
	if err := store.EnqueueBangumiSync(user.ID, extraID, 42, 999999, time.Now()); err != nil {
		t.Fatalf("enqueue invalid target: %v", err)
	}
	badJob, err := store.ClaimBangumiSyncJob(time.Now().Add(time.Second))
	if err != nil || badJob == nil {
		t.Fatalf("claim invalid target: job %+v err %v", badJob, err)
	}
	badJob.Status = "failed"
	badJob.LastError = "permanent"
	badJob.UpdatedAt = time.Now()
	if err := store.UpdateBangumiSyncJob(badJob); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := svc.EnqueueWatched(user.ID, extraID); err != nil {
		t.Fatalf("EnqueueWatched after failure: %v", err)
	}
	_, failed, _, _ = store.BangumiSyncStats(user.ID)
	if failed != 1 {
		t.Fatalf("failed jobs after progress save = %d, want 1", failed)
	}
	if _, err := store.RetryBangumiSyncJobs(user.ID, time.Now()); err != nil {
		t.Fatalf("retry failed jobs: %v", err)
	}
	_, failed, _, _ = store.BangumiSyncStats(user.ID)
	if failed != 0 {
		t.Fatalf("failed jobs after explicit retry = %d, want 0", failed)
	}

	// Pausing sync postpones the remaining job; re-enabling it must make that
	// job immediately eligible instead of leaving it parked for six hours.
	if err := svc.SetSyncEnabled(user.ID, false); err != nil {
		t.Fatalf("disable sync: %v", err)
	}
	pendingJob, err := store.ClaimBangumiSyncJob(time.Now().Add(time.Second))
	if err != nil || pendingJob == nil {
		t.Fatalf("claim before pause: job %+v err %v", pendingJob, err)
	}
	pendingJob.Status = "pending"
	pendingJob.NextAttemptAt = time.Now().Add(6 * time.Hour)
	pendingJob.UpdatedAt = time.Now()
	if err := store.UpdateBangumiSyncJob(pendingJob); err != nil {
		t.Fatalf("park job: %v", err)
	}
	if err := svc.SetSyncEnabled(user.ID, true); err != nil {
		t.Fatalf("enable sync: %v", err)
	}
	resumed, err := store.ClaimBangumiSyncJob(time.Now().Add(time.Second))
	if err != nil || resumed == nil || resumed.ID != pendingJob.ID {
		t.Fatalf("claim after resume: job %+v err %v, want id %d", resumed, err, pendingJob.ID)
	}
}

func seedBangumiAnime(t *testing.T, store *db.Store) (*models.User, *models.Item, int64) {
	t.Helper()
	user := &models.User{Name: "alice", PassHash: "hash", Salt: "salt"}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	src := &models.Source{Name: "local", Type: "local", Config: "{}"}
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	lib := &models.Library{Name: "anime", Type: "series", SourceID: src.ID, Path: "/", Providers: "[]"}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	show := &models.Item{LibraryID: lib.ID, Type: "show", Name: "Test Anime", BGMID: 42}
	if err := store.InsertItem(show); err != nil {
		t.Fatalf("insert show: %v", err)
	}
	season := &models.Item{LibraryID: lib.ID, Type: "season", Name: "Season 1", ParentID: &show.ID, Season: 1}
	if err := store.InsertItem(season); err != nil {
		t.Fatalf("insert season: %v", err)
	}
	episode := &models.Item{
		LibraryID: lib.ID, Type: "episode", Name: "Episode 1",
		ParentID: &season.ID, Season: 1, Episode: 1,
	}
	if err := store.InsertItem(episode); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	fileID, err := store.UpsertFile(&models.MediaFile{
		ItemID: episode.ID, SourceID: src.ID, Path: "/anime/episode-01.mkv",
	})
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
	return user, episode, fileID
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestClientKeepsAPIStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewClient("bad", srv.URL, srv.Client()).Me(t.Context())
	if err == nil {
		t.Fatal("Me succeeded with a 401")
	}
	ae, ok := err.(*APIError)
	if !ok || ae.Status != http.StatusUnauthorized {
		t.Fatalf("error = %#v, want APIError 401", err)
	}
}

func TestCredentialReplacementFencesInFlightJob(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/me" {
			writeTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()

	user, _, fileID := seedBangumiAnime(t, store)
	cfg := &config.Config{
		Secret: "test-secret",
		BGM:    config.BGMTV{APIBase: api.URL},
	}
	svc := NewService(store, metadata.NewManager(store, cfg), cfg)
	if _, err := svc.Connect(t.Context(), user.ID, "first-token"); err != nil {
		t.Fatalf("connect first token: %v", err)
	}
	if err := svc.EnqueueWatched(user.ID, fileID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	oldJob, err := store.ClaimBangumiSyncJob(time.Now().Add(time.Second))
	if err != nil || oldJob == nil {
		t.Fatalf("claim old job: job %+v err %v", oldJob, err)
	}
	if oldJob.AccountGen != 1 {
		t.Fatalf("old job generation = %d, want 1", oldJob.AccountGen)
	}

	if _, err := svc.Connect(t.Context(), user.ID, "second-token"); err != nil {
		t.Fatalf("connect replacement token: %v", err)
	}
	// Replacing the token must not leave the old claim visible to the queue.
	// It becomes a fresh pending row under the new generation.
	pending, failed, _, err := store.BangumiSyncStats(user.ID)
	if err != nil {
		t.Fatalf("stats after replacement: %v", err)
	}
	if pending != 1 || failed != 0 {
		t.Fatalf("stats after replacement = pending %d failed %d, want 1/0", pending, failed)
	}

	oldJob.Status = jobDone
	oldJob.UpdatedAt = time.Now()
	if err := store.UpdateBangumiSyncJob(oldJob); !errors.Is(err, db.ErrBangumiJobLeaseLost) {
		t.Fatalf("old worker update error = %v, want lease lost", err)
	}
	newJob, err := store.ClaimBangumiSyncJob(time.Now().Add(2 * time.Second))
	if err != nil || newJob == nil {
		t.Fatalf("claim replacement job: job %+v err %v", newJob, err)
	}
	if newJob.ID != oldJob.ID || newJob.AccountGen != 2 {
		t.Fatalf("replacement job = id %d generation %d, want id %d generation 2",
			newJob.ID, newJob.AccountGen, oldJob.ID)
	}
	if err := svc.EnqueueWatched(user.ID, fileID); err != nil {
		t.Fatalf("enqueue duplicate after replacement: %v", err)
	}
	pending, _, _, err = store.BangumiSyncStats(user.ID)
	if err != nil || pending != 1 {
		t.Fatalf("pending after duplicate enqueue = %d, err %v; want 1", pending, err)
	}
}

func TestConcurrentConnectsAdvanceGenerationSerially(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/me" {
			writeTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()
	user := &models.User{Name: "alice", PassHash: "hash", Salt: "salt"}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	cfg := &config.Config{
		Secret: "test-secret",
		BGM:    config.BGMTV{APIBase: api.URL},
	}
	svc := NewService(store, metadata.NewManager(store, cfg), cfg)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, token := range []string{"first-token", "second-token"} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			<-start
			_, err := svc.Connect(t.Context(), user.ID, token)
			errs <- err
		}(token)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Connect: %v", err)
		}
	}
	account, err := store.BangumiAccountByUser(user.ID)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	if account.Generation != 2 {
		t.Fatalf("generation after two connects = %d, want 2", account.Generation)
	}
}

func TestCredentialSwitchWaitsForInFlightRemoteWrite(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrite) }) }
	defer release()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/me":
			writeTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/episodes":
			writeTestJSON(w, map[string]any{"data": []map[string]any{{
				"id": 101, "type": 0, "ep": 1, "sort": 1,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/users/alice/collections/42":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/users/-/collections/42":
			startOnce.Do(func() { close(writeStarted) })
			<-releaseWrite
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/users/-/collections/42/episodes":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()
	user, _, fileID := seedBangumiAnime(t, store)
	cfg := &config.Config{
		Secret: "test-secret",
		BGM:    config.BGMTV{APIBase: api.URL},
	}
	svc := NewService(store, metadata.NewManager(store, cfg), cfg)
	if _, err := svc.Connect(t.Context(), user.ID, "first-token"); err != nil {
		t.Fatalf("connect first token: %v", err)
	}
	if err := svc.EnqueueWatched(user.ID, fileID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	processDone := make(chan error, 1)
	go func() {
		_, err := svc.processOne(t.Context())
		processDone <- err
	}()
	select {
	case <-writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach the remote collection write")
	}

	connectDone := make(chan error, 1)
	go func() {
		_, err := svc.Connect(t.Context(), user.ID, "second-token")
		connectDone <- err
	}()
	select {
	case err := <-connectDone:
		t.Fatalf("credential switch completed while the old write was in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	release()
	if err := <-processDone; err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if err := <-connectDone; err != nil {
		t.Fatalf("connect replacement token: %v", err)
	}
	account, err := store.BangumiAccountByUser(user.ID)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	if account.Generation != 2 {
		t.Fatalf("generation after switch = %d, want 2", account.Generation)
	}
}

func TestBangumiAccountsAreIsolatedPerLocalUser(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/me" {
			http.NotFound(w, r)
			return
		}
		username := ""
		switch r.Header.Get("Authorization") {
		case "Bearer alice-token":
			username = "alice"
		case "Bearer bob-token":
			username = "bob"
		}
		if username == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, map[string]any{
			"id": 7, "username": username, "nickname": username,
		})
	}))
	defer api.Close()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()
	alice, _, fileID := seedBangumiAnime(t, store)
	bob := &models.User{Name: "bob", PassHash: "hash", Salt: "salt"}
	if err := store.CreateUser(bob); err != nil {
		t.Fatalf("create second user: %v", err)
	}
	cfg := &config.Config{
		Secret: "test-secret",
		BGM:    config.BGMTV{APIBase: api.URL},
	}
	svc := NewService(store, metadata.NewManager(store, cfg), cfg)
	if _, err := svc.Connect(t.Context(), alice.ID, "alice-token"); err != nil {
		t.Fatalf("connect alice: %v", err)
	}
	if _, err := svc.Connect(t.Context(), bob.ID, "bob-token"); err != nil {
		t.Fatalf("connect bob: %v", err)
	}
	aliceAccount, err := store.BangumiAccountByUser(alice.ID)
	if err != nil || aliceAccount.Username != "alice" {
		t.Fatalf("alice account = %+v, err %v", aliceAccount, err)
	}
	bobAccount, err := store.BangumiAccountByUser(bob.ID)
	if err != nil || bobAccount.Username != "bob" {
		t.Fatalf("bob account = %+v, err %v", bobAccount, err)
	}

	if err := svc.EnqueueWatched(alice.ID, fileID); err != nil {
		t.Fatalf("enqueue alice: %v", err)
	}
	if err := svc.EnqueueWatched(bob.ID, fileID); err != nil {
		t.Fatalf("enqueue bob: %v", err)
	}
	alicePending, _, _, err := store.BangumiSyncStats(alice.ID)
	if err != nil || alicePending != 1 {
		t.Fatalf("alice pending = %d, err %v; want 1", alicePending, err)
	}
	bobPending, _, _, err := store.BangumiSyncStats(bob.ID)
	if err != nil || bobPending != 1 {
		t.Fatalf("bob pending = %d, err %v; want 1", bobPending, err)
	}
}
