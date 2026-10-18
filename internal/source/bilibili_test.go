package source

import (
	"strings"
	"testing"
)

func TestCodecOf(t *testing.T) {
	cases := []struct {
		in                string
		name, tag, profil string
	}{
		{"avc1.64001F", "h264", "avc1", "High"},
		{"avc1.4D401E", "h264", "avc1", "Main"},
		{"avc1.42E01E", "h264", "avc1", "Baseline"},
		{"avc1.6E0033", "h264", "avc1", "High 10"}, // Hi10P: no hardware decode path
		{"hev1.1.6.L120.90", "hevc", "hev1", "Main"},
		{"hvc1.1.6.L120.90", "hevc", "hvc1", "Main"},
		{"av01.0.08M.08.0.110.01.01.01.0", "av1", "av01", "Main"},
		{"mp4a.40.2", "aac", "mp4a", ""},
		{"mp4a.40.5", "aac", "mp4a", ""},
	}
	for _, c := range cases {
		name, tag, profile := codecOf(c.in)
		if name != c.name || tag != c.tag || profile != c.profil {
			t.Errorf("codecOf(%q) = %q/%q/%q, want %q/%q/%q",
				c.in, name, tag, profile, c.name, c.tag, c.profil)
		}
	}
}

// The episode number has to survive the tree, and a season directory has to be
// recognisable from its own name alone — that is what keeps the driver
// stateless across a restart.
func TestVirtualPathsRoundTrip(t *testing.T) {
	ep := &pgcEpisode{ID: 267851, Title: "1", LongTitle: "残酷"}
	name := episodeFileName("鬼灭之刃", ep, "")
	if !strings.HasSuffix(name, ".mp4") {
		t.Fatalf("episode name %q is not a video file", name)
	}
	dir := seasonDir("鬼灭之刃", 26801)
	path := VirtualJoin("/热门番剧", VirtualJoin(dir, name))

	id, ok := episodeIDFromPath(path)
	if !ok || id != 267851 {
		t.Fatalf("episodeIDFromPath(%q) = %d/%v, want 267851", path, id, ok)
	}
	parent := path[:strings.LastIndex(path, "/")]
	sid, ok := seasonIDFromDir(lastSegment(parent))
	if !ok || sid != 26801 {
		t.Fatalf("seasonIDFromDir(%q) = %d/%v, want 26801", parent, sid, ok)
	}
	// A file path must never be mistaken for a season directory.
	if _, ok := seasonIDFromDir(lastSegment(path)); ok {
		t.Fatalf("file path %q parsed as a season directory", path)
	}
}

func TestEpisodeFileNameSections(t *testing.T) {
	ep := &pgcEpisode{ID: 5, Title: "PV1", LongTitle: "正式预告"}
	got := episodeFileName("某番", ep, "")
	if !strings.Contains(got, "PV1") || !strings.Contains(got, "正式预告") || !strings.Contains(got, "[ep5]") {
		t.Fatalf("unexpected name for a non-numeric episode: %q", got)
	}
	// Titles carry characters a path may not.
	dirty := episodeFileName(`A/B:C*D?E"F<G>H|I`, &pgcEpisode{ID: 1, Title: "1"}, "")
	if strings.ContainsAny(strings.SplitN(dirty, " ", 2)[0], `/\:*?"<>|`) {
		t.Fatalf("separators survived sanitising: %q", dirty)
	}
}

func TestBiliClean(t *testing.T) {
	if got := biliClean("  鬼灭   之刃  "); got != "鬼灭 之刃" {
		t.Fatalf("biliClean collapses whitespace, got %q", got)
	}
	if got := biliClean(""); got != "未命名" {
		t.Fatalf("biliClean(\"\") = %q", got)
	}
	if got := biliClean(strings.Repeat("长", 200)); len([]rune(got)) > 90 {
		t.Fatalf("biliClean left %d runes, want <= 90", len([]rune(got)))
	}
}

// The ladder is walked from the top: the best quality the account may have, and
// within one rung the codec most clients can decode.
func TestPickVideo(t *testing.T) {
	d := &BilibiliDriver{codec: "avc"}
	reps := []biliRep{
		{ID: 32, Codecs: "avc1.64001F", BaseURL: "u32avc", Bandwidth: 700},
		{ID: 80, Codecs: "hev1.1.6.L120.90", BaseURL: "u80hevc", Bandwidth: 900},
		{ID: 80, Codecs: "avc1.640028", BaseURL: "u80avc", Bandwidth: 1100},
		{ID: 80, Codecs: "av01.0.08M.08", BaseURL: "u80av1", Bandwidth: 800},
		{ID: 112, Codecs: "av01.0.12M.08", BaseURL: "u112av1", Bandwidth: 2000},
	}
	got, ok := d.pickVideo(reps)
	if !ok || got.ID != 112 {
		t.Fatalf("pickVideo chose %d/%s, want the highest quality (112)", got.ID, got.Codecs)
	}
	// Without the AV1-only rung, the AVC variant of 80 wins over HEVC.
	got, _ = d.pickVideo(reps[:4])
	if got.BaseURL != "u80avc" {
		t.Fatalf("pickVideo chose %q, want the AVC variant of the same quality", got.BaseURL)
	}
	// An explicit HEVC preference is honoured.
	dh := &BilibiliDriver{codec: "hevc"}
	got, _ = dh.pickVideo(reps[:4])
	if got.BaseURL != "u80hevc" {
		t.Fatalf("HEVC preference chose %q", got.BaseURL)
	}
	// Entries with no address at all are not candidates.
	if _, ok := d.pickVideo([]biliRep{{ID: 80, Codecs: "avc1.640028"}}); ok {
		t.Fatal("pickVideo accepted a representation with no URL")
	}
}

func TestPickAudio(t *testing.T) {
	got, ok := pickAudio([]biliRep{
		{ID: 30216, Codecs: "mp4a.40.2", BaseURL: "a64", Bandwidth: 66000},
		{ID: 30280, Codecs: "mp4a.40.2", BaseURL: "a192", Bandwidth: 192000},
		{ID: 30232, Codecs: "mp4a.40.2", BaseURL: "a132", Bandwidth: 132000},
	})
	if !ok || got.BaseURL != "a192" {
		t.Fatalf("pickAudio chose %q, want the highest bitrate", got.BaseURL)
	}
}

// A membership episode comes back as code 0 with no DASH at all, so the only
// place it can be recognised is here.
func TestCheckPlayInfoRefusesPreviewEpisode(t *testing.T) {
	if err := checkPlayInfo(&pgcPlayInfo{IsPreview: 1}); err == nil {
		t.Fatal("a preview (会员) episode must not resolve to streams")
	}
	if err := checkPlayInfo(&pgcPlayInfo{}); err == nil {
		t.Fatal("an episode with no dash must not resolve to streams")
	}
	ok := &pgcPlayInfo{Dash: &pgcDash{Video: []biliRep{{ID: 80, BaseURL: "u"}}}}
	if err := checkPlayInfo(ok); err != nil {
		t.Fatalf("a playable episode was refused: %v", err)
	}
}

// The virtual tree doubles as a media library: the scanner turns every directory
// under the library root into one show, and it deletes what it can no longer see.
// So two things have to hold — nothing below the root may be anything but a 番剧,
// and a 番剧's directory name must be the same on every scan (a renamed directory
// reads as a different show, and the old one goes, watch progress with it).
func TestTreeIsLibraryShaped(t *testing.T) {
	lists := [][]pgcIndexItem{
		{{SeasonID: 1, Title: "鬼灭之刃"}, {SeasonID: 2, Title: "间谍过家家", IndexShow: "全12话"}},
		{{SeasonID: 2, Title: "间谍过家家"}, {SeasonID: 3, Title: "Fuimos Revolución 2021"}},
	}
	got := seasonEntries("/", dedupeSeasons(lists...))
	if len(got) != 3 {
		t.Fatalf("dedupe left %d entries, want 3", len(got))
	}
	for _, e := range got {
		if !e.IsDir {
			t.Fatalf("%q is not a directory: a library would read it as an episode", e.Name)
		}
		if _, ok := seasonIDFromDir(e.Name); !ok {
			t.Fatalf("%q does not carry a season id, so it cannot be resolved back", e.Name)
		}
		// The id is the only thing besides the title: no "全12话", no "更新至3话".
		// Both change as a 番剧 airs, which would rename the directory.
		for _, unstable := range []string{"全12话", "更新至", "连载中"} {
			if strings.Contains(e.Name, unstable) {
				t.Fatalf("directory name %q carries %q, which changes between scans", e.Name, unstable)
			}
		}
	}
}

// A season directory has to survive the trip back: the name is the whole state.
func TestSeasonDirRoundTrip(t *testing.T) {
	for _, c := range []struct {
		title string
		id    int64
	}{
		{"鬼灭之刃", 26801},
		{"Fuimos Revolución 2021", 123},
		{"A/B:C", 7},
	} {
		dir := seasonDir(c.title, c.id)
		got, ok := seasonIDFromDir(dir)
		if !ok || got != c.id {
			t.Fatalf("seasonIDFromDir(%q) = %d/%v, want %d", dir, got, ok, c.id)
		}
	}
}
