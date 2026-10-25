package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"doriyamedia/internal/models"
)

func TestBangumiCollectionsIsAnEmptySuccessfulPageWhenUnbound(t *testing.T) {
	_, ts := newTestServer(t)
	admin := setupAdmin(t, ts.URL)

	code, body := call(t, admin, http.MethodGet, ts.URL+"/api/bangumi/collections?type=0&offset=0&limit=50", nil)
	if code != http.StatusOK {
		t.Fatalf("unbound collections: code %d body %v", code, body)
	}
	if body["connected"] != false || body["total"] != float64(0) ||
		body["offset"] != float64(0) || body["limit"] != float64(50) || body["has_more"] != false {
		t.Fatalf("unbound collections page = %v", body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("unbound items = %#v, want []", body["items"])
	}

	for _, path := range []string{
		"/api/bangumi/collections?type=6",
		"/api/bangumi/collections?offset=-1",
		"/api/bangumi/collections?limit=0",
		"/api/bangumi/collections?limit=51",
	} {
		if code, _ := call(t, admin, http.MethodGet, ts.URL+path, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, code)
		}
	}
}

func TestBangumiCollectionsRouteMatchesLibraryAndNeverReturnsToken(t *testing.T) {
	const token = "route-secret-token"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/me":
			writeBangumiTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
				"avatar": map[string]string{"large": "https://lain.bgm.tv/pic/user/l/alice.jpg"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/users/alice/collections":
			q := r.URL.Query()
			if q.Get("subject_type") != "2" || q.Get("type") != "3" ||
				q.Get("offset") != "0" || q.Get("limit") != "2" {
				t.Errorf("collection query = %v", q)
			}
			writeBangumiTestJSON(w, map[string]any{
				"total": 3, "limit": 2, "offset": 0,
				"data": []map[string]any{
					{
						"subject_id": 42, "type": 3, "ep_status": 8,
						"updated_at": "2026-10-20T12:00:00+08:00",
						"subject": map[string]any{
							"id": 42, "name": "Test Anime", "name_cn": "测试番剧",
							"short_summary": "summary", "date": "2026-01-01", "eps": 12,
							"score": 8.1, "rank": 123,
							"images": map[string]string{"large": "https://lain.bgm.tv/pic/cover/l/42.jpg"},
						},
					},
					{
						"subject_id": 99, "type": 3, "ep_status": 2,
						"subject": map[string]any{
							"id": 99, "name": "Remote Only", "name_cn": "未收录",
							"short_summary": "not in the library", "eps": 13,
							"images": map[string]string{"large": "https://lain.bgm.tv/pic/cover/l/99.jpg"},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	srv, ts := newTestServer(t)
	srv.Cfg.BGM.APIBase = api.URL
	srv.Cfg.BGM.UseProxy = false
	admin := setupAdmin(t, ts.URL)

	src := &models.Source{Name: "local", Type: "local", Config: "{}"}
	if err := srv.Store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	lib := &models.Library{Name: "anime", Type: "series", SourceID: src.ID, Path: "/", Providers: "[]"}
	if err := srv.Store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	show := &models.Item{LibraryID: lib.ID, Type: "show", Name: "Test Anime", BGMID: 42}
	if err := srv.Store.InsertItem(show); err != nil {
		t.Fatalf("insert local show: %v", err)
	}

	code, body := call(t, admin, http.MethodPut, ts.URL+"/api/bangumi/account", map[string]string{"token": token})
	if code != http.StatusOK || body["connected"] != true {
		t.Fatalf("connect Bangumi: code %d body %v", code, body)
	}
	code, page := call(t, admin, http.MethodGet, ts.URL+"/api/bangumi/collections?type=3&offset=0&limit=2", nil)
	if code != http.StatusOK {
		t.Fatalf("collections: code %d body %v", code, page)
	}
	if page["connected"] != true || page["username"] != "alice" || page["nickname"] != "Alice" {
		t.Fatalf("account fields = %v", page)
	}
	if page["total"] != float64(3) || page["offset"] != float64(0) ||
		page["limit"] != float64(2) || page["has_more"] != true {
		t.Fatalf("pagination = %v", page)
	}
	items, ok := page["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v", page["items"])
	}
	matched := items[0].(map[string]any)
	unmatched := items[1].(map[string]any)
	if got := int64(matched["local_item_id"].(float64)); got != show.ID {
		t.Fatalf("matched local_item_id = %d, want %d", got, show.ID)
	}
	if unmatched["local_item_id"] != float64(0) {
		t.Fatalf("unmatched local_item_id = %v, want 0", unmatched["local_item_id"])
	}
	cover, err := url.Parse(matched["cover_url"].(string))
	if err != nil || cover.Path != "/api/bangumi/cover" {
		t.Fatalf("cover_url = %q, err %v", matched["cover_url"], err)
	}
	if got := cover.Query().Get("url"); got != "https://lain.bgm.tv/pic/cover/l/42.jpg" {
		t.Fatalf("proxied cover URL = %q", got)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), token) {
		t.Fatalf("collections response leaked the token: %s", raw)
	}
}

func TestBangumiCoverRouteRejectsNonBangumiHostsAndRequiresLogin(t *testing.T) {
	_, ts := newTestServer(t)
	admin := setupAdmin(t, ts.URL)
	target := ts.URL + "/api/bangumi/cover?url=" + url.QueryEscape("https://example.com/cover.jpg")

	if code, _ := call(t, browser(t), http.MethodGet, target, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous cover request = %d, want 401", code)
	}
	if code, body := call(t, admin, http.MethodGet, target, nil); code != http.StatusBadRequest {
		t.Fatalf("non-Bangumi cover = %d, body %v; want 400", code, body)
	}
	for _, rawURL := range []string{
		"https://lain.bgm.tv.evil.example/cover.jpg",
		"https://lain.bgm.tv:444/cover.jpg",
	} {
		target := ts.URL + "/api/bangumi/cover?url=" + url.QueryEscape(rawURL)
		if code, _ := call(t, admin, http.MethodGet, target, nil); code != http.StatusBadRequest {
			t.Errorf("cover %q = %d, want 400", rawURL, code)
		}
	}
}

func writeBangumiTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
