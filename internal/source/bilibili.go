package source

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bilibili.go: bilibili's licensed anime (番剧 / PGC) as a storage source.
//
// Two things make this source different from every other one here, and both are
// why it is not built out of .strm files:
//
//   - The CDN refuses a request without `Referer: https://www.bilibili.com/`
//     (403, measured), so no client can fetch a stream itself. Every byte goes
//     through this server, and the driver therefore reports HasDirect() == false.
//   - bilibili serves DASH: the picture and the sound are two separate URLs that
//     have to be muxed. The driver implements MultiPart so the playback engine
//     reads both, and states the codecs itself — the playurl API already knows
//     them, so no ffprobe pass is needed (and none could work: one pass reads
//     one URL).
//
// URLs carry a `deadline` about two hours out, so they are re-resolved per play
// rather than stored; see the engine's resolve cache.
//
// 弹幕 are a plain extra: Comments() returns the bullet comments published
// against an episode's cid.
const (
	biliAPI     = "https://api.bilibili.com"
	biliReferer = "https://www.bilibili.com/"
	biliUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	// biliMaxFollowPages bounds the 我的追番 listing. A follow list can be
	// thousands long and the file browser has no paging; walking all of it on
	// every directory listing is a good way to meet the risk control.
	biliMaxFollowPages = 4
	// biliMaxCommentsPerFile bounds one episode's bullet comments.
	biliMaxCommentsPerFile = 20000
)

// BilibiliDriver lists 番剧 as a virtual tree and plays episodes through the
// engine's multi-input remux.
type BilibiliDriver struct {
	cookie  string
	quality int    // preferred qn; 0 = highest the account may have
	codec   string // avc | hevc | av01 | auto
}

func NewBilibili(cookie string, quality int, codec string) (*BilibiliDriver, error) {
	switch codec {
	case "", "auto", "avc", "h264", "hevc", "h265", "av1", "av01":
	default:
		return nil, fmt.Errorf("bilibili: 未知编码 %q(可选 avc/hevc/av1/auto)", codec)
	}
	if codec == "" {
		codec = "avc"
	}
	return &BilibiliDriver{cookie: strings.TrimSpace(cookie), quality: quality, codec: codec}, nil
}

// ---------- HTTP ----------

type biliResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Result  json.RawMessage `json:"result"`
}

// payload returns whichever envelope this endpoint used: the /pgc/ family
// answers in `result`, everything else in `data`.
func (r *biliResp) payload() json.RawMessage {
	if len(r.Result) > 0 && string(r.Result) != "null" {
		return r.Result
	}
	return r.Data
}

func (d *BilibiliDriver) headers() map[string]string {
	h := map[string]string{
		"Referer":    biliReferer,
		"User-Agent": biliUA,
		"Origin":     "https://www.bilibili.com",
	}
	if d.cookie != "" {
		h["Cookie"] = d.cookie
	}
	return h
}

// streamHeaders is what the CDN needs. The account cookie is deliberately not
// sent here: the link's own signature is the authorisation, and a credential
// does not belong on a third-party host.
func (d *BilibiliDriver) streamHeaders() map[string]string {
	return map[string]string{
		"Referer":    biliReferer,
		"User-Agent": biliUA,
	}
}

// biliMinInterval keeps two API calls apart. A scan lists one directory per
// 番剧 — a hundred calls in a burst, with four directories in flight at once —
// and a burst is exactly what the risk control (-352) looks for. Answering a
// scan a few seconds later costs nothing; a listing that comes back empty costs
// a library, because the scanner deletes what it can no longer see.
const biliMinInterval = 150 * time.Millisecond

var (
	biliRateMu   sync.Mutex
	biliLastCall time.Time
)

// biliWaitTurn paces the calls made through getJSON.
func biliWaitTurn() {
	biliRateMu.Lock()
	defer biliRateMu.Unlock()
	if wait := biliMinInterval - time.Since(biliLastCall); wait > 0 {
		time.Sleep(wait)
	}
	biliLastCall = time.Now()
}

// getJSON calls one API endpoint and unwraps the envelope.
func (d *BilibiliDriver) getJSON(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	biliWaitTurn()
	u := biliAPI + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range d.headers() {
		req.Header.Set(k, v)
	}
	resp, err := httpDo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bilibili: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bilibili: %s: HTTP %d", path, resp.StatusCode)
	}
	var r biliResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("bilibili: %s: 响应无法解析: %w", path, err)
	}
	if r.Code != 0 {
		return nil, biliError(r.Code, r.Message)
	}
	return r.payload(), nil
}

// biliError turns the codes a viewer can actually meet into something worth
// reading. Everything else keeps bilibili's own wording.
func biliError(code int, msg string) error {
	switch code {
	case -101:
		return fmt.Errorf("bilibili: 账号未登录(Cookie 失效或没填)——登录后才能看会员集和高清")
	case -10403:
		return fmt.Errorf("bilibili: 该内容在当前地区不可观看")
	case -404:
		return fmt.Errorf("bilibili: 内容不存在或已下架")
	case 87007, 87008:
		return fmt.Errorf("bilibili: 该集为大会员专享(App 专享),当前账号看不了")
	case -352, -412:
		return fmt.Errorf("bilibili: 请求被风控拦截(-%d),稍后再试;频繁请求会加重限制", -code)
	}
	if msg == "" {
		msg = "未知错误"
	}
	return fmt.Errorf("bilibili: %s(%d)", msg, code)
}

// ---------- small TTL cache ----------
//
// Listings and episode lists are read far more often than they change: the file
// browser, the subtitle scan and the play decision all list the same directory
// within seconds of each other. Without this the same API call would go out
// several times per playback, which is how a source gets rate-limited — and a
// rate-limited List is worse than a slow one, because the scanner deletes what
// it cannot see. Stale-but-present is the right trade here.

type biliCacheItem struct {
	val any
	at  time.Time
}

var (
	biliCacheMu sync.Mutex
	biliCache   = map[string]biliCacheItem{}
)

const biliCacheMax = 256

func biliCached(key string, ttl time.Duration, fetch func() (any, error)) (any, error) {
	biliCacheMu.Lock()
	if it, ok := biliCache[key]; ok && time.Since(it.at) < ttl {
		biliCacheMu.Unlock()
		return it.val, nil
	}
	biliCacheMu.Unlock()

	val, err := fetch()
	if err != nil {
		// Keep serving whatever is there rather than failing: a stale listing
		// beats a directory that suddenly looks empty.
		biliCacheMu.Lock()
		it, ok := biliCache[key]
		biliCacheMu.Unlock()
		if ok {
			return it.val, nil
		}
		return nil, err
	}
	biliCacheMu.Lock()
	if len(biliCache) > biliCacheMax {
		for k := range biliCache {
			delete(biliCache, k)
			if len(biliCache) <= biliCacheMax/2 {
				break
			}
		}
	}
	biliCache[key] = biliCacheItem{val: val, at: time.Now()}
	biliCacheMu.Unlock()
	return val, nil
}

// ---------- API shapes ----------

type pgcEpisode struct {
	ID        int64  `json:"id"` // ep_id
	Aid       int64  `json:"aid"`
	Cid       int64  `json:"cid"`
	Title     string `json:"title"` // the episode number as a string: "1", "2", "PV1"
	LongTitle string `json:"long_title"`
	Duration  int64  `json:"duration"` // ms
	Badge     string `json:"badge"`    // "会员" marks an episode only a VIP can watch
	Status    int    `json:"status"`
	PubTime   int64  `json:"pub_time"`
}

type pgcSection struct {
	Title    string       `json:"title"`
	Episodes []pgcEpisode `json:"episodes"`
}

type pgcSeason struct {
	SeasonID int64        `json:"season_id"`
	Title    string       `json:"title"`
	Evaluate string       `json:"evaluate"`
	Total    int          `json:"total"`
	Episodes []pgcEpisode `json:"episodes"`
	Section  []pgcSection `json:"section"`
	Rating   struct {
		Score float64 `json:"score"`
	} `json:"rating"`
}

type pgcIndexItem struct {
	SeasonID   int64  `json:"season_id"`
	Title      string `json:"title"`
	IndexShow  string `json:"index_show"`
	SeasonType int    `json:"season_type"`
}

type biliRep struct {
	ID        int      `json:"id"` // quality code: 16/32/64/80/112/120…
	BaseURL   string   `json:"baseUrl"`
	BackupURL []string `json:"backupUrl"`
	Bandwidth int      `json:"bandwidth"`
	MimeType  string   `json:"mimeType"`
	Codecs    string   `json:"codecs"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
}

// pgcDash is the DASH manifest of one episode: its length in seconds and the
// representations the account is allowed to have.
type pgcDash struct {
	Duration int       `json:"duration"`
	Video    []biliRep `json:"video"`
	Audio    []biliRep `json:"audio"`
}

type pgcPlayInfo struct {
	Quality   int      `json:"quality"`
	IsPreview int      `json:"is_preview"`
	TimeLen   int64    `json:"timelength"` // ms
	Dash      *pgcDash `json:"dash"`
}

// ---------- virtual tree ----------

// Season directories carry their id in brackets. That keeps the tree stateless:
// a path is all the driver needs to resolve an episode again after a restart,
// and the bracket is dropped by the scanner's name parser, so it never reaches
// the UI as part of a title.
var reSeasonDir = regexp.MustCompile(`\[(\d+)\]$`)
var reEpisodeFile = regexp.MustCompile(`\[ep(\d+)\]\.mp4$`)

// dirtyPathChars are the ones a title may contain and a path may not.
var dirtyPathChars = strings.NewReplacer(
	"/", "_", "\\", "_", ":", "：", "*", "_", "?", "？",
	"\"", "'", "<", "《", ">", "》", "|", "_", "\n", " ", "\r", " ", "\t", " ",
)

func biliClean(s string) string {
	s = dirtyPathChars.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 90 {
		s = strings.TrimSpace(s[:90])
	}
	if s == "" {
		s = "未命名"
	}
	return s
}

// seasonDir is the directory name of one 番剧. The id in brackets is what makes
// the tree stateless: a path alone is enough to resolve the season again.
func seasonDir(title string, seasonID int64) string {
	return fmt.Sprintf("%s [%d]", biliClean(title), seasonID)
}

func episodeFileName(season string, ep *pgcEpisode, sectionTitle string) string {
	num := strings.TrimSpace(ep.Title)
	name := biliClean(season)
	switch {
	case sectionTitle != "":
		name += " " + biliClean(sectionTitle+num)
	case num != "":
		if _, err := strconv.Atoi(num); err == nil {
			name += " 第" + num + "话"
		} else {
			name += " " + biliClean(num)
		}
	}
	if lt := strings.TrimSpace(ep.LongTitle); lt != "" {
		name += " " + biliClean(lt)
	}
	return fmt.Sprintf("%s [ep%d].mp4", name, ep.ID)
}

func lastSegment(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func seasonIDFromDir(name string) (int64, bool) {
	m := reSeasonDir.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func episodeIDFromPath(p string) (int64, bool) {
	m := reEpisodeFile.FindStringSubmatch(p)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// List renders the virtual tree.
func (d *BilibiliDriver) List(ctx context.Context, dir string) ([]Entry, error) {
	if dir == "" {
		dir = "/"
	}
	// Any directory whose own name carries a season id lists that season's
	// episodes, wherever it hangs: 最近更新/10-19/…, 我的追番/…, 热门番剧/….
	if id, ok := seasonIDFromDir(lastSegment(dir)); ok {
		return d.episodeEntries(ctx, dir, id)
	}
	switch strings.TrimSuffix(dir, "/") {
	case "", "/":
		return d.rootEntries(ctx)
	case "/热门番剧":
		return d.hotEntries(ctx)
	case "/我的追番":
		return d.followEntries(ctx)
	}
	return nil, fmt.Errorf("bilibili: 未知目录 %s", dir)
}

// rootEntries is a flat list of 番剧: what the account follows, then what is
// airing, then the most followed.
//
// Flat on purpose. This tree is also what a 媒体库 pointed at this source scans,
// and the scanner turns every directory under the library root into one show — so
// a structural directory (「最近更新」, a day, a category) would become a show of
// its own and bury the 番剧 under it. Everything below the root is therefore a
// 番剧 and nothing else; the groupings that are worth having live at their own
// paths (/热门番剧, /我的追番), which a library can also point at directly.
func (d *BilibiliDriver) rootEntries(ctx context.Context) ([]Entry, error) {
	var lists [][]pgcIndexItem
	// 我的追番 needs an account; a visitor just gets the public lists.
	if d.cookie != "" {
		if f, err := d.followSeasons(ctx); err == nil {
			lists = append(lists, f)
		}
	}
	if t, err := d.timelineSeasons(ctx); err == nil {
		lists = append(lists, t)
	}
	if h, err := d.hotSeasons(ctx); err == nil {
		lists = append(lists, h)
	}
	if len(lists) == 0 {
		return nil, fmt.Errorf("bilibili: 番剧列表获取失败(网络或风控)")
	}
	return seasonEntries("/", dedupeSeasons(lists...)), nil
}

func (d *BilibiliDriver) hotEntries(ctx context.Context) ([]Entry, error) {
	hot, err := d.hotSeasons(ctx)
	if err != nil {
		return nil, err
	}
	return seasonEntries("/热门番剧", hot), nil
}

func (d *BilibiliDriver) followEntries(ctx context.Context) ([]Entry, error) {
	if d.cookie == "" {
		return nil, fmt.Errorf("bilibili: 我的追番需要先填写账号 Cookie")
	}
	f, err := d.followSeasons(ctx)
	if err != nil {
		return nil, err
	}
	return seasonEntries("/我的追番", f), nil
}

// seasonEntry names one 番剧 directory. The name is canonical — the 番剧's own
// title and its id, never a decoration like "更新至12话" — because a directory
// that gets renamed between two scans reads to the scanner as a different show:
// the old one is deleted and its watch progress with it.
func seasonEntry(parent, title string, seasonID int64, mtime int64) Entry {
	name := seasonDir(title, seasonID)
	return Entry{Name: name, Path: VirtualJoin(parent, name), IsDir: true, MTime: mtime}
}

func seasonEntries(parent string, list []pgcIndexItem) []Entry {
	out := make([]Entry, 0, len(list))
	for _, it := range list {
		if it.SeasonID == 0 || strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, seasonEntry(parent, it.Title, it.SeasonID, 0))
	}
	return out
}

// dedupeSeasons keeps the first occurrence of each 番剧, so one that is both
// followed and airing is a single directory rather than two.
func dedupeSeasons(lists ...[]pgcIndexItem) []pgcIndexItem {
	seen := map[int64]bool{}
	var out []pgcIndexItem
	for _, l := range lists {
		for _, it := range l {
			if it.SeasonID == 0 || seen[it.SeasonID] {
				continue
			}
			seen[it.SeasonID] = true
			out = append(out, it)
		}
	}
	return out
}

// ---------- listings ----------

type timelineDay struct {
	Date     string `json:"date"`
	Episodes []struct {
		SeasonID  int64  `json:"season_id"`
		EpisodeID int64  `json:"episode_id"`
		Title     string `json:"title"`
		PubIndex  string `json:"pub_index"`
		PubTime   int64  `json:"pub_ts"`
	} `json:"episodes"`
}

// timelineSeasons is what is airing over the next few days, as 番剧. The API
// speaks in episodes; a directory in this tree means a whole 番剧, so the
// episodes are folded into their seasons (and the day boundaries dropped, since
// a directory per day is exactly the shape a library cannot use).
func (d *BilibiliDriver) timelineSeasons(ctx context.Context) ([]pgcIndexItem, error) {
	v, err := biliCached("timeline", 10*time.Minute, func() (any, error) {
		raw, err := d.getJSON(ctx, "/pgc/web/timeline", url.Values{
			"types": {"1"}, "before": {"0"}, "after": {"6"},
		})
		if err != nil {
			return nil, err
		}
		var days []timelineDay
		if err := json.Unmarshal(raw, &days); err != nil {
			return nil, err
		}
		seen := map[int64]bool{}
		var out []pgcIndexItem
		for _, day := range days {
			for _, ep := range day.Episodes {
				if ep.SeasonID == 0 || seen[ep.SeasonID] || strings.TrimSpace(ep.Title) == "" {
					continue
				}
				seen[ep.SeasonID] = true
				out = append(out, pgcIndexItem{SeasonID: ep.SeasonID, Title: ep.Title})
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]pgcIndexItem), nil
}

// hotSeasons is the most-followed 番剧.
func (d *BilibiliDriver) hotSeasons(ctx context.Context) ([]pgcIndexItem, error) {
	const page, size = 1, 60
	key := fmt.Sprintf("index:%d:%d", page, size)
	v, err := biliCached(key, 30*time.Minute, func() (any, error) {
		raw, err := d.getJSON(ctx, "/pgc/season/index/result", url.Values{
			"season_type": {"1"}, "page": {strconv.Itoa(page)}, "pagesize": {strconv.Itoa(size)},
			"order": {"3"}, "st": {"1"}, "sort": {"0"}, "type": {"1"},
			"area": {"-1"}, "year": {"-1"}, "version": {"1"},
		})
		if err != nil {
			return nil, err
		}
		var res struct {
			List []pgcIndexItem `json:"list"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		return res.List, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]pgcIndexItem), nil
}

// followSeasons lists the account's followed 番剧.
func (d *BilibiliDriver) followSeasons(ctx context.Context) ([]pgcIndexItem, error) {
	v, err := biliCached("follows", 30*time.Minute, func() (any, error) {
		mid, err := d.myMid(ctx)
		if err != nil {
			return nil, err
		}
		var all []pgcIndexItem
		for page := 1; page <= biliMaxFollowPages; page++ {
			raw, err := d.getJSON(ctx, "/x/space/bangumi/follow/list", url.Values{
				"type": {"1"}, "follow_status": {"0"},
				"pn": {strconv.Itoa(page)}, "ps": {"30"}, "vmid": {strconv.FormatInt(mid, 10)},
			})
			if err != nil {
				if page == 1 {
					return nil, err
				}
				break
			}
			var res struct {
				List []pgcIndexItem `json:"list"`
				Total  int          `json:"total"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, err
			}
			if len(res.List) == 0 {
				break
			}
			all = append(all, res.List...)
			if res.Total > 0 && len(all) >= res.Total {
				break
			}
		}
		return all, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]pgcIndexItem), nil
}

// episodeEntries lists one season's episodes and its extra sections. Paths are
// built under the directory that was actually listed, so the same 番剧 reached
// through two paths (the root and /热门番剧) still resolves to its own episodes.
func (d *BilibiliDriver) episodeEntries(ctx context.Context, dir string, seasonID int64) ([]Entry, error) {
	s, err := d.season(ctx, seasonID)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(s.Episodes)+4)
	add := func(ep *pgcEpisode, section string) {
		name := episodeFileName(s.Title, ep, section)
		out = append(out, Entry{
			Name: name, Path: VirtualJoin(dir, name),
			IsDir: false, MTime: ep.PubTime,
		})
	}
	for i := range s.Episodes {
		add(&s.Episodes[i], "")
	}
	for _, sec := range s.Section {
		for i := range sec.Episodes {
			add(&sec.Episodes[i], sec.Title)
		}
	}
	return out, nil
}

// ---------- resolution ----------

func (d *BilibiliDriver) season(ctx context.Context, seasonID int64) (*pgcSeason, error) {
	key := fmt.Sprintf("season:%d", seasonID)
	v, err := biliCached(key, 10*time.Minute, func() (any, error) {
		raw, err := d.getJSON(ctx, "/pgc/view/web/season", url.Values{
			"season_id": {strconv.FormatInt(seasonID, 10)},
		})
		if err != nil {
			return nil, err
		}
		var s pgcSeason
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		// Remember the episode ids this season holds, so an episode path can be
		// resolved without asking again.
		biliCacheMu.Lock()
		for _, ep := range s.Episodes {
			biliCache[fmt.Sprintf("epid:%d", ep.ID)] = biliCacheItem{
				val: biliEpisodeRef{SeasonID: s.SeasonID, Cid: ep.Cid, Aid: ep.Aid, Duration: ep.Duration},
				at:  time.Now(),
			}
		}
		biliCacheMu.Unlock()
		return &s, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*pgcSeason), nil
}

// biliEpisodeRef is what a play needs to know about one episode.
type biliEpisodeRef struct {
	SeasonID int64
	Cid      int64
	Aid      int64
	Duration int64 // ms
}

// episode resolves an ep_id into the ids playurl wants. Browsing a season has
// usually already recorded them; a cold start (or a restart) asks bilibili.
func (d *BilibiliDriver) episode(ctx context.Context, epID int64) (biliEpisodeRef, error) {
	biliCacheMu.Lock()
	it, ok := biliCache[fmt.Sprintf("epid:%d", epID)]
	biliCacheMu.Unlock()
	if ok {
		if ref, ok := it.val.(biliEpisodeRef); ok && ref.Cid != 0 {
			return ref, nil
		}
	}
	raw, err := d.getJSON(ctx, "/pgc/view/web/season", url.Values{
		"ep_id": {strconv.FormatInt(epID, 10)},
	})
	if err != nil {
		return biliEpisodeRef{}, err
	}
	var s pgcSeason
	if err := json.Unmarshal(raw, &s); err != nil {
		return biliEpisodeRef{}, err
	}
	for _, ep := range s.Episodes {
		if ep.ID == epID {
			ref := biliEpisodeRef{SeasonID: s.SeasonID, Cid: ep.Cid, Aid: ep.Aid, Duration: ep.Duration}
			biliCacheMu.Lock()
			biliCache[fmt.Sprintf("epid:%d", epID)] = biliCacheItem{val: ref, at: time.Now()}
			biliCacheMu.Unlock()
			return ref, nil
		}
	}
	for _, sec := range s.Section {
		for _, ep := range sec.Episodes {
			if ep.ID == epID {
				ref := biliEpisodeRef{SeasonID: s.SeasonID, Cid: ep.Cid, Aid: ep.Aid, Duration: ep.Duration}
				biliCacheMu.Lock()
				biliCache[fmt.Sprintf("epid:%d", epID)] = biliCacheItem{val: ref, at: time.Now()}
				biliCacheMu.Unlock()
				return ref, nil
			}
		}
	}
	return biliEpisodeRef{}, fmt.Errorf("bilibili: 找不到第 %d 集(可能已下架)", epID)
}

func (d *BilibiliDriver) myMid(ctx context.Context) (int64, error) {
	v, err := biliCached("nav", 30*time.Minute, func() (any, error) {
		raw, err := d.getJSON(ctx, "/x/web-interface/nav", nil)
		if err != nil {
			return nil, err
		}
		var nav struct {
			IsLogin bool   `json:"isLogin"`
			Mid     int64  `json:"mid"`
			Uname   string `json:"uname"`
		}
		if err := json.Unmarshal(raw, &nav); err != nil {
			return nil, err
		}
		if !nav.IsLogin {
			return nil, fmt.Errorf("bilibili: Cookie 无效或已过期,请重新登录后复制")
		}
		return nav.Mid, nil
	})
	if err != nil {
		return 0, err
	}
	mid, ok := v.(int64)
	if !ok || mid == 0 {
		return 0, fmt.Errorf("bilibili: 无法取得账号 UID")
	}
	return mid, nil
}

// playInfo asks for one episode's streams.
func (d *BilibiliDriver) playInfo(ctx context.Context, ref biliEpisodeRef, epID int64) (*pgcPlayInfo, error) {
	qn := d.quality
	if qn <= 0 {
		qn = 127 // 8K; bilibili hands back the best the account is entitled to
	}
	raw, err := d.getJSON(ctx, "/pgc/player/web/playurl", url.Values{
		"ep_id": {strconv.FormatInt(epID, 10)},
		"cid":   {strconv.FormatInt(ref.Cid, 10)},
		"avid":  {strconv.FormatInt(ref.Aid, 10)},
		"qn":    {strconv.Itoa(qn)},
		"fnval": {"4048"}, // DASH + 4K + HDR + Dolby, and every codec bilibili has
		"fnver": {"0"}, "fourk": {"1"}, "otype": {"json"},
		"session": {""}, "module": {"bangumi"},
	})
	if err != nil {
		return nil, err
	}
	var info pgcPlayInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	if err := checkPlayInfo(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// checkPlayInfo rejects the shapes that carry no streams. A membership episode
// is the one that matters: for an account without a membership bilibili answers
// code 0, is_preview 1 and no DASH at all. That is not an error the API reports,
// so it has to be recognised here or the viewer gets a player that never starts.
func checkPlayInfo(info *pgcPlayInfo) error {
	if info.Dash != nil && len(info.Dash.Video) > 0 {
		return nil
	}
	if info.IsPreview == 1 {
		return fmt.Errorf("bilibili: 这一集需要大会员(当前账号只能试看)")
	}
	return fmt.Errorf("bilibili: 这一集没有可播放的流(可能已下架或需要大会员)")
}

// ---------- codec mapping ----------

// avcProfiles maps the profile byte in an RFC 6381 avc1 codec string to the
// profile name ffprobe would report. The playback decision reads it to spot
// 10-bit H.264, which no consumer hardware decodes.
var avcProfiles = map[string]string{
	"42": "Baseline", "4d": "Main", "58": "Extended",
	"64": "High", "6e": "High 10", "7a": "High 4:4:4",
	"f4": "High 4:4:4 Predictive",
}

// codecOf turns a DASH codecs string into (ffprobe codec, codec_tag_string,
// profile) — the three things the decision engine looks at.
func codecOf(codecs string) (name, tag, profile string) {
	c := strings.ToLower(strings.TrimSpace(codecs))
	switch {
	case strings.HasPrefix(c, "avc1"), strings.HasPrefix(c, "avc3"):
		profile = "High"
		if parts := strings.SplitN(c, ".", 2); len(parts) == 2 && len(parts[1]) >= 2 {
			if p, ok := avcProfiles[parts[1][:2]]; ok {
				profile = p
			}
		}
		return "h264", c[:4], profile
	case strings.HasPrefix(c, "hev1"):
		return "hevc", "hev1", "Main"
	case strings.HasPrefix(c, "hvc1"):
		return "hevc", "hvc1", "Main"
	case strings.HasPrefix(c, "av01"):
		return "av1", "av01", "Main"
	case strings.HasPrefix(c, "mp4a"), strings.HasPrefix(c, "aac"):
		return "aac", "mp4a", ""
	case strings.HasPrefix(c, "ec-3"), strings.HasPrefix(c, "ec3"):
		return "eac3", "ec-3", ""
	case strings.HasPrefix(c, "flac"):
		return "flac", "fLaC", ""
	}
	return strings.ToLower(strings.SplitN(c, ".", 2)[0]), "", ""
}

// codecRank orders the codecs bilibili offers for one quality. AVC is first on
// purpose: it is the one every client here decodes, and a copy of it goes into
// MPEG-TS, which is the path that needs no packaging session. HEVC follows for
// clients that asked for it, AV1 last — it is the smallest and the least
// playable. A configured codec takes the top slot; anything it does not match
// drops to the bottom but is still usable.
func codecRank(codecs, wanted string) int {
	c := strings.ToLower(codecs)
	if wanted != "" && wanted != "auto" {
		if codecMatches(c, wanted) {
			return 0
		}
		return 10
	}
	switch {
	case strings.HasPrefix(c, "avc"):
		return 0
	case strings.HasPrefix(c, "hev"), strings.HasPrefix(c, "hvc"):
		return 1
	case strings.HasPrefix(c, "av01"):
		return 2
	}
	return 3
}

// codecMatches matches a configured codec name against a DASH codecs string.
// bilibili names the family in the string (avc1 / hev1 / hvc1 / av01) while a
// person configuring this writes h264 or hevc, so both spellings are accepted.
func codecMatches(codecs, wanted string) bool {
	switch strings.ToLower(wanted) {
	case "avc", "h264":
		return strings.HasPrefix(codecs, "avc")
	case "hevc", "h265":
		return strings.HasPrefix(codecs, "hev") || strings.HasPrefix(codecs, "hvc")
	case "av1", "av01":
		return strings.HasPrefix(codecs, "av01")
	}
	return strings.HasPrefix(codecs, strings.ToLower(wanted))
}

// pickVideo chooses one representation: the highest quality the account may
// have, and within it the most playable codec.
func (d *BilibiliDriver) pickVideo(reps []biliRep) (biliRep, bool) {
	best := biliRep{}
	found := false
	for _, r := range reps {
		if repURL(r) == "" {
			continue
		}
		if !found {
			best, found = r, true
			continue
		}
		if betterRep(r, best, d.codec) {
			best = r
		}
	}
	return best, found
}

func betterRep(a, b biliRep, wanted string) bool {
	if a.ID != b.ID {
		return a.ID > b.ID
	}
	ra, rb := codecRank(a.Codecs, wanted), codecRank(b.Codecs, wanted)
	if ra != rb {
		return ra < rb
	}
	// Same quality and same codec family: keep the heavier one, it is the
	// higher-bitrate variant of the same ladder rung.
	return a.Bandwidth > b.Bandwidth
}

// pickAudio chooses the highest-bitrate audio track.
func pickAudio(reps []biliRep) (biliRep, bool) {
	best := biliRep{}
	found := false
	for _, r := range reps {
		if repURL(r) == "" {
			continue
		}
		if !found || r.Bandwidth > best.Bandwidth {
			best, found = r, true
		}
	}
	return best, found
}

// repURL is the address to fetch a representation from: the main CDN one, or the
// first backup when bilibili left it out (its PCDN nodes are the flaky ones, and
// the backups exist for exactly that).
func repURL(r biliRep) string {
	if r.BaseURL != "" {
		return r.BaseURL
	}
	if len(r.BackupURL) > 0 {
		return r.BackupURL[0]
	}
	return ""
}

// ---------- Driver interface ----------

// HasDirect is false: a link that only works with a bilibili Referer cannot be
// handed to a browser, so nothing here is ever 302'd.
func (d *BilibiliDriver) HasDirect() bool { return false }

// DirectURL answers with the video stream even though HasDirect is false, so a
// caller that ignores MultiPart still has something to work with.
func (d *BilibiliDriver) DirectURL(ctx context.Context, path string) (string, map[string]string, error) {
	parts, err := d.Parts(ctx, path)
	if err != nil {
		return "", nil, err
	}
	return parts[0].URL, parts[0].Headers, nil
}

// ProbeURL is the same answer: one URL cannot describe this file, and the parts
// are what the engine is meant to read.
func (d *BilibiliDriver) ProbeURL(ctx context.Context, path string) (string, map[string]string, error) {
	return d.DirectURL(ctx, path)
}

// Parts resolves the two DASH streams of an episode.
func (d *BilibiliDriver) Parts(ctx context.Context, path string) ([]Part, error) {
	epID, ok := episodeIDFromPath(path)
	if !ok {
		return nil, fmt.Errorf("bilibili: 不是可播放的剧集路径: %s", path)
	}
	ref, err := d.episode(ctx, epID)
	if err != nil {
		return nil, err
	}
	info, err := d.playInfo(ctx, ref, epID)
	if err != nil {
		return nil, err
	}
	dur := 0.0
	if info.Dash.Duration > 0 {
		dur = float64(info.Dash.Duration)
	} else if ref.Duration > 0 {
		dur = float64(ref.Duration) / 1000.0
	}
	headers := d.streamHeaders()

	vid, ok := d.pickVideo(info.Dash.Video)
	if !ok {
		return nil, fmt.Errorf("bilibili: 没有可用的视频流")
	}
	vname, vtag, vprofile := codecOf(vid.Codecs)
	parts := []Part{{
		URL: repURL(vid), Headers: headers, Kind: "video",
		Codec: vname, Tag: vtag, Profile: vprofile,
		Width: vid.Width, Height: vid.Height, Duration: dur,
	}}
	if aud, ok := pickAudio(info.Dash.Audio); ok {
		aname, atag, _ := codecOf(aud.Codecs)
		parts = append(parts, Part{
			URL: repURL(aud), Headers: headers, Kind: "audio",
			Codec: aname, Tag: atag, Channels: 2, Duration: dur,
		})
	}
	return parts, nil
}

// Open returns the video stream's bytes. Nothing in this tree is a small file
// (no .strm, no subtitles), so this exists only so the driver is complete.
func (d *BilibiliDriver) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	raw, headers, err := d.DirectURL(ctx, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpDo(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("bilibili: GET %s: %s", path, resp.Status)
	}
	return resp.Body, nil
}

// ---------- 弹幕 ----------

// dmXML is the shape of the list.so document.
type dmXML struct {
	Ds []struct {
		P string `xml:"p,attr"`
		T string `xml:",chardata"`
	} `xml:"d"`
}

// Comments returns an episode's bullet comments, oldest first.
func (d *BilibiliDriver) Comments(ctx context.Context, path string) ([]Comment, error) {
	epID, ok := episodeIDFromPath(path)
	if !ok {
		return nil, fmt.Errorf("bilibili: 不是可播放的剧集路径: %s", path)
	}
	ref, err := d.episode(ctx, epID)
	if err != nil {
		return nil, err
	}
	if ref.Cid == 0 {
		return nil, fmt.Errorf("bilibili: 该集没有弹幕 id")
	}
	v, err := biliCached(fmt.Sprintf("dm:%d", ref.Cid), 30*time.Minute, func() (any, error) {
		return d.fetchComments(ctx, ref.Cid)
	})
	if err != nil {
		return nil, err
	}
	return v.([]Comment), nil
}

func (d *BilibiliDriver) fetchComments(ctx context.Context, cid int64) ([]Comment, error) {
	u := fmt.Sprintf("%s/x/v1/dm/list.so?oid=%d", biliAPI, cid)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range d.headers() {
		req.Header.Set(k, v)
	}
	resp, err := httpDo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 拉取弹幕失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bilibili: 拉取弹幕失败: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	body := decodeMaybeDeflate(raw)
	var doc dmXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("bilibili: 弹幕格式无法解析: %w", err)
	}
	out := make([]Comment, 0, len(doc.Ds))
	for _, e := range doc.Ds {
		f := strings.Split(e.P, ",")
		if len(f) < 4 {
			continue
		}
		t, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		mode, _ := strconv.Atoi(f[1])
		size, _ := strconv.Atoi(f[2])
		color, _ := strconv.Atoi(f[3])
		text := strings.TrimSpace(e.T)
		if text == "" {
			continue
		}
		out = append(out, Comment{Time: t, Mode: mode, Size: size, Color: color, Text: text})
		if len(out) >= biliMaxCommentsPerFile {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, nil
}

// decodeMaybeDeflate unwraps what list.so sends. The endpoint answers with a raw
// deflate stream (measured), but a proxy in between may hand back zlib or plain
// XML, so all three are accepted rather than assumed.
func decodeMaybeDeflate(raw []byte) []byte {
	if len(raw) > 0 && raw[0] == '<' {
		return raw
	}
	if b, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), 32<<20)); err == nil && looksLikeXML(b) {
		return b
	}
	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer zr.Close()
		if b, err := io.ReadAll(io.LimitReader(zr, 32<<20)); err == nil && looksLikeXML(b) {
			return b
		}
	}
	return raw
}

func looksLikeXML(b []byte) bool {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	return i < len(b) && b[i] == '<'
}
