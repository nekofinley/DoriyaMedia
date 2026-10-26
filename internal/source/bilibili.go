package source

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/netproxy"
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

// biliAPI is the API origin. It is a variable rather than a constant so tests
// can point the driver at a local server; nothing in production writes it.
var biliAPI = "https://api.bilibili.com"

// BilibiliDriver lists 番剧 as a virtual tree and plays episodes through the
// engine's multi-input remux.
type BilibiliDriver struct {
	cookie string
	// quality is the rung the viewer picked for this request; 0 means automatic
	// (the best SDR rung the account is entitled to). It is set per request from
	// the player's 清晰度 menu — the driver instance is built for one HTTP
	// request, so one viewer's choice never reaches another's stream.
	quality int
	// selected is the rung the last resolved ladder actually used. It differs
	// from quality when the requested rung is absent (an episode without 4K, a
	// stored preference from a title that had it) or when the codec preference
	// forces a lower rung; the API reports it so the player can show the truth.
	selected int
	codec    string // avc | hevc | av01 | auto

	// proxySpec is this source's own way out, for a machine bilibili's region
	// check refuses (see SourceConfig.BiliProxy). The zero value means direct:
	// every request then goes through the shared client exactly as before.
	proxySpec netproxy.Spec
	// client is the HTTP client the API calls and the CDN reads go through: the
	// shared one when there is no proxy, otherwise one dialing through it.
	client *http.Client

	// playableOnly keeps the listing to what this route can play: see
	// SourceConfig.BiliPlayableOnly. The check is per season (one playurl probe,
	// cached), because that is the granularity a library card has.
	playableOnly bool

	// buvid is the device id bilibili's risk control wants on API requests. A
	// cookie captured from a browser can lack buvid3, and the search endpoint
	// then answers with an HTML 412 after the first few requests. It is fetched
	// once from /x/frontend/finger/spi and appended to the cookie header.
	buvidMu    sync.Mutex
	buvid      string
	buvidTried bool
}

// NewBilibili builds the driver. proxy is the source's own proxy setting in any
// of the forms netproxy accepts (socks5:// / http:// / ss:// / host:port); an
// empty string means direct. playableOnly asks the driver to leave out seasons
// the route it dials from cannot play (see SourceConfig.BiliPlayableOnly).
//
// There is deliberately no quality parameter: the rung is chosen while the
// episode plays (SetQuality, from the player's 清晰度 menu), not fixed in the
// library's source settings.
func NewBilibili(cookie string, codec string, proxy string, playableOnly bool) (*BilibiliDriver, error) {
	switch codec {
	case "", "auto", "avc", "h264", "hevc", "h265", "av1", "av01":
	default:
		return nil, fmt.Errorf("bilibili: 未知编码 %q(可选 avc/hevc/av1/auto)", codec)
	}
	if codec == "" {
		codec = "avc"
	}
	d := &BilibiliDriver{
		cookie: strings.TrimSpace(cookie), codec: codec,
		client: httpClient, playableOnly: playableOnly,
	}
	spec, err := netproxy.Parse(proxy)
	switch {
	case errors.Is(err, netproxy.ErrNoProxy):
		return d, nil
	case err != nil:
		return nil, fmt.Errorf("bilibili: 代理设置无效: %w", err)
	}
	c, err := netproxy.NewClient(spec, 0)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 代理无法使用: %w", err)
	}
	d.proxySpec, d.client = spec, newSourceHTTPClient(c)
	return d, nil
}

// ProxySummary describes the source's proxy for a status line, or "" when it
// dials directly.
func (d *BilibiliDriver) ProxySummary() string {
	if d == nil || d.proxySpec.Raw == "" {
		return ""
	}
	return d.proxySpec.Summary()
}

// ProxySpec states the proxy as configured, for the playback engine (which has
// to route ffmpeg's own fetches through it too). Empty means direct.
func (d *BilibiliDriver) ProxySpec() string {
	if d == nil {
		return ""
	}
	return d.proxySpec.Raw
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
	ck := d.cookie
	d.buvidMu.Lock()
	if d.buvid != "" {
		if ck == "" {
			ck = d.buvid
		} else {
			ck += "; " + d.buvid
		}
	}
	d.buvidMu.Unlock()
	if ck != "" {
		h["Cookie"] = ck
	}
	return h
}

// ensureBuvid fetches the device id bilibili's risk control expects, once per
// driver. A browser cookie normally carries buvid3; a cookie copied out of one
// often does not, and /x/web-interface/search/type then answers with an HTML 412
// (not the JSON -352) after a few requests. The SPI endpoint is public.
func (d *BilibiliDriver) ensureBuvid(ctx context.Context) {
	if strings.Contains(d.cookie, "buvid3=") {
		return
	}
	d.buvidMu.Lock()
	defer d.buvidMu.Unlock()
	if d.buvidTried {
		return
	}
	d.buvidTried = true

	req, err := http.NewRequestWithContext(ctx, "GET", biliAPI+"/x/frontend/finger/spi", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", biliUA)
	req.Header.Set("Referer", biliReferer)
	req.Header.Set("Origin", "https://www.bilibili.com")
	if d.cookie != "" {
		req.Header.Set("Cookie", d.cookie)
	}
	resp, err := d.do(ctx, req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return
	}
	var spi struct {
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &spi) != nil || strings.TrimSpace(spi.Data.B3) == "" {
		return
	}
	extra := "buvid3=" + strings.TrimSpace(spi.Data.B3)
	if b4 := strings.TrimSpace(spi.Data.B4); b4 != "" {
		extra += "; buvid4=" + b4
	}
	d.buvid = extra
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
	d.ensureBuvid(ctx)
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
	resp, err := d.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bilibili: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 412, 429:
			return nil, fmt.Errorf("bilibili: 被 B 站风控/限流(%d),请过几分钟再试", resp.StatusCode)
		}
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

// do sends one request through this source's own client: the shared one when no
// proxy is configured, otherwise one dialing through the source's proxy. Every
// bilibili request — API, CDN and 弹幕 — goes through here, because a machine
// that needs a proxy for the region check needs it for all of them.
func (d *BilibiliDriver) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := d.client
	if client == nil {
		client = httpClient
	}
	return client.Do(req.WithContext(ctx))
}

// errNotLoggedIn is bilibili's -101. It is a sentinel because one caller has to
// read it as an answer rather than a failure: the account check asks "who does
// this cookie belong to", and "nobody" is a perfectly good reply — reporting it
// as a network error would send the user to fix their proxy.
var errNotLoggedIn = errors.New("bilibili: 账号未登录")

// errRegionLocked is bilibili's -10403: the licence for this title does not
// cover the region the request came from. It is a decision the caller can act
// on (this season is not watchable from here), unlike every other API failure,
// so it is a sentinel rather than a message.
var errRegionLocked = errors.New("bilibili: 该内容在当前地区不可观看(-10403)")

// biliError turns the codes a viewer can actually meet into something worth
// reading. Everything else keeps bilibili's own wording.
func biliError(code int, msg string) error {
	switch code {
	case -101:
		return fmt.Errorf("%w(Cookie 失效或没填)——登录后才能看会员集和高清", errNotLoggedIn)
	case -10403:
		// Both directions exist: a mainland-only title from a Hong Kong egress
		// and a 港澳台-only title from a mainland one answer the same code. The
		// advice therefore names both regions instead of assuming the machine
		// should land in mainland.
		return fmt.Errorf("%w;港澳台与大陆的版权不同,换一个落地在它能看的地区的代理即可", errRegionLocked)
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

// pgcSeasonRef is one entry of a series' season list: bilibili's own name for
// that 季度 and its id. The list is the same whoever you ask, and it is ordered,
// with the series' first season first — which makes its head a stable key for
// the whole franchise.
type pgcSeasonRef struct {
	SeasonID    int64  `json:"season_id"`
	SeasonTitle string `json:"season_title"` // 第一季 / 游郭篇 / 立志篇(中配)
	Badge       string `json:"badge"`
	SeasonType  int    `json:"season_type"` // 1 = 番剧, 2 = 剧场版
}

type pgcSeason struct {
	SeasonID int64        `json:"season_id"`
	Title    string       `json:"title"`
	Evaluate string       `json:"evaluate"`
	Total    int          `json:"total"`
	Episodes []pgcEpisode `json:"episodes"`
	Section  []pgcSection `json:"section"`
	// Seasons is every 季度 of this series, this one included.
	Seasons []pgcSeasonRef `json:"seasons"`
	Rating  struct {
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

// biliFormat is one row of the playurl answer's support_formats: bilibili's own
// name for a rung in the quality menu it would draw. The menu here reads it so
// the labels match what the viewer knows from bilibili itself (1080P 高码率,
// 4K 超清 …) rather than an invented table.
type biliFormat struct {
	Quality        int    `json:"quality"`
	NewDescription string `json:"new_description"`
	Description    string `json:"description"`
	DisplayDesc    string `json:"display_desc"`
	NeedVIP        bool   `json:"need_vip"`
}

type pgcPlayInfo struct {
	Quality   int      `json:"quality"`
	IsPreview int      `json:"is_preview"`
	TimeLen   int64    `json:"timelength"` // ms
	Dash      *pgcDash `json:"dash"`
	// AcceptQuality is every rung the API is willing to talk about, and
	// SupportFormats is the same list with bilibili's labels. Neither is the
	// ladder that can actually be played: an account without a membership is
	// told 4K and HDR exist while Dash carries only 480P and 360P. The menu is
	// built from the representations, and these two only name them.
	AcceptQuality  []int        `json:"accept_quality"`
	SupportFormats []biliFormat `json:"support_formats"`
}

// ---------- virtual tree ----------
//
// Two levels above the episodes, because bilibili has two:
//
//	/<系列> [series 26801]/<季度> [season 39433]/<剧集> [ep267851].mp4
//
// A 番剧's quarterly sequels — 立志篇, 游郭篇, 刀匠村篇 — are separate seasons on
// bilibili, published as separate entries with their own episode numbering. Left
// flat they would arrive in a media library as so many unrelated shows; nested
// like this the scanner reads them the way it reads any other series, with the
// 系列 as the show and each 季度 as one of its seasons (its 命名季 support, which
// exists for exactly this: 物语系列/{化物语,伪物语}).
//
// The brackets are the whole state: a path alone resolves the next level, so the
// tree survives a restart, and the scanner's name parser drops bracketed suffixes,
// so none of it reaches the UI as part of a title.

var reSeriesDir = regexp.MustCompile(`\[series (\d+)\]$`)
var reSeasonDir = regexp.MustCompile(`\[season (\d+)\]$`)
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

// seriesDir names a 番剧 series — the whole franchise, all of its 季度 under it.
func seriesDir(title string, key int64) string {
	return fmt.Sprintf("%s [series %d]", biliClean(title), key)
}

// seasonDirOf names one 季度 inside a series. Bilibili's own name for it is the
// best label there is: 第一季 / 第二季 / 游郭篇 / 刀匠村篇 — and the numbered ones
// are then read as seasons by the scanner's own season parser, so 第二季 lands in
// season 2 without any mapping table.
func seasonDirOf(title string, seasonID int64) string {
	return fmt.Sprintf("%s [season %d]", biliClean(title), seasonID)
}

// episodeFileName names one entry of a season after bilibili's own words for it.
//
// A numbered episode keeps the shape it always had, "<series> 第N话 <标题>"
// (bilibili's title is the number, its long title the episode's own name), so a
// rescan does not move every file of a library to a new path — a path is what a
// library keys its rows by, and moving them would take the watch progress with
// it.
//
// Everything else in a season listing is *not* an episode, and must not be
// readable as one: bilibili keeps 特别篇 / PV / 预告 in sections beside the
// episodes, named as a viewer would describe them ("第3话预告" — the preview of
// episode 3). That name contains "第3话", so a scanner reading it as a name sees
// episode 3 again and the preview ends up as a second version of episode 3 — and
// that is what plays when the episode is picked. The section is what the entry
// actually belongs to, so its own label is carried as a marker (extraTag) that
// the scanner reads before anything else: order within the section, and the
// section's name.
func episodeFileName(series string, ep *pgcEpisode, section string, at int) string {
	num := strings.TrimSpace(ep.Title)
	name := biliClean(series)
	marker := ""
	switch {
	case section != "":
		name += " " + biliClean(section)
		if num != "" {
			name += " " + biliClean(num)
		}
		marker = extraTag(at, section)
	case num != "":
		if _, err := strconv.Atoi(num); err == nil {
			name += " 第" + num + "话"
		} else {
			// "OVA" / "SP1" / "剧场版": a real entry bilibili does not number.
			// Its own word is the best label there is — as a card of its own,
			// not as a version of episode 1.
			name += " " + biliClean(num)
			marker = extraTag(at, num)
		}
	}
	if lt := strings.TrimSpace(ep.LongTitle); lt != "" {
		name += " " + biliClean(lt)
	}
	if marker != "" {
		name += " " + marker
	}
	return fmt.Sprintf("%s [ep%d].mp4", name, ep.ID)
}

// extraTag is the marker that says an entry is a section item: [sp#<order>#<section>].
// It goes before the [ep<id>] tag, which has to stay last because that is what
// identifies the file. Brackets and the separator are stripped from the label so
// the marker itself stays parseable whatever bilibili calls the section.
func extraTag(at int, label string) string {
	label = biliClean(label)
	label = strings.NewReplacer("#", " ", "[", " ", "]", " ").Replace(label)
	return fmt.Sprintf("[sp#%d#%s]", at, strings.Join(strings.Fields(label), " "))
}

func lastSegment(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dirID(re *regexp.Regexp, name string) (int64, bool) {
	m := re.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func seriesKeyFromDir(name string) (int64, bool) { return dirID(reSeriesDir, name) }
func seasonKeyFromDir(name string) (int64, bool) { return dirID(reSeasonDir, name) }

// franchiseTitle works out what a series is called from one of its seasons.
//
// Bilibili does not state the series' name: it gives the season's own title
// ("鬼灭之刃 柱训练篇") and, in the series list, what that season is within it
// ("柱训练篇"). One minus the other is the series, and every season of a series
// computes the same answer — which is what lets a season found through the hot
// index land in the same directory as the same season found through 我的追番.
func franchiseTitle(title, seasonTitle string) string {
	t := strings.TrimSpace(title)
	st := strings.TrimSpace(seasonTitle)
	if st != "" && t != st && strings.HasSuffix(t, st) {
		if base := strings.TrimSpace(strings.TrimSuffix(t, st)); base != "" {
			return base
		}
	}
	return t
}

// seriesName is what a series is called, always derived from its *first* season.
//
// Deriving it from whichever 季度 you arrived through would name one series
// several ways — 工作细胞 through its movie came out as 工作细胞：细胞大作战 —
// and a directory that changes its name between two scans reads to the scanner
// as a different show: the old one is deleted, and the watch progress with it.
func (d *BilibiliDriver) seriesName(ctx context.Context, key int64) (string, error) {
	v, err := biliCached(fmt.Sprintf("series-name:%d", key), 6*time.Hour, func() (any, error) {
		s, err := d.season(ctx, key)
		if err != nil {
			return "", err
		}
		label := ""
		for _, ref := range s.Seasons {
			if ref.SeasonID == key {
				label = ref.SeasonTitle
				break
			}
		}
		name := strings.TrimSpace(franchiseTitle(s.Title, label))
		// A series title that lost its tail to the split can end in punctuation:
		// 小车队大救援：安全先锋队 minus 安全先锋队 leaves 小车队大救援：.
		name = strings.Trim(strings.TrimSpace(name), "：:·-—– \t")
		if name == "" {
			return strings.TrimSpace(s.Title), nil
		}
		return name, nil
	})
	if err != nil {
		return "", err
	}
	name, _ := v.(string)
	return name, nil
}

// franchiseOf returns the key and name of the series a season belongs to. The
// key is the first season in bilibili's own series list: the same for every
// sibling, and enough to list the whole series again from the path alone.
func (d *BilibiliDriver) franchiseOf(ctx context.Context, seasonID int64) (int64, string, error) {
	s, err := d.season(ctx, seasonID)
	if err != nil {
		return 0, "", err
	}
	if len(s.Seasons) == 0 {
		// A season with no series list is a series of one.
		return s.SeasonID, strings.TrimSpace(s.Title), nil
	}
	key := s.Seasons[0].SeasonID
	if key == 0 {
		key = s.SeasonID
	}
	name, err := d.seriesName(ctx, key)
	if err != nil || name == "" {
		// Still list it — as a series named after this season rather than none.
		for _, ref := range s.Seasons {
			if ref.SeasonID == s.SeasonID {
				return key, franchiseTitle(s.Title, ref.SeasonTitle), nil
			}
		}
		return key, strings.TrimSpace(s.Title), nil
	}
	return key, name, nil
}

// episodeIDFromPath p is a driver path of an episode file.
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
	// A directory names what it is in its own brackets, so a path is all the
	// state there is: a season directory lists its episodes, a series directory
	// lists its seasons, wherever either of them hangs.
	last := lastSegment(dir)
	if id, ok := seasonKeyFromDir(last); ok {
		return d.episodeEntries(ctx, dir, id)
	}
	if key, ok := seriesKeyFromDir(last); ok {
		return d.seasonEntries(ctx, dir, key)
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

// rootEntries lists 番剧 series: what the account follows, then what is airing,
// then the most followed.
//
// Every directory here is a whole series rather than a single 季度, and that is
// on purpose twice over. This tree is what a 媒体库 pointed at this source scans,
// and the scanner turns every directory under the library root into one show — so
// a structural directory (「最近更新」, a day, a category) would become a show of
// its own, and a lone 季度 (「游郭篇」) would become a show of its own too, which
// is what made a series with three seasons arrive as three unrelated anime.
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
	return d.seriesEntries(ctx, "/", dedupeSeasons(lists...)), nil
}

func (d *BilibiliDriver) hotEntries(ctx context.Context) ([]Entry, error) {
	hot, err := d.hotSeasons(ctx)
	if err != nil {
		return nil, err
	}
	return d.seriesEntries(ctx, "/热门番剧", hot), nil
}

func (d *BilibiliDriver) followEntries(ctx context.Context) ([]Entry, error) {
	if d.cookie == "" {
		return nil, fmt.Errorf("bilibili: 我的追番需要先填写账号 Cookie")
	}
	f, err := d.followSeasons(ctx)
	if err != nil {
		return nil, err
	}
	return d.seriesEntries(ctx, "/我的追番", f), nil
}

// seriesEntries turns season seeds into series directories, one per franchise.
//
// A seed is a single 季度 (from the hot index, the timeline, or the follow list);
// resolving which series it belongs to costs one cached API call each, and is
// what keeps 鬼灭之刃's five 季度 in one directory instead of five.
func (d *BilibiliDriver) seriesEntries(ctx context.Context, parent string, seeds []pgcIndexItem) []Entry {
	seen := map[int64]bool{}
	out := make([]Entry, 0, len(seeds))
	for _, seed := range seeds {
		if seed.SeasonID == 0 {
			continue
		}
		key, title, err := d.franchiseOf(ctx, seed.SeasonID)
		if err != nil || key == 0 {
			// One unresolvable seed must not empty the whole listing: it is
			// listed as a series of itself, which is what it looks like without
			// the series list bilibili did not hand over.
			key, title = seed.SeasonID, seed.Title
		}
		if seen[key] || strings.TrimSpace(title) == "" {
			continue
		}
		seen[key] = true
		name := seriesDir(title, key)
		out = append(out, Entry{Name: name, Path: VirtualJoin(parent, name), IsDir: true})
	}
	return out
}

// seasonEntries lists one series' 季度, in bilibili's own order — which is the
// broadcast order, and why 第二季 lands in season 2 and 游郭篇 right after 立志篇.
func (d *BilibiliDriver) seasonEntries(ctx context.Context, dir string, seriesKey int64) ([]Entry, error) {
	s, err := d.season(ctx, seriesKey)
	if err != nil {
		return nil, err
	}
	refs := s.Seasons
	if len(refs) == 0 {
		// A series of one, listed by its own name.
		refs = []pgcSeasonRef{{SeasonID: s.SeasonID, SeasonTitle: s.Title}}
	}
	out := make([]Entry, 0, len(refs))
	for _, ref := range refs {
		if ref.SeasonID == 0 {
			continue
		}
		title := strings.TrimSpace(ref.SeasonTitle)
		if title == "" {
			title = fmt.Sprintf("第%d季", len(out)+1)
		}
		name := seasonDirOf(title, ref.SeasonID)
		out = append(out, Entry{Name: name, Path: VirtualJoin(dir, name), IsDir: true})
	}
	return out, nil
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
				List  []pgcIndexItem `json:"list"`
				Total int            `json:"total"`
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

// ---------- catalogue search (search → import) ----------

// biliCatalogItem is one hit of /x/web-interface/search/type?search_type=media_bangumi.
type biliCatalogItem struct {
	SeasonID       int64  `json:"season_id"`
	MediaID        int64  `json:"media_id"`
	Title          string `json:"title"`
	OrgTitle       string `json:"org_title"`
	Cover          string `json:"cover"`
	Desc           string `json:"desc"`
	IndexShow      string `json:"index_show"`
	SeasonType     int    `json:"season_type"`
	SeasonTypeName string `json:"season_type_name"`
	PubTime        int64  `json:"pubtime"`
	EpSize         int    `json:"ep_size"`
	MediaScore     struct {
		Score float64 `json:"score"`
	} `json:"media_score"`
}

var reCatalogTag = regexp.MustCompile(`<[^>]*>`)

// catalogText removes the <em class="keyword"> highlight a search hit wraps its
// match in, and decodes the entities bilibili leaves around it.
func catalogText(s string) string {
	s = reCatalogTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// parseCatalogHits converts the search payload into hits. getJSON has already
// unwrapped the API envelope, so this is the data object itself. Only 番剧 is
// kept: the search endpoint also matches films and documentaries, and those do
// not belong in a series library.
func parseCatalogHits(raw []byte, limit int) ([]CatalogHit, error) {
	var payload struct {
		Result []biliCatalogItem `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("bilibili: 搜索结果无法解析: %w", err)
	}
	seen := map[int64]bool{}
	out := make([]CatalogHit, 0, len(payload.Result))
	for _, it := range payload.Result {
		if it.SeasonType != 1 || it.SeasonID <= 0 || seen[it.SeasonID] {
			continue
		}
		title := catalogText(it.Title)
		if title == "" {
			continue
		}
		seen[it.SeasonID] = true
		cover := strings.Replace(it.Cover, "http://", "https://", 1)
		year := 0
		if it.PubTime > 0 {
			year = time.Unix(it.PubTime, 0).Year()
		}
		out = append(out, CatalogHit{
			SeasonID: it.SeasonID,
			Title:    title,
			Original: catalogText(it.OrgTitle),
			Overview: strings.TrimSpace(it.Desc),
			Cover:    cover,
			Year:     year,
			Episodes: it.EpSize,
			Score:    it.MediaScore.Score,
			Index:    strings.TrimSpace(it.IndexShow),
			Kind:     it.SeasonTypeName,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SearchCatalog finds 番剧 in bilibili's public catalogue. The catalogue is what
// the library's root cannot show: 我的追番 + 正在更新 + 热门 are a sample, so a
// title the viewer wants can be absent from the tree and still be on bilibili.
func (d *BilibiliDriver) SearchCatalog(ctx context.Context, keyword string, limit int) ([]CatalogHit, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 30 {
		limit = 20
	}
	// The search endpoint is the one bilibili rate-limits hardest; caching per
	// keyword keeps a viewer typing in the search box from spending the source's
	// request budget.
	key := "catalog-search:" + strings.ToLower(keyword)
	v, err := biliCached(key, 10*time.Minute, func() (any, error) {
		raw, err := d.getJSON(ctx, "/x/web-interface/search/type", url.Values{
			"search_type": {"media_bangumi"},
			"keyword":     {keyword},
			"page":        {"1"},
		})
		if err != nil {
			return nil, err
		}
		return parseCatalogHits(raw, 30)
	})
	if err != nil {
		return nil, err
	}
	hits := v.([]CatalogHit)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// CatalogImportPath resolves the series directory a catalogue hit belongs to.
// Importing a season means importing its whole series: the virtual tree groups
// every 季度 of a franchise under one series directory, and the scanner turns
// that directory into one show with its seasons.
func (d *BilibiliDriver) CatalogImportPath(ctx context.Context, seasonID int64) (string, string, error) {
	if seasonID <= 0 {
		return "", "", fmt.Errorf("bilibili: 无效的季度 ID")
	}
	key, title, err := d.franchiseOf(ctx, seasonID)
	if err != nil {
		return "", "", err
	}
	if key == 0 {
		key = seasonID
	}
	if strings.TrimSpace(title) == "" {
		if s, err := d.season(ctx, seasonID); err == nil {
			title = strings.TrimSpace(s.Title)
		}
	}
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("番剧 %d", seasonID)
	}
	return VirtualJoin("/", seriesDir(title, key)), title, nil
}

// episodeEntries lists one season's episodes and its extra sections. Paths are
// built under the directory that was actually listed, so the same 季度 reached
// through two paths still resolves to its own episodes.
func (d *BilibiliDriver) episodeEntries(ctx context.Context, dir string, seasonID int64) ([]Entry, error) {
	s, err := d.season(ctx, seasonID)
	if err != nil {
		return nil, err
	}
	// A season this route cannot play is listed as *empty*, not as an error.
	//
	// The scan is what this is for: an empty season directory holds no files,
	// so the title never becomes a show — and a rescan removes the rows an
	// earlier scan stored for it, which is what makes switching the proxy (or
	// turning this on) clean up a library instead of leaving dead cards behind.
	// Returning an error instead would only add a warning per season and could
	// fail a scan whose whole catalogue is unavailable from here; it would also
	// leave the old rows in place, because an unreadable directory proves
	// nothing to the scanner's cleanup.
	if d.playableOnly && !d.seasonPlayable(ctx, s) {
		return []Entry{}, nil
	}
	// The file names carry the series, not the season: they already sit inside
	// the season's own directory, and the series is what a player shows as the
	// title when an episode is opened from the file browser.
	series := strings.TrimSpace(s.Title)
	for _, ref := range s.Seasons {
		if ref.SeasonID == s.SeasonID {
			series = franchiseTitle(s.Title, ref.SeasonTitle)
			break
		}
	}
	out := make([]Entry, 0, len(s.Episodes)+4)
	// at is the entry's own position within its list. It is what the section
	// marker records, so a 特别篇 that is not numbered still has a stable order.
	add := func(ep *pgcEpisode, section string, at int) {
		name := episodeFileName(series, ep, section, at)
		out = append(out, Entry{
			Name: name, Path: VirtualJoin(dir, name),
			IsDir: false, MTime: ep.PubTime,
		})
	}
	for i := range s.Episodes {
		add(&s.Episodes[i], "", i+1)
	}
	for _, sec := range s.Section {
		for i := range sec.Episodes {
			add(&sec.Episodes[i], sec.Title, i+1)
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

// playURL asks for one episode's streams and returns the payload as it comes.
// playInfo is the typed reader; the region probe needs only the error, and
// going through the same request keeps the two from drifting apart.
func (d *BilibiliDriver) playURL(ctx context.Context, ref biliEpisodeRef, epID int64, qn int) (json.RawMessage, error) {
	if qn <= 0 {
		qn = 127 // 8K; bilibili hands back the best the account is entitled to
	}
	return d.getJSON(ctx, "/pgc/player/web/playurl", url.Values{
		"ep_id": {strconv.FormatInt(epID, 10)},
		"cid":   {strconv.FormatInt(ref.Cid, 10)},
		"avid":  {strconv.FormatInt(ref.Aid, 10)},
		"qn":    {strconv.Itoa(qn)},
		"fnval": {"4048"}, // DASH + 4K + HDR + Dolby, and every codec bilibili has
		"fnver": {"0"}, "fourk": {"1"}, "otype": {"json"},
		"session": {""}, "module": {"bangumi"},
	})
}

// playInfo asks for one episode's full ladder.
//
// The request always asks for the highest rung (qn=127): the answer then lists
// every representation the account may have, and the rung to play is chosen
// from that ladder by pickVideo. Asking for the viewer's rung instead would
// make the answer for one episode depend on it, which is what made the old
// library-wide setting impossible to change while watching.
func (d *BilibiliDriver) playInfo(ctx context.Context, ref biliEpisodeRef, epID int64) (*pgcPlayInfo, error) {
	raw, err := d.playURL(ctx, ref, epID, 0)
	if err != nil {
		return nil, d.explainRefusal(ctx, err)
	}
	var info pgcPlayInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	if err := checkPlayInfo(&info, ref.Duration); err != nil {
		return nil, d.explainRefusal(ctx, err)
	}
	return &info, nil
}

// errCookieDead is a refusal that is really "nobody is logged in". It is a
// sentinel because the two look identical from the playurl answer alone — a
// 会员 episode without a membership and a 会员 episode without a *session* both
// come back as is_preview 1 — and only one of them is fixed by paying bilibili.
var errCookieDead = errors.New("bilibili: Cookie 已失效(账号未登录)")

// explainRefusal re-labels a refusal with the reason it actually has.
//
// The playurl answer cannot tell the two apart: an account without a membership
// and a request without a session both get code 0, is_preview 1 and no DASH. The
// difference is one nav call, and it is worth making — "这一集需要大会员" sends a
// paying member to buy a membership they already have, while the real fix is
// re-logging in. It is asked only when something was already refused, so a
// playable episode never pays for it.
func (d *BilibiliDriver) explainRefusal(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, errRegionLocked) || errors.Is(err, errCookieDead) {
		return err
	}
	// The API already said so itself (-101). No nav call is needed to confirm
	// it, and the message must not repeat "账号未登录" twice around the reason.
	if errors.Is(err, errNotLoggedIn) {
		return fmt.Errorf("%w —— 登录后这一集就能正常播放,不需要重新开通大会员", errCookieDead)
	}
	acc, aerr := d.Account(ctx)
	if aerr != nil || acc == nil || acc.LoggedIn {
		return err
	}
	// The old wording is kept as the tail so nothing is lost, but the reason
	// leads: the viewer's next action is to log in again, not to buy anything.
	return fmt.Errorf("%w: %s —— 登录后这一集就能正常播放,不需要重新开通大会员",
		errCookieDead, strings.TrimPrefix(err.Error(), "bilibili: "))
}

// probeEpisode picks the entry a region check asks playurl about: the first
// regular episode, or the first section entry when a season carries none (only
// PVs and previews, an unusual but real shape). A season with neither has
// nothing playurl could answer for.
func probeEpisode(s *pgcSeason) *pgcEpisode {
	if s == nil {
		return nil
	}
	for i := range s.Episodes {
		if ep := &s.Episodes[i]; ep.ID != 0 && ep.Cid != 0 {
			return ep
		}
	}
	for i := range s.Section {
		for j := range s.Section[i].Episodes {
			if ep := &s.Section[i].Episodes[j]; ep.ID != 0 && ep.Cid != 0 {
				return ep
			}
		}
	}
	return nil
}

// seasonPlayable asks the one endpoint whose answer depends on where the
// request comes from. The season page is *not* region-checked: bilibili hands
// the metadata — title, episode list, covers — to any region, and puts the
// licence decision in playurl. A title licensed only outside the current
// egress therefore looks perfectly normal in the tree and answers -10403 the
// moment a viewer presses play. That is exactly the card this filter removes.
//
// Only a definitive -10403 hides a season. A timeout, a rate limit or an
// account problem must not: an empty listing is what a scan reads as "this
// season has no episodes", and a flaky probe would take the library rows with
// it. Those answers keep the season listed and let playback report its own
// reason.
func (d *BilibiliDriver) seasonPlayable(ctx context.Context, s *pgcSeason) bool {
	ep := probeEpisode(s)
	if ep == nil {
		return true
	}
	// The answer is a property of (season, egress), not of the account —
	// bilibili's region check is by IP — so the probe is cached under the proxy
	// it was made through. Six hours keeps a rescan (and every browse of the
	// same season) from doubling the API calls; changing the proxy changes the
	// key, so a new route is checked immediately.
	key := fmt.Sprintf("playable:%s:%d", d.proxySpec.Raw, s.SeasonID)
	v, err := biliCached(key, 6*time.Hour, func() (any, error) {
		_, err := d.playURL(ctx, biliEpisodeRef{
			SeasonID: s.SeasonID, Cid: ep.Cid, Aid: ep.Aid, Duration: ep.Duration,
		}, ep.ID, 80)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, errRegionLocked):
			return false, nil
		default:
			return nil, err
		}
	})
	if err != nil {
		// Unknown is not "unplayable": never empty a season on a failure that
		// is not bilibili's region verdict.
		return true
	}
	ok, _ := v.(bool)
	return ok
}

// checkPlayInfo rejects the shapes that carry no streams — or carry the wrong
// ones. A membership episode is the case that matters most: for an account
// without a membership bilibili answers code 0, is_preview 1 and no DASH at
// all. That is not an error the API reports, so it has to be recognised here or
// the viewer gets a player that never starts.
//
// Two further answers are refused for the same reason: they are not the
// episode, but they do play.
//
//   - is_preview 1 *with* a DASH block is the trial clip: a few minutes of the
//     episode at 360p with a 64 kbit/s audio track. It used to pass this check
//     (there is video, so nothing looked wrong) and the viewer was shown a
//     couple of minutes of it instead of the episode.
//   - a DASH block whose duration is far short of the episode's is the same
//     thing wearing a different flag, and a sample/trailer answer looks exactly
//     like it. episodeMS is the length the season listing stated, 0 when it did
//     not.
func checkPlayInfo(info *pgcPlayInfo, episodeMS int64) error {
	if info.IsPreview == 1 {
		return fmt.Errorf("bilibili: 这一集需要大会员(当前账号只能试看)")
	}
	if info.Dash != nil && len(info.Dash.Video) > 0 {
		if episodeMS > 0 && info.Dash.Duration > 0 {
			got := int64(info.Dash.Duration) * 1000
			// 20% of slack: a listing's duration is rounded, and an episode can
			// carry a slightly longer opening than the number states.
			if got*5 < episodeMS*4 {
				return fmt.Errorf("bilibili: 只拿到 %d 秒的片段,整集约 %d 秒——这一集没有按正片下发(试看或样片)",
					info.Dash.Duration, episodeMS/1000)
			}
		}
		return nil
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

// hevcProfiles maps the profile_idc field of an RFC 6381 HEVC codec string
// ("hvc1.2.4.L150.90" → "2") to the profile name ffprobe reports. It matters
// because Main 10 is what bilibili's HDR rungs carry and plenty of consumer
// decoders that advertise HEVC only handle 8-bit Main.
var hevcProfiles = map[string]string{
	"1": "Main", "2": "Main 10", "3": "Main Still Picture", "4": "Rext",
}

// hevcProfile reads the profile out of an HEVC codec string.
func hevcProfile(codecs string) string {
	if parts := strings.Split(codecs, "."); len(parts) >= 2 {
		if p, ok := hevcProfiles[parts[1]]; ok {
			return p
		}
	}
	return "Main"
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
		return "hevc", "hev1", hevcProfile(c)
	case strings.HasPrefix(c, "hvc1"):
		return "hevc", "hvc1", hevcProfile(c)
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

// biliQNHDR and biliQNDolbyVision are the quality rungs bilibili publishes as
// HDR and as Dolby Vision. Both are HEVC Main 10 in BT.2020 with a PQ transfer
// function, and neither survives a plain stream copy to a normal client: the
// picture needs tone mapping (HDR) or a Dolby Vision decoder (DV), and without
// them the same frames come out washed out at best and green/purple at worst.
// The player sees a picture that looks like a different scene, not a slightly
// wrong colour, so these rungs are only ever a last resort.
const (
	biliQNHDR         = 125
	biliQNDolbyVision = 126
)

// isHDR reports whether this representation needs tone mapping (or a Dolby
// Vision decoder) before a viewer can see it as intended.
func (r biliRep) isHDR() bool {
	if r.ID == biliQNHDR || r.ID == biliQNDolbyVision {
		return true
	}
	c := strings.ToLower(strings.TrimSpace(r.Codecs))
	return strings.HasPrefix(c, "dvhe") || strings.HasPrefix(c, "dvh1") ||
		strings.HasPrefix(c, "dva1") || strings.HasPrefix(c, "dvav")
}

// ---------- quality ladder ----------

// SetQuality selects a rung for the calls that follow. 0 is automatic: the
// highest SDR rung the account has.
func (d *BilibiliDriver) SetQuality(qn int) {
	if qn < 0 {
		qn = 0
	}
	d.quality = qn
}

// Quality reports the preference (0 = automatic).
func (d *BilibiliDriver) Quality() int { return d.quality }

// SelectedQuality reports the rung the last resolved stream used. It is 0
// until an episode's ladder has been read.
func (d *BilibiliDriver) SelectedQuality() int { return d.selected }

// biliQualityLabels are bilibili's own names for the ladder's rungs, used when
// the answer carries no support_formats (a proxy or an older API shape may drop
// them) and to give the menu a compact name the control bar can hold.
var biliQualityLabels = map[int]string{
	127: "8K 超高清", 126: "杜比视界", 125: "HDR 真彩", 120: "4K 超清",
	116: "1080P60 高帧率", 112: "1080P 高码率", 80: "1080P 高清",
	74: "720P60 高帧率", 64: "720P 高清", 32: "480P 清晰",
	16: "360P 流畅", 6: "240P 极速",
}

var biliQualityShorts = map[int]string{
	127: "8K", 126: "杜比", 125: "HDR", 120: "4K",
	116: "1080P60", 112: "1080P+", 80: "1080P",
	74: "720P60", 64: "720P", 32: "480P", 16: "360P", 6: "240P",
}

func biliQualityName(qn int) string {
	if n, ok := biliQualityLabels[qn]; ok {
		return n
	}
	return fmt.Sprintf("Q%d", qn)
}

func biliQualityShort(qn int) string {
	if n, ok := biliQualityShorts[qn]; ok {
		return n
	}
	return fmt.Sprintf("Q%d", qn)
}

// Qualities lists the rungs one episode can actually be played at, best first.
//
// The list is built from the DASH representations, not from accept_quality: the
// API names rungs the account is not entitled to (an anonymous cookie is told
// HDR and 4K exist while its ladder holds only 360P and 480P), and a menu entry
// that cannot be played is worse than no entry. HDR / Dolby Vision rungs are
// left out as well: a plain stream copy of them is the wrong picture (see
// isHDR), so they are never a choice a viewer should be offered.
func (d *BilibiliDriver) Qualities(ctx context.Context, path string) ([]Quality, error) {
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
	if info.Dash == nil {
		return nil, nil
	}
	// The rung the stream will really use, decided by the same chooser Parts()
	// goes through, so the menu and the picture cannot disagree.
	if _, ok := d.pickVideo(info.Dash.Video); !ok {
		d.selected = 0
	}

	formats := make(map[int]biliFormat, len(info.SupportFormats))
	for _, f := range info.SupportFormats {
		formats[f.Quality] = f
	}

	// One entry per rung, with the largest picture any codec offers there.
	type rung struct {
		width, height int
		sdr           bool
	}
	seen := make(map[int]*rung, len(info.Dash.Video))
	var ids []int
	for _, r := range info.Dash.Video {
		if repURL(r) == "" {
			continue
		}
		g := seen[r.ID]
		if g == nil {
			g = &rung{}
			seen[r.ID] = g
			ids = append(ids, r.ID)
		}
		if r.Width > g.width {
			g.width = r.Width
		}
		if r.Height > g.height {
			g.height = r.Height
		}
		if !r.isHDR() {
			g.sdr = true
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))

	out := make([]Quality, 0, len(ids))
	for _, id := range ids {
		g := seen[id]
		if !g.sdr {
			continue
		}
		label := biliQualityName(id)
		if f, ok := formats[id]; ok {
			if n := strings.TrimSpace(f.NewDescription); n != "" {
				label = n
			}
		}
		out = append(out, Quality{
			ID: id, Label: label, Short: biliQualityShort(id),
			Width: g.width, Height: g.height,
		})
	}
	return out, nil
}

// pickVideo chooses one representation: the configured codec when the account
// has it, an SDR one rather than an HDR/Dolby Vision one, and within that the
// highest quality and the heaviest variant.
//
// The order is deliberate, and it is not the obvious one. Quality used to come
// first, with the codec only breaking ties — which made the source setting a
// no-op: an account entitled to 4K got the HDR rung (qn 125) on every episode
// whatever "bili_codec" said, and a client that copied it showed the picture
// with the wrong luminance and colours. A configured codec is a statement about
// what the viewer's machine can decode, so it now wins over a higher rung; if
// the account has no stream in that codec the ladder is used as before.
//
// A rung the player picked comes before all of that: the viewer asked for it by
// name, so the codec preference is applied within that rung (and only within it)
// instead of being allowed to move the picture to another one.
func (d *BilibiliDriver) pickVideo(reps []biliRep) (biliRep, bool) {
	cands := make([]biliRep, 0, len(reps))
	for _, r := range reps {
		if repURL(r) != "" {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		d.selected = 0
		return biliRep{}, false
	}

	// 0. The rung the viewer chose, when the ladder offers it.
	if d.quality > 0 {
		var atRung []biliRep
		for _, r := range cands {
			if r.ID == d.quality {
				atRung = append(atRung, r)
			}
		}
		if len(atRung) > 0 {
			cands = atRung
		}
	}

	// 1. The configured codec, when the ladder actually offers it.
	if d.codec != "" && d.codec != "auto" {
		var matching []biliRep
		for _, r := range cands {
			if codecMatches(strings.ToLower(r.Codecs), d.codec) {
				matching = append(matching, r)
			}
		}
		if len(matching) > 0 {
			cands = matching
		}
	}

	// 2. SDR over HDR/Dolby Vision: a copy of the latter is not the picture the
	// viewer is supposed to see, and no client here tone maps it.
	var sdr []biliRep
	for _, r := range cands {
		if !r.isHDR() {
			sdr = append(sdr, r)
		}
	}
	if len(sdr) > 0 {
		cands = sdr
	}

	best := cands[0]
	for _, r := range cands[1:] {
		if betterRep(r, best, d.codec) {
			best = r
		}
	}
	d.selected = best.ID
	return best, true
}

// betterRep orders two representations that have already passed the codec and
// SDR filters: the higher quality rung first, then the better-ranked codec (a
// no-op when the configured codec was matched above), then the heavier variant
// of the same rung.
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

// pickAudio chooses the audio track to mux: the highest bitrate among the
// tracks the packager can copy.
//
// Copying is the point. bilibili lists its Dolby (ec-3) and Hi-Res (flac) tracks
// in this same array when the title has them, and taking one by bitrate alone
// makes things worse, not better: the packager has no way to carry E-AC-3 or
// FLAC into the output, so it re-encodes the track to 192 kbit/s AAC — measurably
// worse than the plain 320 kbit/s AAC it passed over, and heard as "muffled".
// The AAC ladder is what can be handed through untouched.
func pickAudio(reps []biliRep) (biliRep, bool) {
	best := biliRep{}
	found := false
	var fallback biliRep
	haveFallback := false
	for _, r := range reps {
		if repURL(r) == "" {
			continue
		}
		if !isAACAudio(r.Codecs) {
			if !haveFallback || r.Bandwidth > fallback.Bandwidth {
				fallback, haveFallback = r, true
			}
			continue
		}
		if !found || r.Bandwidth > best.Bandwidth {
			best, found = r, true
		}
	}
	if found {
		return best, true
	}
	// No AAC at all: an unusual answer, but a track is better than silence.
	return fallback, haveFallback
}

// isAACAudio reports whether a DASH codecs string is AAC, the one audio codec
// this source's output can carry without re-encoding it.
func isAACAudio(codecs string) bool {
	c := strings.ToLower(strings.TrimSpace(codecs))
	return strings.HasPrefix(c, "mp4a")
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
	resp, err := d.do(ctx, req)
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
	resp, err := d.do(ctx, req)
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

// PartialListing is true: the root is 我的追番 + 正在更新 + 热门, not the whole
// catalogue. A 番剧 leaving those lists is still there, and must not take its
// library rows with it.
func (d *BilibiliDriver) PartialListing() bool { return true }
