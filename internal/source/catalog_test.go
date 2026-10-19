package source

import (
	"testing"
)

// The catalogue search wraps the matched words in <em> and serves films next to
// 番剧; both must be cleaned before the result can be shown as a card.
func TestParseCatalogHits(t *testing.T) {
	raw := []byte(`{"result":[
		{"season_id":43164,"media_id":1,"title":"<em class=\"keyword\">孤独摇滚</em>！","org_title":"<em class=\"keyword\">ぼっち</em>・ざ・ろっく！","cover":"http://i0.hdslb.com/a.jpg","desc":" 简介 ","index_show":"全12话","season_type":1,"season_type_name":"番剧","pubtime":1664553600,"ep_size":12,"media_score":{"score":9.8}},
		{"season_id":999,"media_id":2,"title":"某个电影","cover":"http://i0.hdslb.com/b.jpg","season_type":2,"season_type_name":"电影","ep_size":1},
		{"season_id":43164,"media_id":1,"title":"重复条目","season_type":1},
		{"season_id":0,"title":"没有季度","season_type":1}
	]}`)
	hits, err := parseCatalogHits(raw, 0)
	if err != nil {
		t.Fatalf("parseCatalogHits: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (film, duplicate and empty id filtered)", len(hits))
	}
	h := hits[0]
	if h.Title != "孤独摇滚！" {
		t.Errorf("Title = %q, want 孤独摇滚！", h.Title)
	}
	if h.Original != "ぼっち・ざ・ろっく！" {
		t.Errorf("Original = %q", h.Original)
	}
	if h.Cover != "https://i0.hdslb.com/a.jpg" {
		t.Errorf("Cover = %q, want https", h.Cover)
	}
	if h.Year != 2022 {
		t.Errorf("Year = %d, want 2022", h.Year)
	}
	if h.Episodes != 12 || h.Index != "全12话" || h.Score != 9.8 {
		t.Errorf("hit = %+v", h)
	}
}

func TestParseCatalogHitsLimit(t *testing.T) {
	raw := []byte(`{"result":[
		{"season_id":1,"title":"A","season_type":1},
		{"season_id":2,"title":"B","season_type":1},
		{"season_id":3,"title":"C","season_type":1}
	]}`)
	hits, err := parseCatalogHits(raw, 2)
	if err != nil || len(hits) != 2 {
		t.Fatalf("parseCatalogHits limit = %v, %v", hits, err)
	}
	if hits[0].SeasonID != 1 || hits[1].SeasonID != 2 {
		t.Fatalf("order changed: %+v", hits)
	}
}

// A search hit is a season; importing it means importing its whole series.
// The path must be what the scanner expects under the virtual root.
func TestSeriesDirPathShape(t *testing.T) {
	got := VirtualJoin("/", seriesDir("鬼灭之刃", 26801))
	if got != "/鬼灭之刃 [series 26801]" {
		t.Fatalf("series path = %q", got)
	}
	key, ok := seriesKeyFromDir(lastSegment(got))
	if !ok || key != 26801 {
		t.Fatalf("series key from dir = %d, %v", key, ok)
	}
}
