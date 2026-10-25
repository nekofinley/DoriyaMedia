package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func TestOpenRebuildsLegacyBangumiSyncJobConstraint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-bangumi.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	// This is the queue from the first per-user Bangumi release: unresolved
	// jobs all use episode_id=0, so this UNIQUE constraint rejects the second
	// episode of the same subject before the worker can resolve either one.
	if _, err := old.Exec(`CREATE TABLE bangumi_sync_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		subject_id INTEGER NOT NULL DEFAULT 0,
		episode_id INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMP NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		UNIQUE(user_id, subject_id, episode_id)
	)`); err != nil {
		t.Fatalf("create legacy queue: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy queue: %v", err)
	}
	defer store.DB.Close()

	legacy, err := store.hasLegacyBangumiSyncUnique()
	if err != nil {
		t.Fatalf("inspect migrated indexes: %v", err)
	}
	if legacy {
		t.Fatal("legacy episode_id unique constraint survived migration")
	}

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
	episodeIDs := make([]int64, 0, 2)
	for number := 1; number <= 2; number++ {
		ep := &models.Item{
			LibraryID: lib.ID, Type: "episode", Name: "Episode",
			ParentID: &show.ID, Episode: number,
		}
		if err := store.InsertItem(ep); err != nil {
			t.Fatalf("insert episode %d: %v", number, err)
		}
		fileID, err := store.UpsertFile(&models.MediaFile{
			ItemID: ep.ID, SourceID: src.ID, Path: fmt.Sprintf("/anime/episode-%02d.mkv", number),
		})
		if err != nil {
			t.Fatalf("insert file %d: %v", number, err)
		}
		episodeIDs = append(episodeIDs, fileID)
	}
	account := &models.BangumiAccount{
		UserID: user.ID, Username: "alice", TokenEnc: "v1:test",
		SyncEnabled: true, ConnectedAt: time.Now(),
	}
	if err := store.SaveBangumiCredential(account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	now := time.Now()
	if err := store.EnqueueBangumiSync(user.ID, episodeIDs[0], 42, 1, now); err != nil {
		t.Fatalf("enqueue episode 1: %v", err)
	}
	if err := store.EnqueueBangumiSync(user.ID, episodeIDs[1], 42, 2, now.Add(time.Second)); err != nil {
		t.Fatalf("enqueue episode 2: %v", err)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM bangumi_sync_jobs
		WHERE user_id=? AND subject_id=?`, user.ID, 42).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 2 {
		t.Fatalf("job count = %d, want 2 episodes of one subject", count)
	}

	// Simulate a database left by the older credential-replacement path, which
	// incremented the account generation but left a processing job behind.
	job, err := store.ClaimBangumiSyncJob(now.Add(2 * time.Second))
	if err != nil || job == nil {
		t.Fatalf("claim job before simulated upgrade: job %+v err %v", job, err)
	}
	if _, err := store.DB.Exec(`UPDATE bangumi_accounts SET generation=generation+1
		WHERE user_id=?`, user.ID); err != nil {
		t.Fatalf("advance account generation: %v", err)
	}
	if err := store.reconcileBangumiSyncGenerations(time.Now()); err != nil {
		t.Fatalf("reconcile generations: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM bangumi_sync_jobs
		WHERE user_id=? AND account_generation=2 AND status='pending' AND lease_token=''`,
		user.ID).Scan(&count); err != nil {
		t.Fatalf("count reconciled jobs: %v", err)
	}
	if count != 2 {
		t.Fatalf("reconciled jobs = %d, want both jobs pending under generation 2", count)
	}
}
