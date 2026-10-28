package api

import (
	"strconv"
	"testing"
)

// The exclusion lists: both start empty, the global folder list round-trips
// through /api/config, and a library can carry its own folder list.

func TestConfigExcludeListsRoundTrip(t *testing.T) {
	_, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	// A fresh install excludes nothing — no built-in keywords to undo.
	code, cfg := call(t, admin, "GET", base+"/api/config", nil)
	if code != 200 {
		t.Fatalf("get config: code %d body %v", code, cfg)
	}
	if list, ok := cfg["exclude_keywords"].([]any); !ok || len(list) != 0 {
		t.Errorf("exclude_keywords should start empty, got %v", cfg["exclude_keywords"])
	}
	if list, ok := cfg["exclude_folders"].([]any); !ok || len(list) != 0 {
		t.Errorf("exclude_folders should start empty, got %v", cfg["exclude_folders"])
	}

	// Both lists are saved as sent, whitespace trimmed and blanks dropped.
	if code, body := call(t, admin, "PUT", base+"/api/config", map[string]any{
		"exclude_keywords": []string{" nc-ed ", ""},
		"exclude_folders":  []string{" Extras ", "特典", " "},
	}); code != 200 {
		t.Fatalf("put config: code %d body %v", code, body)
	}
	_, cfg = call(t, admin, "GET", base+"/api/config", nil)
	if got := stringsOf(cfg["exclude_keywords"]); len(got) != 1 || got[0] != "nc-ed" {
		t.Errorf("exclude_keywords = %v", got)
	}
	if got := stringsOf(cfg["exclude_folders"]); len(got) != 2 || got[0] != "Extras" || got[1] != "特典" {
		t.Errorf("exclude_folders = %v", got)
	}

	// Clearing is an answer too, not "keep what was there".
	if code, body := call(t, admin, "PUT", base+"/api/config", map[string]any{
		"exclude_keywords": []string{},
		"exclude_folders":  []string{},
	}); code != 200 {
		t.Fatalf("clear config: code %d body %v", code, body)
	}
	_, cfg = call(t, admin, "GET", base+"/api/config", nil)
	if got := stringsOf(cfg["exclude_folders"]); len(got) != 0 {
		t.Errorf("exclude_folders after clearing = %v", got)
	}
}

func TestLibraryFolderExcludes(t *testing.T) {
	_, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	code, src := call(t, admin, "POST", base+"/api/sources", map[string]any{
		"name": "本地", "type": "local", "config": map[string]any{"root": "/media"},
	})
	if code != 200 {
		t.Fatalf("create source: code %d body %v", code, src)
	}
	sourceID := src["id"]

	// A library that keeps its own folder list.
	code, lib := call(t, admin, "POST", base+"/api/libraries", map[string]any{
		"name": "剧集", "type": "series", "source_id": sourceID, "path": "/tv",
		"sources":             []map[string]any{{"source_id": sourceID, "path": "/tv"}},
		"exclude_mode":        "inherit",
		"exclude_folder_mode": "custom",
		"exclude_folders":     []string{"Extras", "特典"},
	})
	if code != 200 {
		t.Fatalf("create library: code %d body %v", code, lib)
	}
	if lib["exclude_folder_mode"] != "custom" {
		t.Errorf("exclude_folder_mode = %v", lib["exclude_folder_mode"])
	}
	// A library's lists come back as json text, the shape the column holds.
	if got := lib["exclude_folders"]; got != `["Extras","特典"]` {
		t.Errorf("exclude_folders = %v", got)
	}
	// The file-name list was not given one, so it follows the global list.
	if lib["exclude_mode"] != "inherit" || lib["excludes"] != "[]" {
		t.Errorf("file list should inherit, got mode %v list %v", lib["exclude_mode"], lib["excludes"])
	}

	// Switching back to the global list drops the library's own entries.
	id := idString(t, lib["id"])
	code, lib = call(t, admin, "PUT", base+"/api/libraries/"+id, map[string]any{
		"name": "剧集", "type": "series",
		"exclude_folder_mode": "inherit",
	})
	if code != 200 {
		t.Fatalf("update library: code %d body %v", code, lib)
	}
	if lib["exclude_folder_mode"] != "inherit" || lib["exclude_folders"] != "[]" {
		t.Errorf("after inherit: mode %v list %v", lib["exclude_folder_mode"], lib["exclude_folders"])
	}
}

// stringsOf reads a json list of strings out of a decoded response body.
func stringsOf(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// idString formats a json number id the way a URL wants it.
func idString(t *testing.T, v any) string {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("id is not a number: %v", v)
	}
	return strconv.FormatInt(int64(f), 10)
}

// A /…/ entry that does not compile is refused with a message naming it, for the
// global lists and for a library's own lists alike.
func TestBrokenRegexIsRefused(t *testing.T) {
	_, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	code, body := call(t, admin, "PUT", base+"/api/config", map[string]any{
		"exclude_folders": []string{`/(unclosed/`},
	})
	if code != 400 {
		t.Fatalf("broken regex accepted: code %d body %v", code, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("error message should name the entry, got %v", body)
	}
	// Nothing was written.
	_, cfg := call(t, admin, "GET", base+"/api/config", nil)
	if got := stringsOf(cfg["exclude_folders"]); len(got) != 0 {
		t.Errorf("exclude_folders should still be empty, got %v", got)
	}

	code, src := call(t, admin, "POST", base+"/api/sources", map[string]any{
		"name": "本地", "type": "local", "config": map[string]any{"root": "/media"},
	})
	if code != 200 {
		t.Fatalf("create source: code %d body %v", code, src)
	}
	code, body = call(t, admin, "POST", base+"/api/libraries", map[string]any{
		"name": "剧集", "type": "series", "source_id": src["id"], "path": "/tv",
		"exclude_mode": "custom", "excludes": []string{"*bad"},
	})
	if code != 200 {
		t.Fatalf("a plain keyword must never be rejected, code %d body %v", code, body)
	}

	code, body = call(t, admin, "POST", base+"/api/libraries", map[string]any{
		"name": "剧集", "type": "series", "source_id": src["id"], "path": "/tv",
		"exclude_folder_mode": "custom", "exclude_folders": []string{`/(unclosed/`},
	})
	if code != 400 {
		t.Fatalf("broken folder regex accepted: code %d body %v", code, body)
	}

	// A good one goes through.
	code, lib := call(t, admin, "POST", base+"/api/libraries", map[string]any{
		"name": "剧集", "type": "series", "source_id": src["id"], "path": "/tv",
		"exclude_mode": "custom", "excludes": []string{`/^\[PV\d+\]$/`},
	})
	if code != 200 {
		t.Fatalf("valid regex rejected: code %d body %v", code, lib)
	}
	if got := lib["excludes"]; got != `["/^\\[PV\\d+\\]$/"]` {
		t.Errorf("excludes = %v", got)
	}
}
