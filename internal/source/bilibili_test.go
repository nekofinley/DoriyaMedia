package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	name := episodeFileName("鬼灭之刃", ep, "", 1)
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
	got := episodeFileName("某番", ep, "", 1)
	if !strings.Contains(got, "PV1") || !strings.Contains(got, "正式预告") || !strings.Contains(got, "[ep5]") {
		t.Fatalf("unexpected name for a non-numeric episode: %q", got)
	}
	// A section entry carries the section it came from as a marker, and the marker
	// has to survive the [epN] tag being last: that tag is what identifies the file.
	sec := episodeFileName("国王排名", &pgcEpisode{ID: 427417, Title: "第3话预告", LongTitle: "新国王"}, "PV&其他", 2)
	if !strings.Contains(sec, "[sp#2#PV&其他]") {
		t.Fatalf("a section entry lost its marker: %q", sec)
	}
	if !strings.HasSuffix(sec, "[ep427417].mp4") {
		t.Fatalf("a section entry lost its identifying tag: %q", sec)
	}
	// A numbered episode of the main list keeps the shape it has always had, so a
	// rescan does not move every file (and take the watch progress with it).
	if main := episodeFileName("某番", &pgcEpisode{ID: 9, Title: "3", LongTitle: "标题"}, "", 3); main != "某番 第3话 标题 [ep9].mp4" {
		t.Fatalf("a numbered episode changed shape: %q", main)
	}
	// Titles carry characters a path may not.
	dirty := episodeFileName(`A/B:C*D?E"F<G>H|I`, &pgcEpisode{ID: 1, Title: "1"}, "", 1)
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

// The ladder is walked from the top, but the configured codec comes first: that
// setting exists because a viewer knows what their machine decodes, and a higher
// rung in a codec they cannot decode is not a better answer.
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
	if !ok || got.BaseURL != "u80avc" {
		t.Fatalf("pickVideo chose %d/%s, want the AVC rung the account has (80)", got.ID, got.Codecs)
	}
	// With no AVC on offer at all the ladder decides, exactly as before.
	noAVC := []biliRep{
		{ID: 80, Codecs: "hev1.1.6.L120.90", BaseURL: "u80hevc", Bandwidth: 900},
		{ID: 112, Codecs: "av01.0.12M.08", BaseURL: "u112av1", Bandwidth: 2000},
	}
	got, _ = d.pickVideo(noAVC)
	if got.BaseURL != "u112av1" {
		t.Fatalf("pickVideo chose %q without an AVC rung, want the highest quality", got.BaseURL)
	}
	// An explicit HEVC preference is honoured, and within it the AVC variant is
	// no longer preferred.
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

// An HDR (qn 125) or Dolby Vision (qn 126) rung is not a better picture for a
// plain stream copy: without tone mapping or a DV decoder the client renders the
// same frames with the wrong luminance and colours. SDR wins even though its
// quality id is lower.
func TestPickVideoAvoidsHDR(t *testing.T) {
	d := &BilibiliDriver{codec: "auto"}
	reps := []biliRep{
		{ID: 125, Codecs: "hvc1.2.4.H150.90", BaseURL: "u125hdr", Bandwidth: 4400},
		{ID: 120, Codecs: "hev1.1.6.L153.90", BaseURL: "u120hevc", Bandwidth: 2700},
		{ID: 120, Codecs: "avc1.640033", BaseURL: "u120avc", Bandwidth: 5400},
	}
	got, _ := d.pickVideo(reps)
	if got.BaseURL == "u125hdr" {
		t.Fatal("pickVideo chose the HDR rung for a plain copy")
	}
	if got.BaseURL != "u120avc" {
		t.Fatalf("pickVideo chose %q, want the SDR AVC variant of 4K", got.BaseURL)
	}
	// Dolby Vision is recognised from the codec string even at another id.
	dv := []biliRep{
		{ID: 120, Codecs: "dvh1.08.06", BaseURL: "u120dv", Bandwidth: 3000},
		{ID: 112, Codecs: "hev1.1.6.L150.90", BaseURL: "u112hevc", Bandwidth: 900},
	}
	got, _ = d.pickVideo(dv)
	if got.BaseURL != "u112hevc" {
		t.Fatalf("pickVideo chose %q, want the SDR rung beside a Dolby Vision one", got.BaseURL)
	}
	// An HDR-only ladder still plays: it is the last resort, not a refusal.
	got, ok := d.pickVideo([]biliRep{{ID: 125, Codecs: "hvc1.2.4.H150.90", BaseURL: "u125hdr"}})
	if !ok || got.BaseURL != "u125hdr" {
		t.Fatalf("pickVideo refused an HDR-only ladder: %q/%v", got.BaseURL, ok)
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
	// A Dolby or Hi-Res track has a higher bitrate but cannot be copied into the
	// output: it would be re-encoded to 192k AAC, which is worse than handing the
	// plain 320k AAC through untouched.
	got, ok = pickAudio([]biliRep{
		{ID: 30280, Codecs: "mp4a.40.2", BaseURL: "a320", Bandwidth: 320000},
		{ID: 30250, Codecs: "ec-3", BaseURL: "dolby", Bandwidth: 448000},
		{ID: 30251, Codecs: "fLaC", BaseURL: "hires", Bandwidth: 1400000},
	})
	if !ok || got.BaseURL != "a320" {
		t.Fatalf("pickAudio chose %q, want the copyable AAC track", got.BaseURL)
	}
	// If the answer offers nothing copyable, a track still beats silence.
	got, ok = pickAudio([]biliRep{{ID: 30250, Codecs: "ec-3", BaseURL: "dolby", Bandwidth: 448000}})
	if !ok || got.BaseURL != "dolby" {
		t.Fatalf("pickAudio refused an E-AC-3-only answer: %q/%v", got.BaseURL, ok)
	}
}

// A membership episode comes back as code 0 with no DASH at all, so the only
// place it can be recognised is here. A trial clip is the same thing with a
// DASH block attached, and used to slip through: it plays, so nothing looked
// wrong, and the viewer got a few minutes of the episode instead of the episode.
func TestCheckPlayInfoRefusesPreviewEpisode(t *testing.T) {
	if err := checkPlayInfo(&pgcPlayInfo{IsPreview: 1}, 0); err == nil {
		t.Fatal("a preview (会员) episode must not resolve to streams")
	}
	if err := checkPlayInfo(&pgcPlayInfo{}, 0); err == nil {
		t.Fatal("an episode with no dash must not resolve to streams")
	}
	ok := &pgcPlayInfo{Dash: &pgcDash{Video: []biliRep{{ID: 80, BaseURL: "u"}}}}
	if err := checkPlayInfo(ok, 0); err != nil {
		t.Fatalf("a playable episode was refused: %v", err)
	}
	// A preview that carries a DASH block is still a trial, not the episode.
	trial := &pgcPlayInfo{IsPreview: 1, Dash: &pgcDash{Video: []biliRep{{ID: 16, BaseURL: "u"}}}}
	if err := checkPlayInfo(trial, 0); err == nil {
		t.Fatal("a preview with a DASH block must be refused")
	}
	// A DASH block that is minutes short of the episode is a sample, whatever the
	// flags say.
	whole := func() *pgcPlayInfo {
		return &pgcPlayInfo{Dash: &pgcDash{Video: []biliRep{{ID: 80, BaseURL: "u"}}, Duration: 1421}}
	}
	if err := checkPlayInfo(whole(), 1421000); err != nil {
		t.Fatalf("a full-length episode was refused: %v", err)
	}
	short := whole()
	short.Dash.Duration = 360 // the six-minute trial
	if err := checkPlayInfo(short, 1421000); err == nil {
		t.Fatal("a six-minute answer for a 23-minute episode must be refused")
	}
}

// ---------- region filter ----------

// resetBiliCache empties the driver's shared TTL cache. The tests below ask the
// same season ids through changing routes, and a cached verdict from an earlier
// case would answer for the wrong one.
func resetBiliCache() {
	biliCacheMu.Lock()
	biliCache = map[string]biliCacheItem{}
	biliCacheMu.Unlock()
}

// biliSeasonFixture is the shape /pgc/view/web/season answers with, reduced to
// the fields the driver reads: the id, a title, and one regular episode.
func biliSeasonFixture(seasonID, epID, cid, aid int64) []byte {
	body, err := json.Marshal(map[string]any{
		"code": 0, "message": "success",
		"result": map[string]any{
			"season_id": seasonID,
			"title":     fmt.Sprintf("番剧 %d", seasonID),
			"episodes": []map[string]any{{
				"id": epID, "cid": cid, "aid": aid,
				"title": "1", "long_title": "第一集", "duration": 1440000,
			}},
		},
	})
	if err != nil {
		// The fixture is a fixed map of strings and numbers; this cannot fail.
		panic(err)
	}
	return body
}

// The scan is what this behaviour exists for, so the test watches the listing
// the walker actually reads: a season that playurl refuses must come back
// empty (which is what keeps it out of a library and removes stale rows), a
// playable one must come back whole, and a failure that is *not* a region
// verdict must not hide anything.
func TestPlayableOnlySkipsRegionLockedSeason(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pgc/view/web/season":
			id, _ := strconv.ParseInt(r.URL.Query().Get("season_id"), 10, 64)
			ep, cid, aid := id*10+1, id*10+2, id*10+3
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(biliSeasonFixture(id, ep, cid, aid))
		case "/pgc/player/web/playurl":
			// The probe episode of the blocked season is the only one bilibili
			// refuses; 900003's answer is a server failure, not a verdict.
			switch r.URL.Query().Get("ep_id") {
			case "9000011":
				_, _ = w.Write([]byte(`{"code":-10403,"message":"抱歉您所在地区不可观看！"}`))
			case "9000031":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				_, _ = w.Write([]byte(`{"code":0,"message":"success","result":{}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	ctx := context.Background()
	d := &BilibiliDriver{codec: "avc", client: srv.Client(), playableOnly: true}

	eps, err := d.episodeEntries(ctx, "/挡住 [series 900001]/看不见 [season 900001]", 900001)
	if err != nil {
		t.Fatalf("a region-locked season must list as empty, not fail: %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("a region-locked season listed %d episodes, want none", len(eps))
	}

	eps, err = d.episodeEntries(ctx, "/能看 [series 900002]/看得见 [season 900002]", 900002)
	if err != nil || len(eps) != 1 {
		t.Fatalf("a playable season listed %d episodes (err=%v), want 1", len(eps), err)
	}

	// A 500 from the playurl probe is unknown, not "unplayable". The season has
	// to stay listed: hiding it would delete a library's rows for a transient
	// failure.
	eps, err = d.episodeEntries(ctx, "/风控 [series 900003]/未知 [season 900003]", 900003)
	if err != nil || len(eps) != 1 {
		t.Fatalf("an unknown probe failure listed %d episodes (err=%v), want the season kept", len(eps), err)
	}

	// With the option off the same blocked season is listed exactly as before.
	d.playableOnly = false
	eps, err = d.episodeEntries(ctx, "/挡住 [series 900001]/看不见 [season 900001]", 900001)
	if err != nil || len(eps) != 1 {
		t.Fatalf("with the filter off the blocked season listed %d episodes (err=%v), want 1", len(eps), err)
	}
}

func TestProbeEpisodePrefersRegularThenSection(t *testing.T) {
	if got := probeEpisode(&pgcSeason{Episodes: []pgcEpisode{
		{ID: 0, Cid: 1}, {ID: 5, Cid: 6, Aid: 7},
	}}); got == nil || got.ID != 5 {
		t.Fatalf("probeEpisode picked %+v, want the first episode with an id and cid", got)
	}
	sectionOnly := &pgcSeason{Section: []pgcSection{{
		Title:    "PV",
		Episodes: []pgcEpisode{{ID: 9, Cid: 10}},
	}}}
	if got := probeEpisode(sectionOnly); got == nil || got.ID != 9 {
		t.Fatalf("probeEpisode ignored a section-only season: %+v", got)
	}
	if probeEpisode(&pgcSeason{}) != nil {
		t.Fatal("probeEpisode invented an episode for an empty season")
	}
}

func TestBiliErrorMarksRegionLocked(t *testing.T) {
	if err := biliError(-10403, ""); !errors.Is(err, errRegionLocked) {
		t.Fatalf("-10403 did not classify as region-locked: %v", err)
	}
	if err := biliError(-404, ""); errors.Is(err, errRegionLocked) {
		t.Fatalf("-404 classified as region-locked: %v", err)
	}
	if err := biliError(-352, ""); errors.Is(err, errRegionLocked) {
		t.Fatalf("-352 classified as region-locked: %v", err)
	}
}

// A season's title is not the series' title, and bilibili states only the first.
// Every season of a series has to compute the same answer, or the same series
// found through the hot index and through 我的追番 lands in two directories —
// which the scanner would read as two shows.
func TestFranchiseTitle(t *testing.T) {
	cases := []struct{ title, seasonTitle, want string }{
		{"鬼灭之刃", "立志篇", "鬼灭之刃"},             // first season: nothing to strip
		{"鬼灭之刃 柱训练篇", "柱训练篇", "鬼灭之刃"},       // a sequel names its own season
		{"鬼灭之刃 游郭篇 中配版", "游郭篇 中配版", "鬼灭之刃"}, // ...even a dubbed one
		{"间谍过家家 第二季", "第二季", "间谍过家家"},
		{"间谍过家家 第一季(粤配)", "第一季(粤配)", "间谍过家家"},
		{"冰菓", "冰菓", "冰菓"},     // season_title == title: keep it
		{"某番", "", "某番"},       // nothing to go on
		{"游郭篇", "柱训练篇", "游郭篇"}, // not a suffix: leave it alone
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
