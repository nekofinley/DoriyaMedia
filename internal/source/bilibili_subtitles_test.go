package source

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bilibili_subtitles_test.go: the 字幕 a 番剧 carries.
//
// The failure this is about is silent: /x/player/v2 answers code 0 with an empty
// track list when ep_id is missing, so a wrong implementation does not error — it
// reports "this episode has no subtitles" for an episode that has them. The live
// answer for 尼爾：自動人形 is the fixture below.

// biliSubtitleFixture is one /x/player/v2 answer, shaped like the live one.
// subBase is the origin the signed link points at, so a test can make it its own
// server instead of the real CDN.
func biliSubtitleFixture(epID int64, subBase string) []byte {
	if subBase == "" {
		subBase = "https://aisubtitle.hdslb.com"
	}
	body, _ := json.Marshal(map[string]any{
		"code": 0, "message": "OK",
		"data": map[string]any{
			"subtitle": map[string]any{
				"allow_submit": false,
				"lan":          "",
				"lan_doc":      "",
				"subtitles": []map[string]any{
					{
						"id":           2018135641438823424,
						"id_str":       "2018135641438823424",
						"lan":          "zh-Hant",
						"lan_doc":      "中文（繁體）",
						"is_lock":      false,
						"subtitle_url": subBase + "/bfs/subtitle/abc.json?auth_key=ep" + strconv.FormatInt(epID, 10),
						"ai_type":      0,
						"ai_status":    0,
					},
					{
						// A track with no URL is not playable and must not become an
						// empty entry in the player's menu.
						"id": 7, "id_str": "7", "lan": "zh-CN", "lan_doc": "中文（中国）",
						"subtitle_url": "",
					},
				},
			},
		},
	})
	return body
}

// biliSubtitleDoc is the document behind a subtitle_url, trimmed to two cues.
const biliSubtitleDoc = `{"body":[
 {"from":10.41,"to":14.08,"location":2,"content":"在這個腥風血雨的戰場漩渦之中"},
 {"from":14.08,"to":16.7,"location":2,"content":"我究竟要戰鬥到何時呢"}
]}`

// The list has to carry ep_id. Without it this endpoint answers code 0 with an
// empty list for a 番剧 episode — which is exactly the bug that made subtitles
// look absent, so the test asserts the parameter rather than only the result.
func TestSubtitlesAsksWithEpIDAndListsTracks(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var sawEpID, sawCid, sawAid string
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/player/wbi/v2", "/x/player/v2":
			sawEpID = r.URL.Query().Get("ep_id")
			sawCid = r.URL.Query().Get("cid")
			sawAid = r.URL.Query().Get("aid")
			// A server that only answers with a list when ep_id is present: that
			// is what bilibili does for 番剧.
			if sawEpID == "" {
				_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"subtitle":{"subtitles":[]}}}`))
				return
			}
			id, _ := strconv.ParseInt(sawEpID, 10, 64)
			_, _ = w.Write(biliSubtitleFixture(id, base))
		case "/pgc/view/web/season":
			id, _ := strconv.ParseInt(r.URL.Query().Get("ep_id"), 10, 64)
			_, _ = w.Write(biliSeasonFixture(500001, id, 600001, 700001))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	const epID = int64(725460)
	tracks, err := d.Subtitles(context.Background(), "/尼爾 [series 44508]/TV [season 44508]/第1话 [ep725460].mp4")
	if err != nil {
		t.Fatalf("Subtitles: %v", err)
	}
	if sawEpID != "725460" {
		t.Fatalf("ep_id sent = %q, want 725460 (without it the answer is empty)", sawEpID)
	}
	if sawCid != "600001" || sawAid != "700001" {
		t.Errorf("aid/cid sent = %q/%q, want 700001/600001", sawAid, sawCid)
	}
	if len(tracks) != 1 {
		t.Fatalf("%d tracks, want 1 (the one with a URL)", len(tracks))
	}
	got := tracks[0]
	if got.Path != biliSubPrefix+"2018135641438823424" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Lang != "zh-Hant" || got.Title != "中文（繁體）" {
		t.Errorf("lang/title = %q/%q", got.Lang, got.Title)
	}
	if !got.Convertible || got.Format != "subrip" {
		t.Errorf("format/convertible = %q/%v", got.Format, got.Convertible)
	}

	// A non-episode path publishes nothing and is not an error.
	if ts, err := d.Subtitles(context.Background(), "/某系列 [series 1]"); err != nil || len(ts) != 0 {
		t.Errorf("a directory path gave %d tracks, err %v", len(ts), err)
	}
}

// The track is fetched from a signed URL that carries an expiry, so it is
// re-resolved at open time rather than reused from the listing.
func TestOpenSubtitleResolvesFreshAndReturnsSRT(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var listCalls int
	var subtitleUA, subtitleCookie, subtitleReferer string
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/x/player/wbi/v2" || r.URL.Path == "/x/player/v2":
			listCalls++
			_, _ = w.Write(biliSubtitleFixture(725460, base))
		case r.URL.Path == "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		case strings.HasSuffix(r.URL.Path, "/abc.json"):
			subtitleUA = r.Header.Get("User-Agent")
			subtitleCookie = r.Header.Get("Cookie")
			subtitleReferer = r.Header.Get("Referer")
			_, _ = w.Write([]byte(biliSubtitleDoc))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{
		cookie: "SESSDATA=secret; bili_jct=csrf",
		codec:  "avc", client: srv.Client(),
	}
	rc, err := d.OpenSubtitle(context.Background(),
		"/尼爾 [series 44508]/TV [season 44508]/第1话 [ep725460].mp4",
		biliSubPrefix+"2018135641438823424")
	if err != nil {
		t.Fatalf("OpenSubtitle: %v", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	srt := string(raw)
	for _, want := range []string{
		"1\n00:00:10,410 --> 00:00:14,080\n在這個腥風血雨的戰場漩渦之中",
		"2\n00:00:14,080 --> 00:00:16,700\n我究竟要戰鬥到何時呢",
	} {
		if !strings.Contains(srt, want) {
			t.Errorf("SRT is missing %q\n---\n%s", want, srt)
		}
	}
	if listCalls < 1 {
		t.Errorf("the track list was never asked for")
	}
	// The signature on the URL is the authorisation; the account cookie must not
	// travel to the CDN.
	if subtitleCookie != "" {
		t.Errorf("the account cookie was sent to the subtitle host: %q", subtitleCookie)
	}
	if subtitleUA == "" || subtitleReferer == "" {
		t.Errorf("the CDN was called without UA/Referer: ua=%q ref=%q", subtitleUA, subtitleReferer)
	}
}

// Opening a track that no longer exists says so instead of handing back an empty
// document that a player would render as a subtitle with no lines.
func TestOpenSubtitleReportsAMissingTrack(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/player/wbi/v2", "/x/player/v2":
			_, _ = w.Write(biliSubtitleFixture(725460, ""))
		case "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	p := "/第1话 [ep725460].mp4"
	if _, err := d.OpenSubtitle(context.Background(), p, biliSubPrefix+"999"); err == nil {
		t.Fatal("a retired track id was accepted")
	}
	if _, err := d.OpenSubtitle(context.Background(), p, ""); err == nil {
		t.Fatal("an empty track id was accepted")
	}
}

// The document -> SubRip conversion is where a wrong time format or a stray blank
// line turns a subtitle into garbage, so it is tested directly.
func TestBiliSubtitleToSRT(t *testing.T) {
	srt := string(biliSubtitleToSRT([]byte(`{"body":[
	 {"from":0,"to":2.5,"content":"第一行\n第二行"},
	 {"from":3,"to":3.2,"content":"空白行\n\n在中间"},
	 {"from":4,"to":4,"content":"零长度"},
	 {"from":5,"to":6,"content":"   "},
	 {"from":1,"to":2,"content":"乱序"}
	]}`)))
	// The blank line inside a cue is dropped: SubRip would read it as the end of
	// the cue and renumber everything after it.
	if strings.Contains(srt, "\n\n\n") && strings.Contains(srt, "空白行\n\n在中间") {
		t.Errorf("a blank line survived inside a cue:\n%s", srt)
	}
	if !strings.Contains(srt, "空白行\n在中间") {
		t.Errorf("a two-part cue lost its second line:\n%s", srt)
	}
	// A zero-length cue is given the shortest readable life rather than dropped.
	if !strings.Contains(srt, "00:00:04,000 --> 00:00:04,500") {
		t.Errorf("a zero-length cue was not given a life:\n%s", srt)
	}
	// Empty text produces no cue at all.
	if strings.Contains(srt, "00:00:05,000") {
		t.Errorf("an empty cue was emitted:\n%s", srt)
	}
	// Cues are sorted by time: bilibili's body is normally ordered, but a player
	// assumes a monotonic document. "乱序" (1s) was listed last and has to come
	// before "空白行" (3s).
	if strings.Index(srt, "乱序") > strings.Index(srt, "空白行") {
		t.Errorf("cues are not in time order:\n%s", srt)
	}
	// Cue numbering is contiguous from 1.
	if !strings.HasPrefix(strings.TrimSpace(srt), "1\n") {
		t.Errorf("the document does not start at cue 1:\n%s", srt)
	}
	// A document this build cannot read yields no cues rather than JSON-as-SRT.
	if out := biliSubtitleToSRT([]byte(`not json`)); len(out) != 0 {
		t.Errorf("garbage was turned into %q", string(out))
	}
	if out := biliSubtitleToSRT([]byte(`{"body":[]}`)); len(out) != 0 {
		t.Errorf("an empty body produced %q", string(out))
	}
}

// The subtitle endpoint answers an empty list at random — measured at 7 empty
// answers out of 12 for an episode that has a subtitle. An implementation that
// trusts the first answer reports "no subtitles" for an episode that has one,
// which is the bug being fixed, so the retry is tested directly.
func TestSubtitlesRetriesTheFlakyEmptyAnswer(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var calls int
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/player/wbi/v2", "/x/player/v2":
			calls++
			// The first two answers are the spurious empty ones; the third is
			// the truth.
			if calls <= 2 {
				_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"subtitle":{"subtitles":[]}}}`))
				return
			}
			_, _ = w.Write(biliSubtitleFixture(725460, base))
		case "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	tracks, err := d.Subtitles(context.Background(), "/第1话 [ep725460].mp4")
	if err != nil {
		t.Fatalf("Subtitles: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("%d tracks after %d calls, want 1 (the empty answers are spurious)", len(tracks), calls)
	}
	if calls < 3 {
		t.Errorf("gave up after %d call(s); an empty answer is not a verdict", calls)
	}
}

// An empty answer must not be remembered for long: it is wrong about half the
// time, and a cached "no subtitles" is a subtitle that silently disappears.
func TestEmptySubtitleListIsNotCachedForLong(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var calls int
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/player/wbi/v2", "/x/player/v2":
			calls++
			// Every attempt in the first round answers empty; afterwards the
			// episode "gains" its subtitle, the way a flaky endpoint does.
			if calls <= biliSubAttempts {
				_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"subtitle":{"subtitles":[]}}}`))
				return
			}
			_, _ = w.Write(biliSubtitleFixture(725460, base))
		case "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	p := "/第1话 [ep725460].mp4"
	if tracks, err := d.Subtitles(context.Background(), p); err != nil || len(tracks) != 0 {
		t.Fatalf("first listing = %d tracks, err %v", len(tracks), err)
	}
	// The empty verdict is believed only briefly. Age the entry rather than
	// sleeping: its age is what the rule is about.
	biliSubMu.Lock()
	if it, ok := biliSubCache[725460]; ok {
		it.at = it.at.Add(-biliSubEmptyTTL - time.Second)
		biliSubCache[725460] = it
	}
	biliSubMu.Unlock()

	tracks, err := d.Subtitles(context.Background(), p)
	if err != nil {
		t.Fatalf("second listing: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("second listing = %d tracks; an empty answer was cached for too long", len(tracks))
	}
}

// A list with tracks is worth caching: the play decision and the HLS master
// playlist ask back to back.
func TestSubtitleListWithTracksIsCached(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var listCalls int
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/player/wbi/v2", "/x/player/v2":
			listCalls++
			_, _ = w.Write(biliSubtitleFixture(725460, base))
		case "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	p := "/第1话 [ep725460].mp4"
	for i := 0; i < 3; i++ {
		if tracks, err := d.Subtitles(context.Background(), p); err != nil || len(tracks) != 1 {
			t.Fatalf("listing %d = %d tracks, err %v", i, len(tracks), err)
		}
	}
	if listCalls != 1 {
		t.Errorf("the list was asked for %d times, want 1 (it is cached)", listCalls)
	}
}

// A stale signed URL must not be the thing that breaks playback: opening a track
// re-reads the list, and when that read comes back empty the cached one is used.
func TestOpenSubtitleFallsBackToTheCachedList(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var listCalls int
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/x/player/wbi/v2" || r.URL.Path == "/x/player/v2":
			listCalls++
			// The first read (discovery) is good; every later read is the
			// spurious empty answer.
			if listCalls == 1 {
				_, _ = w.Write(biliSubtitleFixture(725460, base))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"subtitle":{"subtitles":[]}}}`))
		case r.URL.Path == "/pgc/view/web/season":
			_, _ = w.Write(biliSeasonFixture(500001, 725460, 600001, 700001))
		case strings.HasSuffix(r.URL.Path, "/abc.json"):
			_, _ = w.Write([]byte(biliSubtitleDoc))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base = srv.URL
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	p := "/第1话 [ep725460].mp4"
	if tracks, err := d.Subtitles(context.Background(), p); err != nil || len(tracks) != 1 {
		t.Fatalf("discovery = %d tracks, err %v", len(tracks), err)
	}
	rc, err := d.OpenSubtitle(context.Background(), p, biliSubPrefix+"2018135641438823424")
	if err != nil {
		t.Fatalf("OpenSubtitle fell over on a spurious empty answer: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if !strings.Contains(string(raw), "在這個腥風血雨的戰場漩渦之中") {
		t.Errorf("the subtitle did not come back: %q", string(raw))
	}
}
