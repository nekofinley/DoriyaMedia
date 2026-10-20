package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"doriyamedia/internal/config"
)

// TMDB's zh-CN titles are Simplified, so a Traditional title from the 港澳台
// catalogue may return no results at all. The Simplified spelling is the second
// query, and a result that resembles the original name wins over the first row
// of the response.
func TestSearchTMDBTriesSimplifiedSpelling(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/tv" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query().Get("query")
		mu.Lock()
		seen = append(seen, query)
		mu.Unlock()
		results := []map[string]any{}
		if query == "间谍家家酒" {
			results = append(results, map[string]any{
				"id": 999, "name": "间谍过家家", "original_name": "SPY×FAMILY",
				"first_air_date": "2022-04-09", "vote_average": 8.4,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	m := NewManager(nil, &config.Config{TMDB: config.TMDB{
		APIKey: "test-key", APIBase: srv.URL, Language: "zh-CN",
	}})
	if got := m.searchTMDB(context.Background(), "tv", "間諜家家酒", 2022); got != 999 {
		t.Fatalf("searchTMDB = %d, want 999", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "間諜家家酒" || seen[1] != "间谍家家酒" {
		t.Fatalf("searches = %v, want the original then its Simplified spelling", seen)
	}
}

func TestPickTMDBPrefersAResemblingResult(t *testing.T) {
	results := []TMDBSearchItem{
		{ID: 1, Name: "完全无关的节目", FirstAirDate: "2022-01-01"},
		{ID: 2, Name: "间谍过家家", OriginalName: "SPY×FAMILY", FirstAirDate: "2022-04-09"},
	}
	idx, score := pickTMDB(results, "間諜家家酒", 2022)
	if idx != 1 || score < 60 {
		t.Fatalf("pickTMDB = %d/%d, want the second result with a strong score", idx, score)
	}
}
