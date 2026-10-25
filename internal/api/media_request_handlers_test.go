package api

import (
	"strconv"
	"testing"

	"doriyamedia/internal/models"
)

func TestMediaRequestWorkflowAndAdminBoundary(t *testing.T) {
	_, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	alice := browser(t)
	if code, body := call(t, alice, "POST", base+"/api/auth/register", map[string]string{
		"name": "alice", "password": "alice-pass",
	}); code != 200 {
		t.Fatalf("register alice: code %d body %v", code, body)
	}

	if code, _ := call(t, browser(t), "GET", base+"/api/requests", nil); code != 401 {
		t.Fatalf("anonymous request list = %d, want 401", code)
	}

	payload := map[string]any{
		"provider": "manual",
		"title":    "求一部还没有入库的番",
		"year":     2026,
		"note":     "希望有中文字幕",
	}
	if code, body := call(t, alice, "POST", base+"/api/requests", map[string]any{
		"provider": "manual", "title": "评分异常", "rating": 11,
	}); code != 400 {
		t.Fatalf("invalid rating: code %d body %v, want 400", code, body)
	}
	if code, body := call(t, alice, "POST", base+"/api/requests", map[string]any{
		"provider": "bangumi", "title": "来源异常", "bgm_id": 1,
		"cover_url": "https://127.0.0.1/private.png",
	}); code != 400 {
		t.Fatalf("untrusted cover host: code %d body %v, want 400", code, body)
	}
	code, body := call(t, alice, "POST", base+"/api/requests", payload)
	if code != 200 {
		t.Fatalf("create request: code %d body %v", code, body)
	}
	created, ok := body["request"].(map[string]any)
	if !ok || created["status"] != "pending" || created["user_name"] != "alice" {
		t.Fatalf("created request = %v", body)
	}
	id := created["id"]

	// The same pending title is idempotent for its author.
	code, body = call(t, alice, "POST", base+"/api/requests", payload)
	if code != 200 {
		t.Fatalf("duplicate request: code %d body %v", code, body)
	}
	duplicate := body["request"].(map[string]any)
	if duplicate["id"] != id {
		t.Fatalf("duplicate id = %v, want %v", duplicate["id"], id)
	}

	code, body = call(t, alice, "GET", base+"/api/requests", nil)
	if code != 200 {
		t.Fatalf("list own requests: code %d body %v", code, body)
	}
	if items := body["items"].([]any); len(items) != 1 {
		t.Fatalf("own request count = %d, want 1", len(items))
	}

	if code, _ := call(t, alice, "GET", base+"/api/admin/requests", nil); code != 403 {
		t.Fatalf("viewer admin request board = %d, want 403", code)
	}
	path := base + "/api/admin/requests/" + numberString(id)
	if code, _ := call(t, alice, "PUT", path, map[string]any{"status": "completed"}); code != 403 {
		t.Fatalf("viewer complete request = %d, want 403", code)
	}
	if code, _ := call(t, alice, "POST", path+"/import", nil); code != 403 {
		t.Fatalf("viewer request import = %d, want 403", code)
	}
	if code, _ := call(t, alice, "DELETE", path, nil); code != 403 {
		t.Fatalf("viewer delete request = %d, want 403", code)
	}
	code, body = call(t, admin, "GET", base+"/api/admin/requests", nil)
	if code != 200 {
		t.Fatalf("admin request board: code %d body %v", code, body)
	}
	if pending := body["pending"].([]any); len(pending) != 1 {
		t.Fatalf("admin pending count = %d, want 1", len(pending))
	}

	code, body = call(t, admin, "PUT", path, map[string]any{
		"status": "completed",
	})
	if code != 200 {
		t.Fatalf("complete request: code %d body %v", code, body)
	}
	if done := body["request"].(map[string]any); done["status"] != "completed" || done["completed_at"] == nil {
		t.Fatalf("completed request = %v", done)
	}
	if code, body := call(t, admin, "POST", path+"/import", nil); code != 400 {
		t.Fatalf("import completed request: code %d body %v, want 400", code, body)
	}

	code, body = call(t, alice, "GET", base+"/api/requests", nil)
	if code != 200 {
		t.Fatalf("list completed request: code %d body %v", code, body)
	}
	if item := body["items"].([]any)[0].(map[string]any); item["status"] != "completed" {
		t.Fatalf("viewer status = %v, want completed", item["status"])
	}

	code, body = call(t, admin, "PUT", path, map[string]any{"status": "pending"})
	if code != 200 {
		t.Fatalf("reopen request: code %d body %v", code, body)
	}
	if reopened := body["request"].(map[string]any); reopened["status"] != "pending" ||
		reopened["completed_at"] != nil || reopened["admin_note"] != nil {
		t.Fatalf("reopened request = %v", reopened)
	}

	if code, body := call(t, admin, "DELETE", path, nil); code != 200 {
		t.Fatalf("delete request: code %d body %v", code, body)
	}
}

func TestRequestCoverAllowlist(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		provider string
		want     bool
	}{
		{"https://i0.hdslb.com/bfs/bangumi/a.jpg", "bilibili", true},
		{"https://lain.bgm.tv/pic/cover/c/a.jpg", "bangumi", true},
		{"https://hdslb.com.evil.example/a.jpg", "bilibili", false},
		{"https://127.0.0.1/a.jpg", "bilibili", false},
		{"https://bgm.tv.evil.example/a.jpg", "bangumi", false},
	} {
		if got := requestCoverAllowed(tc.raw, tc.provider); got != tc.want {
			t.Errorf("requestCoverAllowed(%q, %q) = %v, want %v", tc.raw, tc.provider, got, tc.want)
		}
	}
}

func TestImportJobArchivesAttachedRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	req, err := srv.Store.CreateMediaRequest(&models.MediaRequest{
		UserID: 3, UserName: "viewer", Provider: "bilibili",
		Title: "等待入库的番", SourceID: 1, LibraryID: 1, SeasonID: 2,
	})
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	job := &importJob{
		Phase: "meta",
		RequestIDs: map[int64]struct{}{
			req.ID: {},
		},
	}
	job.complete(srv, 987)
	if !job.finished() {
		t.Fatal("completed import job is still running")
	}
	saved, err := srv.Store.MediaRequestByID(req.ID)
	if err != nil {
		t.Fatalf("read archived request: %v", err)
	}
	if saved.Status != models.MediaRequestCompleted || saved.ItemID != 987 {
		t.Fatalf("archived request = %+v", saved)
	}

	// Once a job is finished, another request cannot attach to it and wait
	// forever for an archive that already happened.
	job.addRequest(req.ID + 1)
	if len(job.RequestIDs) != 1 {
		t.Fatalf("finished job accepted another request: %v", job.RequestIDs)
	}
}

func numberString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	}
	return ""
}
