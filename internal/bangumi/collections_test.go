package bangumi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

func TestCollectionsClientUsesAnimePaginationParameters(t *testing.T) {
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v0/users/alice/collections" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer collection-token" {
			t.Errorf("Authorization = %q", got)
		}
		q := r.URL.Query()
		if got := q.Get("subject_type"); got != "2" {
			t.Errorf("subject_type = %q, want 2", got)
		}
		switch {
		case q.Get("type") == "":
			// type 0 means "all" and must not send a type filter.
			if got := q.Get("limit"); got != "1" {
				t.Errorf("limit for all collections = %q, want 1", got)
			}
			writeTestJSON(w, map[string]any{"total": 3, "limit": 1, "offset": 0, "data": []any{}})
		case q.Get("offset") == "0":
			if got := q.Get("type"); got != "3" {
				t.Errorf("type = %q, want 3", got)
			}
			if got := q.Get("limit"); got != "2" {
				t.Errorf("limit = %q, want 2", got)
			}
			writeTestJSON(w, map[string]any{
				"total": 3, "limit": 2, "offset": 0,
				"data": []map[string]any{{
					"subject_id": 42, "type": 3, "ep_status": 7,
					"updated_at": "2026-10-20T12:00:00+08:00",
					"subject": map[string]any{
						"id": 42, "name": "Test Anime", "name_cn": "测试番剧",
						"short_summary": "summary", "date": "2026-01-01", "eps": 12,
						"score": 8.1, "rank": 123,
						"images": map[string]string{"large": "https://lain.bgm.tv/pic/cover/l/42.jpg"},
					},
				}},
			})
		case q.Get("offset") == "2":
			if got := q.Get("limit"); got != "2" {
				t.Errorf("limit on second page = %q, want 2", got)
			}
			writeTestJSON(w, map[string]any{
				"total": 3, "limit": 2, "offset": 2,
				"data": []map[string]any{{
					"subject_id": 43, "type": 3, "ep_status": 1,
					"subject": map[string]any{"id": 43, "name": "Last Anime"},
				}},
			})
		default:
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	defer api.Close()

	client := NewClient("collection-token", api.URL, api.Client())
	first, err := client.Collections(t.Context(), "alice", 3, 0, 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Total != 3 || first.Limit != 2 || first.Offset != 0 || len(first.Data) != 1 {
		t.Fatalf("first page = %+v", first)
	}
	got := first.Data[0]
	if got.SubjectID != 42 || got.Subject.NameCN != "测试番剧" || got.EpStatus != 7 {
		t.Fatalf("first collection = %+v", got)
	}

	second, err := client.Collections(t.Context(), "alice", 3, 2, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.Offset != 2 || len(second.Data) != 1 || second.Data[0].SubjectID != 43 {
		t.Fatalf("second page = %+v", second)
	}
	if _, err := client.Collections(t.Context(), "alice", 0, 0, 1); err != nil {
		t.Fatalf("all collections: %v", err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}

	if _, err := client.Collections(t.Context(), "alice", 3, -1, 2); err == nil {
		t.Fatal("negative offset was accepted")
	}
	if _, err := client.Collections(t.Context(), "alice", 3, 0, 51); err == nil {
		t.Fatal("limit above 50 was accepted")
	}
	if _, err := client.Collections(t.Context(), "alice", 6, 0, 2); err == nil {
		t.Fatal("collection type above 5 was accepted")
	}
}

func TestCollectionsServiceIncludesLocalMatchesWithoutLeakingToken(t *testing.T) {
	const token = "personal-collection-token"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"want Bearer `+token+`"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/me":
			writeTestJSON(w, map[string]any{
				"id": 7, "username": "alice", "nickname": "Alice",
				"avatar": map[string]string{"large": "https://lain.bgm.tv/pic/user/l/alice.jpg"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/users/alice/collections":
			q := r.URL.Query()
			if q.Get("subject_type") != "2" || q.Get("type") != "3" ||
				q.Get("offset") != "0" || q.Get("limit") != "2" {
				t.Errorf("collection query = %v", q)
			}
			writeTestJSON(w, map[string]any{
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
							"id": 99, "name": "Not In Library", "name_cn": "库里没有",
							"short_summary": "remote only", "eps": 13,
							"images": map[string]string{"large": "https://example.test/not-allowed.jpg"},
						},
					},
				},
			})
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
	user, _, _ := seedBangumiAnime(t, store)
	local, err := store.ItemIDsByBGMIDs([]int{42})
	if err != nil {
		t.Fatalf("local item: %v", err)
	}
	if local[42] == 0 {
		t.Fatal("seed show was not matched")
	}

	cfg := &config.Config{Secret: "test-secret", BGM: config.BGMTV{APIBase: api.URL}}
	svc := NewService(store, nil, cfg)
	if _, err := svc.Connect(t.Context(), user.ID, token); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	page, err := svc.Collections(t.Context(), user.ID, 3, 0, 2)
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if !page.Connected || page.Username != "alice" || page.Nickname != "Alice" {
		t.Fatalf("account fields = %+v", page)
	}
	if page.Total != 3 || page.Offset != 0 || page.Limit != 2 || !page.HasMore {
		t.Fatalf("pagination = %+v", page)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	if page.Items[0].LocalItemID != local[42] {
		t.Fatalf("matched local_item_id = %d, want %d", page.Items[0].LocalItemID, local[42])
	}
	if page.Items[1].LocalItemID != 0 {
		t.Fatalf("unmatched local_item_id = %d, want 0", page.Items[1].LocalItemID)
	}
	if !strings.HasPrefix(page.Items[0].CoverURL, "/api/bangumi/cover?url=") {
		t.Fatalf("cover_url = %q, want local proxy", page.Items[0].CoverURL)
	}
	if page.Items[1].CoverURL != "" {
		t.Fatalf("non-Bangumi cover was exposed: %q", page.Items[1].CoverURL)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal page: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("collection response leaked the token: %s", raw)
	}
}

func TestCollectionsServiceTreatsUnboundAccountAsEmptyPage(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.DB.Close()
	user := &models.User{Name: "alice", PassHash: "hash", Salt: "salt"}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc := NewService(store, nil, &config.Config{Secret: "test-secret"})
	page, err := svc.Collections(t.Context(), user.ID, 0, 0, 50)
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if page.Connected || page.Total != 0 || page.Offset != 0 || page.Limit != 50 || page.HasMore {
		t.Fatalf("unbound page = %+v", page)
	}
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("unbound items = %#v, want non-nil empty slice", page.Items)
	}
	if _, err := svc.Collections(t.Context(), user.ID, 6, 0, 50); !errors.Is(err, ErrInvalidCollectionsQuery) {
		t.Fatalf("invalid type error = %v", err)
	}
}
