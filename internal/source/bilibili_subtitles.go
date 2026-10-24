package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bilibili_subtitles.go: the 字幕 a 番剧 carries.
//
// They are not files. bilibili keeps them in a per-episode answer behind a signed,
// short-lived URL, so the driver publishes them through SubtitleSource instead of
// the virtual tree: listing them as directory entries would hand out links that
// are dead by the time a viewer presses play.
//
// Three details each cost a wrong first implementation, and all three were
// measured against the live API:
//
//   - /x/player/v2 needs ep_id *on top of* aid and cid. With only aid+cid it
//     answers code 0 and an empty track list, which reads as "this episode has no
//     subtitles" — the exact symptom being fixed.
//   - /x/player/v2 is unreliable: for an episode that does have subtitles it
//     returned an empty list 7 times out of 12, with identical parameters and
//     between identical successes. Its wbi sibling answered 6 times out of 6, and
//     accepts an unsigned request, so that is the one asked first — with retries,
//     because "empty" is not yet an answer.
//   - The subtitle file itself is a signed URL and needs no credential. The
//     account cookie is deliberately not sent to it: the signature is the
//     authorisation, and a credential does not belong on a CDN.

// biliSubPrefix marks a path this driver owns. The rest is bilibili's own track
// id, which is stable across rediscoveries — a player that already loaded
// /api/subs/{file}/{id}.vtt keeps working when the list is refreshed.
const biliSubPrefix = "bili:"

const (
	// biliSubTTL is how long a list *with* tracks is reused. The play decision
	// asks once and the HLS master playlist asks again immediately after.
	biliSubTTL = 10 * time.Minute
	// biliSubEmptyTTL is how long "this episode has no subtitles" is believed.
	// It is deliberately short: that answer is wrong about half the time when it
	// comes from the unstable endpoint, and caching it for minutes is how a
	// subtitle silently disappears from an episode that has one.
	biliSubEmptyTTL = 45 * time.Second
	// biliSubAttempts bounds the retries. Three calls (~0.5s with the API pacing)
	// were enough for 9 of 10 trials in the live measurement.
	biliSubAttempts = 3
)

// SubtitlePathPrefix implements SubtitleSource.
func (d *BilibiliDriver) SubtitlePathPrefix() string { return biliSubPrefix }

// biliSubtitleTrack is one entry of the subtitle list /x/player/v2 answers with.
type biliSubtitleTrack struct {
	ID          int64  `json:"id"`
	IDStr       string `json:"id_str"`
	Lan         string `json:"lan"`
	LanDoc      string `json:"lan_doc"`
	IsLock      bool   `json:"is_lock"`
	SubtitleURL string `json:"subtitle_url"`
	// AIType / AIStatus mark machine-made tracks (AI 字幕 / AI 翻译). They are
	// real subtitles and are offered as such; the label already says so.
	AIType   int `json:"ai_type"`
	AIStatus int `json:"ai_status"`
}

// biliSubtitleAnswer is the subtitle block of /x/player/v2.
type biliSubtitleAnswer struct {
	Subtitles []biliSubtitleTrack `json:"subtitles"`
}

// biliSubtitleFile is the document behind a subtitle_url.
type biliSubtitleFile struct {
	Body []struct {
		From    float64 `json:"from"`
		To      float64 `json:"to"`
		Content string  `json:"content"`
	} `json:"body"`
}

// biliSubCache remembers one episode's subtitle list.
//
// An empty list is kept for far less time than a full one (see biliSubEmptyTTL):
// the endpoint that produces it answers empty at random, so an empty answer is a
// weak fact rather than an observation.
type biliSubCacheItem struct {
	tracks []biliSubtitleTrack
	at     time.Time
}

var (
	biliSubMu    sync.Mutex
	biliSubCache = map[int64]biliSubCacheItem{}
)

// Subtitles lists one episode's subtitle tracks.
func (d *BilibiliDriver) Subtitles(ctx context.Context, path string) ([]SubtitleTrack, error) {
	epID, ok := episodeIDFromPath(path)
	if !ok {
		// Not an episode of this source (a folder, or a path another driver
		// owns): nothing to publish, and not an error.
		return nil, nil
	}
	tracks, err := d.subtitleTracksCached(ctx, epID)
	if err != nil {
		return nil, err
	}
	out := make([]SubtitleTrack, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, SubtitleTrack{
			Path: biliSubPrefix + biliTrackKey(t),
			// The tag bilibili states is already a BCP-47-ish one ("zh-Hant",
			// "zh-CN", "en-US"), which is what the automatic choice and a
			// <track srclang> both want.
			Lang:        strings.TrimSpace(t.Lan),
			Title:       biliSubtitleLabel(t),
			Format:      "subrip",
			Convertible: strings.TrimSpace(t.SubtitleURL) != "",
		})
	}
	return out, nil
}

// biliTrackKey is the stable identity of a track: bilibili states both a 64-bit
// id and its decimal spelling, and the string is the one that survives JSON float
// coercion.
func biliTrackKey(t biliSubtitleTrack) string {
	if s := strings.TrimSpace(t.IDStr); s != "" {
		return s
	}
	return strconv.FormatInt(t.ID, 10)
}

// biliSubtitleLabel is what a viewer picks from. bilibili's own lan_doc is the
// right words ("中文（繁體）"); when it is missing the tag is the fallback.
func biliSubtitleLabel(t biliSubtitleTrack) string {
	if doc := strings.TrimSpace(t.LanDoc); doc != "" {
		return doc
	}
	if lan := strings.TrimSpace(t.Lan); lan != "" {
		return lan
	}
	return "字幕"
}

// subtitleTracksCached is Subtitles' list, reused for a while.
func (d *BilibiliDriver) subtitleTracksCached(ctx context.Context, epID int64) ([]biliSubtitleTrack, error) {
	biliSubMu.Lock()
	it, ok := biliSubCache[epID]
	biliSubMu.Unlock()
	if ok {
		ttl := biliSubTTL
		if len(it.tracks) == 0 {
			ttl = biliSubEmptyTTL
		}
		if time.Since(it.at) < ttl {
			return it.tracks, nil
		}
	}

	tracks, err := d.fetchSubtitleTracks(ctx, epID)
	if err != nil {
		// Keep serving what is already known rather than turning a flaky API into
		// a subtitle that vanished. Same rule as every other listing here.
		if ok {
			return it.tracks, nil
		}
		return nil, err
	}

	biliSubMu.Lock()
	if len(biliSubCache) > 256 {
		biliSubCache = map[int64]biliSubCacheItem{}
	}
	biliSubCache[epID] = biliSubCacheItem{tracks: tracks, at: time.Now()}
	biliSubMu.Unlock()
	return tracks, nil
}

// fetchSubtitleTracks asks for one episode's subtitle list, retrying the empty
// answers — they are spurious about half the time.
//
// The wbi endpoint is tried first and the plain one as a last resort: the plain
// one is the flaky half of the pair, but it is also the one that keeps working if
// bilibili ever starts refusing unsigned wbi calls.
func (d *BilibiliDriver) fetchSubtitleTracks(ctx context.Context, epID int64) ([]biliSubtitleTrack, error) {
	ref, err := d.episode(ctx, epID)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < biliSubAttempts; attempt++ {
		path := "/x/player/wbi/v2"
		if attempt == biliSubAttempts-1 {
			path = "/x/player/v2"
		}
		tracks, err := d.subtitleTracksOnce(ctx, path, ref, epID)
		if err == nil && len(tracks) > 0 {
			return tracks, nil
		}
		lastErr = err
	}
	// No tracks on any attempt is the ordinary case for most episodes: an answer,
	// not a failure.
	return nil, lastErr
}

// subtitleTracksOnce is one request for the subtitle list.
func (d *BilibiliDriver) subtitleTracksOnce(ctx context.Context, path string, ref biliEpisodeRef, epID int64) ([]biliSubtitleTrack, error) {
	raw, err := d.getJSON(ctx, path, url.Values{
		"aid": {strconv.FormatInt(ref.Aid, 10)},
		"cid": {strconv.FormatInt(ref.Cid, 10)},
		// ep_id is not optional: without it these endpoints answer code 0 with an
		// empty list for a 番剧 episode, which is indistinguishable from "no
		// subtitles at all".
		"ep_id": {strconv.FormatInt(epID, 10)},
	})
	if err != nil {
		return nil, err
	}
	var data struct {
		Subtitle biliSubtitleAnswer `json:"subtitle"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("bilibili: 字幕列表无法解析: %w", err)
	}
	// Only tracks that actually carry a URL are usable; a locked one with no URL
	// would render as an empty track in the player.
	out := make([]biliSubtitleTrack, 0, len(data.Subtitle.Subtitles))
	for _, t := range data.Subtitle.Subtitles {
		if strings.TrimSpace(t.SubtitleURL) == "" {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// OpenSubtitle returns one track as SubRip.
//
// The list is re-read on every call rather than taken from the cache: the URL it
// carries is signed and short-lived, and a viewer can open a subtitle long after
// the play decision that listed it. If the fresh read comes back without the
// track — the endpoint does answer empty at random — the cached list is tried
// before giving up, so a spurious empty answer does not turn into a broken
// subtitle in the middle of an episode.
func (d *BilibiliDriver) OpenSubtitle(ctx context.Context, path, trackPath string) (io.ReadCloser, error) {
	epID, ok := episodeIDFromPath(path)
	if !ok {
		return nil, fmt.Errorf("bilibili: 不是可播放的剧集路径: %s", path)
	}
	want := strings.TrimPrefix(strings.TrimSpace(trackPath), biliSubPrefix)
	if want == "" {
		return nil, fmt.Errorf("bilibili: 缺少字幕 id")
	}
	found := biliFindTrack(mustSubtitles(d, ctx, epID), want)
	if found == nil {
		biliSubMu.Lock()
		it, ok := biliSubCache[epID]
		biliSubMu.Unlock()
		if ok {
			found = biliFindTrack(it.tracks, want)
		}
	}
	if found == nil {
		return nil, fmt.Errorf("bilibili: 这一集的字幕里没有 %s(可能已被下架)", want)
	}
	raw, err := d.readSubtitleFile(ctx, found.SubtitleURL)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(biliSubtitleToSRT(raw))), nil
}

// biliFindTrack picks one track out of a list by its key.
func biliFindTrack(tracks []biliSubtitleTrack, want string) *biliSubtitleTrack {
	for i := range tracks {
		if biliTrackKey(tracks[i]) == want {
			return &tracks[i]
		}
	}
	return nil
}

// mustSubtitles is the fresh (uncached) list, ignoring a failure: the caller has
// a fallback and wants the tracks, not the reason they were missing.
func mustSubtitles(d *BilibiliDriver, ctx context.Context, epID int64) []biliSubtitleTrack {
	tracks, err := d.fetchSubtitleTracks(ctx, epID)
	if err != nil {
		return nil
	}
	return tracks
}

// readSubtitleFile fetches one signed subtitle document.
func (d *BilibiliDriver) readSubtitleFile(ctx context.Context, rawURL string) ([]byte, error) {
	u := strings.TrimSpace(rawURL)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("bilibili: 字幕地址无效: %q", u)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	// streamHeaders, not headers(): the link's own signature authorises it, and
	// the account cookie has no business on a CDN.
	for k, v := range d.streamHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := d.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 拉取字幕失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bilibili: 拉取字幕失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// biliSubtitleToSRT turns bilibili's subtitle document into SubRip.
//
// SubRip is the intermediate form on purpose: it is the one format the server's
// converter already handles well, so the WebVTT a browser receives comes out of
// one well-tested path rather than a second bespoke one. The document is
// {"body":[{"from","to","content"}…]}, times in seconds.
func biliSubtitleToSRT(raw []byte) []byte {
	var doc biliSubtitleFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not the document this build knows. Returning the bytes untouched would
		// put JSON in front of a player as if it were SubRip and render every
		// cue as garbage; an empty document is the honest failure.
		return nil
	}
	type cue struct {
		from, to float64
		text     string
	}
	cues := make([]cue, 0, len(doc.Body))
	for _, b := range doc.Body {
		text := biliCueText(b.Content)
		if text == "" {
			continue
		}
		from, to := b.From, b.To
		if from < 0 {
			continue
		}
		// A zero-length cue is invisible in every player. bilibili does publish
		// them (karaoke lines, one-frame signs); giving them the shortest
		// readable life keeps the words on screen without inventing timing.
		if to <= from {
			to = from + 0.5
		}
		cues = append(cues, cue{from: from, to: to, text: text})
	}
	if len(cues) == 0 {
		return nil
	}
	// Bilibili's body is already in time order, but files that arrive zipped
	// together do not have to be, and a player assumes a monotonic document.
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].from < cues[j].from })

	var buf bytes.Buffer
	for i, c := range cues {
		fmt.Fprintf(&buf, "%d\n%s --> %s\n%s\n\n",
			i+1, srtTime(c.from), srtTime(c.to), c.text)
	}
	return buf.Bytes()
}

// biliCueText prepares one cue's words for SubRip.
//
// A blank line inside a cue ends it, so one arriving in the text would split a
// line into two cues and shift every number after it. Line breaks are kept (a
// two-line cue is normal), blank ones are not.
func biliCueText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// srtTime formats a second offset as SubRip's "HH:MM:SS,mmm".
func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}
