package api

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
)

func newLegacyHandlerSecurityServer(t *testing.T) (*Server, *db.Store) {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	store, err := db.Open(filepath.Join(cfg.DataDir, "legacy-handler-security.db"))
	if err != nil {
		t.Fatalf("open security test store: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	return &Server{
		Store:  store,
		Cfg:    cfg,
		Engine: media.NewEngine(store, cfg),
	}, store
}

func seedLegacyHandlerFile(t *testing.T, store *db.Store, sourceType, sourceConfig, path string) *models.MediaFile {
	t.Helper()
	src := &models.Source{Name: "legacy-handler-security", Type: sourceType, Config: sourceConfig}
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create security test source: %v", err)
	}
	lib := &models.Library{
		Name: "legacy-handler-security", Type: "movie", SourceID: src.ID,
		Path: "/", Providers: "[]",
	}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create security test library: %v", err)
	}
	item := &models.Item{LibraryID: lib.ID, Type: "movie", Name: "security fixture"}
	if err := store.InsertItem(item); err != nil {
		t.Fatalf("create security test item: %v", err)
	}
	fileID, err := store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: src.ID, Path: path,
	})
	if err != nil {
		t.Fatalf("create security test file: %v", err)
	}
	file, err := store.FileByID(fileID)
	if err != nil {
		t.Fatalf("read security test file: %v", err)
	}
	return file
}

func TestLegacyProbeErrorRedactsSignedURL(t *testing.T) {
	srv, store := newLegacyHandlerSecurityServer(t)
	const secretURL = "https://cdn.example.test/video.mp4?token=top-secret"
	file := seedLegacyHandlerFile(t, store, "bilibili", "{}",
		"/not-a-playable.mp4?source="+secretURL)

	req := httptest.NewRequest(http.MethodGet, "/api/play/"+strconv.FormatInt(file.ID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(file.ID, 10))
	rec := httptest.NewRecorder()
	srv.handlePlay(rec, req, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("probe failure status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	for _, secret := range []string{"top-secret", "cdn.example.test", "https://"} {
		if strings.Contains(body, secret) {
			t.Errorf("probe failure response leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "媒体探测失败") {
		t.Fatalf("probe failure response lacks fixed public error: %s", body)
	}
}

func TestLegacyHTTPSStreamDoesNotRedirectToPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "proxied-media")
	}))
	defer upstream.Close()

	srv, store := newLegacyHandlerSecurityServer(t)
	sourceConfig := `{"webdav_url":"` + upstream.URL + `"}`
	file := seedLegacyHandlerFile(t, store, "webdav", sourceConfig, "/video.mp4")

	req := httptest.NewRequest(http.MethodGet,
		"https://app.example.test/api/stream/"+strconv.FormatInt(file.ID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(file.ID, 10))
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)

	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("HTTPS stream sent Location %q", location)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTPS fallback status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "proxied-media" {
		t.Fatalf("HTTPS fallback body = %q, want proxied media", body)
	}
}

func TestLegacyStreamProxyRejectsUnconfiguredPrivateTarget(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "private-media")
	}))
	defer upstream.Close()

	srv, store := newLegacyHandlerSecurityServer(t)
	file := seedLegacyHandlerFile(t, store, "url", "{}",
		upstream.URL+"/video.mp4?token=must-not-be-fetched")

	req := httptest.NewRequest(http.MethodGet,
		"/api/stream/"+strconv.FormatInt(file.ID, 10)+"?proxy=1", nil)
	req.SetPathValue("id", strconv.FormatInt(file.ID, 10))
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("private target status = %d, want %d; body=%q",
			rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("private target was fetched %d time(s)", hits)
	}
	for _, secret := range []string{"127.0.0.1", "must-not-be-fetched", "private-media"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("private target response leaked %q: %s", secret, rec.Body.String())
		}
	}
}
