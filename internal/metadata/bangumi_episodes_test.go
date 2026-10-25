package metadata

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestBangumiEpisodesPaginatesLongShows(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		n := 1
		if offset == 0 {
			n = limit
		}
		data := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			ep := offset + i + 1
			data = append(data, map[string]any{
				"id": 1000 + ep, "type": 0, "ep": ep, "sort": ep,
				"name": "Episode " + strconv.Itoa(ep),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data, "total": 201, "limit": limit, "offset": offset,
		})
	}))
	defer srv.Close()

	client := NewBangumiClient("", srv.URL, &http.Client{Timeout: 5 * time.Second})
	eps, err := client.Episodes(t.Context(), 42)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 201 {
		t.Fatalf("episode count = %d, want 201", len(eps))
	}
	if eps[200].ID != 1201 || eps[200].Number != 201 {
		t.Fatalf("last episode = %+v, want ID 1201 number 201", eps[200])
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2 pages", requests)
	}
}
