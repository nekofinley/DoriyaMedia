package db

import (
	"path/filepath"
	"sync"
	"testing"

	"doriyamedia/internal/models"
)

func TestMediaRequestLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.DB.Close()

	first, err := store.CreateMediaRequest(&models.MediaRequest{
		UserID: 7, UserName: "alice", Provider: "manual",
		Title: "想看但还没有的番", Note: "希望有中文字幕",
	})
	if err != nil {
		t.Fatalf("create first request: %v", err)
	}
	if first.ID == 0 || first.Status != models.MediaRequestPending {
		t.Fatalf("created request = %+v", first)
	}

	// A repeated submission is returned as the existing pending card rather
	// than creating duplicate work for the administrator.
	duplicate, err := store.CreateMediaRequest(&models.MediaRequest{
		UserID: 7, UserName: "alice", Provider: "bangumi", Title: "想看但还没有的番",
	})
	if err != nil {
		t.Fatalf("create duplicate: %v", err)
	}
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate id = %d, want %d", duplicate.ID, first.ID)
	}

	if _, err := store.CreateMediaRequest(&models.MediaRequest{
		UserID: 8, UserName: "bob", Provider: "manual", Title: "另一部番",
	}); err != nil {
		t.Fatalf("create second user's request: %v", err)
	}

	mine, err := store.MediaRequests(7, "")
	if err != nil {
		t.Fatalf("list own requests: %v", err)
	}
	if len(mine) != 1 || mine[0].UserName != "alice" {
		t.Fatalf("own requests = %+v", mine)
	}
	pending, err := store.MediaRequests(0, models.MediaRequestPending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending count = %d, want 2", len(pending))
	}

	completed, err := store.CompleteMediaRequest(first.ID, 42, "已入库")
	if err != nil {
		t.Fatalf("complete request: %v", err)
	}
	if completed.Status != models.MediaRequestCompleted || completed.ItemID != 42 ||
		completed.CompletedAt == nil || completed.AdminNote != "已入库" {
		t.Fatalf("completed request = %+v", completed)
	}
	if again, err := store.CompleteMediaRequest(first.ID, 42, "已入库"); err != nil || again.ID != first.ID {
		t.Fatalf("idempotent complete: request=%+v err=%v", again, err)
	}

	reopened, err := store.ReopenMediaRequest(first.ID)
	if err != nil {
		t.Fatalf("reopen request: %v", err)
	}
	if reopened.Status != models.MediaRequestPending || reopened.ItemID != 0 ||
		reopened.CompletedAt != nil || reopened.AdminNote != "" {
		t.Fatalf("reopened request = %+v", reopened)
	}

	if err := store.DeleteMediaRequest(first.ID); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	if _, err := store.MediaRequestByID(first.ID); err == nil {
		t.Fatal("deleted request is still readable")
	}
}

func TestMediaRequestConcurrentDuplicate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.DB.Close()

	const workers = 12
	start := make(chan struct{})
	type result struct {
		id  int64
		err error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			saved, err := store.CreateMediaRequest(&models.MediaRequest{
				UserID: 9, UserName: "alice", Provider: "manual", Title: "并发求片",
			})
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{id: saved.ID}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var first int64
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent create: %v", result.err)
		}
		if first == 0 {
			first = result.id
		}
		if result.id != first {
			t.Fatalf("concurrent ids = %d and %d, want one request", first, result.id)
		}
	}
	items, err := store.MediaRequests(9, "")
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("concurrent request count = %d, want 1", len(items))
	}
}
