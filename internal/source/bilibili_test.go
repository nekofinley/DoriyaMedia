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
	path := VirtualJoin("/热门番剧",
		VirtualJoin(seriesDir("鬼灭之刃", 26801),
			VirtualJoin(seasonDirOf("立志篇", 26801), name)))

	if id, ok := episodeIDFromPath(path); !ok || id != 267851 {
		t.Fatalf("episodeIDFromPath(%q) = %d/%v, want 267851", path, id, ok)
	}
	seasonPath := path[:strings.LastIndex(path, "/")]
	if id, ok := seasonKeyFromDir(lastSegment(seasonPath)); !ok || id != 26801 {
		t.Fatalf("seasonKeyFromDir(%q) = %d/%v, want 26801", seasonPath, id, ok)
	}
	seriesPath := seasonPath[:strings.LastIndex(seasonPath, "/")]
	if id, ok := seriesKeyFromDir(lastSegment(seriesPath)); !ok || id != 26801 {
		t.Fatalf("seriesKeyFromDir(%q) = %d/%v, want 26801", seriesPath, id, ok)
	}
	// The two kinds of directory must never be mistaken for one another, and a
	// file must never look like either: the dispatch in List() is exactly this.
	if _, ok := seriesKeyFromDir(lastSegment(seasonPath)); ok {
		t.Fatalf("a season directory %q also parsed as a series", seasonPath)
	}
	if _, ok := seasonKeyFromDir(lastSegment(path)); ok {
		t.Fatalf("file path %q parsed as a season directory", path)
	}
	if _, ok := seriesKeyFromDir(lastSegment(path)); ok {
		t.Fatalf("file path %q parsed as a series directory", path)
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

// A season's title is not the series' title, and bilibili states only the first.
// Every season of a series has to compute the same answer, or the same series
// found through the hot index and through 我的追番 lands in two directories —
// which the scanner would read as two shows.
func TestFranchiseTitle(t *testing.T) {
	cases := []struct{ title, seasonTitle, want string }{
		{"鬼灭之刃", "立志篇", "鬼灭之刃"},              // first season: nothing to strip
		{"鬼灭之刃 柱训练篇", "柱训练篇", "鬼灭之刃"},        // a sequel names its own season
		{"鬼灭之刃 游郭篇 中配版", "游郭篇 中配版", "鬼灭之刃"},  // ...even a dubbed one
		{"间谍过家家 第二季", "第二季", "间谍过家家"},
		{"间谍过家家 第一季(粤配)", "第一季(粤配)", "间谍过家家"},
		{"冰菓", "冰菓", "冰菓"},                   // season_title == title: keep it
		{"某番", "", "某番"},                     // nothing to go on
		{"游郭篇", "柱训练篇", "游郭篇"},               // not a suffix: leave it alone
	}
	for _, c := range cases {
		if got := franchiseTitle(c.title, c.seasonTitle); got != c.want {
			t.Errorf("franchiseTitle(%q, %q) = %q, want %q", c.title, c.seasonTitle, got, c.want)
		}
	}
}

// Directory names double as library identities: they carry the id that resolves
// them, and nothing that changes while a 番剧 airs.
func TestTreeNamesAreStableAndResolvable(t *testing.T) {
	names := []string{
		seriesDir("鬼灭之刃", 26801),
		seriesDir("间谍过家家", 41410),
		seasonDirOf("第一季", 41410),
		seasonDirOf("柱训练篇", 47836),
		seasonDirOf("游郭篇 中配版", 45624),
	}
	for _, n := range names {
		for _, unstable := range []string{"全12话", "更新至", "连载中", "已完结"} {
			if strings.Contains(n, unstable) {
				t.Fatalf("directory name %q carries %q, which changes between scans", n, unstable)
			}
		}
	}
	if _, ok := seriesKeyFromDir(names[0]); !ok {
		t.Fatalf("%q does not resolve back to a series id", names[0])
	}
	if _, ok := seasonKeyFromDir(names[2]); !ok {
		t.Fatalf("%q does not resolve back to a season id", names[2])
	}
	// Sanitising must not break the bracket the id lives in.
	if got := seasonDirOf("A/B:C", 7); !strings.HasSuffix(got, "[season 7]") {
		t.Fatalf("sanitised season directory = %q", got)
	}
}
