package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

func TestSaveProgressRejectsInvalidRange(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()
	user := &models.User{Name: "viewer", PassHash: "hash", Salt: "salt"}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	fileID := seedProgressFile(t, store)
	s := &Server{Store: store}
	id := strconv.FormatInt(fileID, 10)

	for _, body := range []string{
		`{"file_id":` + id + `,"position":-1,"duration":100}`,
		`{"file_id":` + id + `,"position":101,"duration":100}`,
		`{"file_id":` + id + `,"position":10,"duration":0}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/progress", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleSaveProgress(rec, req, user)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}

	// The normal player save must still work.
	req := httptest.NewRequest(http.MethodPost, "/api/progress", strings.NewReader(
		`{"file_id":`+id+`,"position":40,"duration":100}`))
	rec := httptest.NewRecorder()
	s.handleSaveProgress(rec, req, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid progress status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func seedProgressFile(t *testing.T, store *db.Store) int64 {
	t.Helper()
	src := &models.Source{Name: "local", Type: "local", Config: "{}"}
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	lib := &models.Library{Name: "films", Type: "movie", SourceID: src.ID, Path: "/", Providers: "[]"}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	item := &models.Item{LibraryID: lib.ID, Type: "movie", Name: "Film"}
	if err := store.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	id, err := store.UpsertFile(&models.MediaFile{ItemID: item.ID, SourceID: src.ID, Path: "/film.mkv"})
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
	return id
}
