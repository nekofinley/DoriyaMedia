package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// bilibili_account.go: who the configured cookie belongs to, and how to obtain
// one without a browser.
//
// A pasted cookie is a copy of a session a browser is still using, and bilibili
// rotates a web session's SESSDATA from the browser: every rotation invalidates
// the previous value. A copy therefore dies at the first rotation — often days
// after it was taken, however far in the future the expiry baked into the
// SESSDATA string lies. The symptoms are quiet rather than loud: playurl answers
// code 0 with a ladder capped at 480P, and a 会员 episode answers is_preview 1
// with no DASH at all, which reads as "this account has no membership" when the
// account is in fact fine.
//
// Scanning a login QR hands this server a session of its own, with a
// refresh_token that belongs to it alone, so nothing the browser does can
// invalidate it. That is the difference this file exists to make.

const (
	// biliPassportOrigin is the login origin. It is a variable for the same
	// reason as biliAPI: tests point it at a local server.
	// (declared as a var below, not a const)

	// biliAccountTTL is how long one account answer is reused. Every play would
	// otherwise spend an API call on nav, and a dead cookie would spend one per
	// retry; a minute is short enough that a re-login takes effect at once
	// (QRLoginPoll clears the cache explicitly anyway).
	biliAccountTTL = time.Minute

	// biliQRSize is the pixel size of the PNG the settings page draws.
	biliQRSize = 320
)

// biliPassportOrigin is the login origin. A variable so tests can point it at a
// local server, exactly like biliAPI.
var biliPassportOrigin = "https://passport.bilibili.com"

// BiliAccount is who the configured cookie belongs to, as bilibili states it.
type BiliAccount struct {
	LoggedIn bool   `json:"logged_in"`
	Mid      int64  `json:"mid,omitempty"`
	Uname    string `json:"uname,omitempty"`
	Face     string `json:"face,omitempty"`
	Level    int    `json:"level,omitempty"`
	// VIPStatus is bilibili's own 大会员 flag: 1 while the membership is active.
	VIPStatus int `json:"vip_status,omitempty"`
	// VIPType distinguishes the plans (0 none, 1 月度, 2 年度); it is reported
	// for the label, never used to decide whether the account is a member.
	VIPType  int    `json:"vip_type,omitempty"`
	VIPLabel string `json:"vip_label,omitempty"`
	// VIPDue is when the membership runs out, in unix seconds (0 = bilibili did
	// not say, which is what an anonymous answer looks like).
	VIPDue int64 `json:"vip_due,omitempty"`
	// Checked is when bilibili was asked.
	Checked int64 `json:"checked"`
}

// VIPActive reports whether the account may watch 会员 content right now.
func (a *BiliAccount) VIPActive() bool {
	return a != nil && a.LoggedIn && a.VIPStatus == 1
}

// BiliHealth is the account verdict the settings page draws: what was asked,
// what came back, and what it means for playback.
type BiliHealth struct {
	// Configured says a cookie is stored at all. False is not a failure — it is
	// the anonymous mode (free episodes, 480P).
	Configured bool `json:"configured"`
	LoggedIn   bool `json:"logged_in"`
	// Account is bilibili's answer, or nil when the call itself failed.
	Account *BiliAccount `json:"account,omitempty"`
	// CookieExpiry is the moment the stored SESSDATA *claims* to expire, in unix
	// seconds. It is read from the string itself, so it is available without a
	// network call — and it is exactly what misleads: a rotated cookie keeps its
	// claimed date while the server has already forgotten it.
	CookieExpiry int64 `json:"cookie_expiry,omitempty"`
	// CookieExpired is that claim having passed.
	CookieExpired bool `json:"cookie_expired"`
	// Refreshable says a refresh_token is stored, so the session can be renewed
	// without a new scan.
	Refreshable bool `json:"refreshable"`
	// Error is a transport or API failure, as opposed to a dead cookie. A dead
	// cookie is an answer, not an error: bilibili says so plainly.
	Error string `json:"error,omitempty"`
	// Message is the one-line verdict for the settings page.
	Message string `json:"message"`
	// Hint is the second line: what to do about it.
	Hint string `json:"hint,omitempty"`
}

// ---------- cookie parsing ----------

// biliCookieParts splits a cookie header into its name=value pairs, keeping the
// last occurrence of a name (a browser export can repeat one).
func biliCookieParts(cookie string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = strings.TrimSpace(value)
	}
	return out
}

// biliCookieExpiry reads the expiry a SESSDATA carries.
//
// A SESSDATA is "<id>%2C<unix seconds>%2C<signature>" — URL-encoded, so it is
// decoded first. The number is what bilibili put there when it issued the
// cookie; it is not a promise, and the whole point of Health is to contrast it
// with what the API actually says.
func biliCookieExpiry(cookie string) (time.Time, bool) {
	raw := biliCookieParts(cookie)["SESSDATA"]
	if raw == "" {
		return time.Time{}, false
	}
	if dec, err := url.QueryUnescape(raw); err == nil {
		raw = dec
	}
	fields := strings.Split(raw, ",")
	if len(fields) < 2 {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// biliCookieHas reports whether a cookie carries a name with a non-empty value.
func biliCookieHas(cookie, name string) bool {
	return strings.TrimSpace(biliCookieParts(cookie)[name]) != ""
}

// biliCookieMerge folds new values into an existing cookie header: a name that
// is already there is replaced, a new one appended, and the order of the rest is
// kept. The browser export's shape is worth preserving — bilibili's risk control
// is happier with the fields it is used to seeing.
func biliCookieMerge(old string, add map[string]string) string {
	if len(add) == 0 {
		return old
	}
	names := make([]string, 0, len(add))
	for k := range add {
		names = append(names, k)
	}
	sort.Strings(names)

	seen := map[string]bool{}
	parts := make([]string, 0, len(add)+4)
	for _, part := range strings.Split(old, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if v, replace := add[name]; replace {
			if v == "" {
				continue
			}
			parts = append(parts, name+"="+v)
			seen[name] = true
			continue
		}
		parts = append(parts, part)
		seen[name] = true
	}
	for _, name := range names {
		if seen[name] || add[name] == "" {
			continue
		}
		parts = append(parts, name+"="+add[name])
	}
	return strings.Join(parts, "; ")
}

// biliCookieFromURL pulls the credential fields out of the URL a confirmed QR
// poll returns. bilibili hands them over as the query string of a crossDomain
// redirect rather than as Set-Cookie on the API reply, so both are read and this
// is the one that carries the account.
func biliCookieFromURL(raw string) map[string]string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return nil
	}
	q := u.Query()
	out := map[string]string{}
	for _, name := range []string{"SESSDATA", "bili_jct", "DedeUserID", "DedeUserID__ckMd5", "sid"} {
		if v := strings.TrimSpace(q.Get(name)); v != "" {
			out[name] = v
		}
	}
	if _, ok := out["SESSDATA"]; !ok {
		return nil
	}
	return out
}

// biliCookieFromSetCookie reads the same fields out of a response's Set-Cookie
// headers, which is the other shape the poll can take.
func biliCookieFromSetCookie(h http.Header) map[string]string {
	out := map[string]string{}
	for _, line := range h.Values("Set-Cookie") {
		pair, _, _ := strings.Cut(line, ";")
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		switch name = strings.TrimSpace(name); name {
		case "SESSDATA", "bili_jct", "DedeUserID", "DedeUserID__ckMd5", "sid":
			if v := strings.TrimSpace(value); v != "" {
				out[name] = v
			}
		}
	}
	if _, ok := out["SESSDATA"]; !ok {
		return nil
	}
	return out
}

// ---------- account state ----------

type biliAccountItem struct {
	acc *BiliAccount
	err error
	at  time.Time
}

var (
	biliAccountMu    sync.Mutex
	biliAccountCache = map[string]biliAccountItem{}
)

// biliCacheClear drops every cached listing and answer. A login changes what
// every one of them would say, so nothing survives it.
func biliCacheClear() {
	biliCacheMu.Lock()
	biliCache = map[string]biliCacheItem{}
	biliCacheMu.Unlock()

	biliAccountMu.Lock()
	biliAccountCache = map[string]biliAccountItem{}
	biliAccountMu.Unlock()
}

// ClearBiliCache is biliCacheClear for callers outside this package: after a
// login or a refresh, every listing and every ladder read with the old cookie is
// suspect, and serving one of them would report the old account's limits.
func ClearBiliCache() { biliCacheClear() }

// Account asks bilibili who this cookie belongs to.
//
// The answer is cached for a minute — including the "not logged in" answer,
// which is what keeps a dead cookie from spending an API call on every play —
// and keyed by the cookie itself, so storing a new one takes effect at once.
func (d *BilibiliDriver) Account(ctx context.Context) (*BiliAccount, error) {
	key := d.cookie
	biliAccountMu.Lock()
	if it, ok := biliAccountCache[key]; ok && time.Since(it.at) < biliAccountTTL {
		biliAccountMu.Unlock()
		return it.acc, it.err
	}
	biliAccountMu.Unlock()

	acc, err := d.fetchAccount(ctx)

	biliAccountMu.Lock()
	if len(biliAccountCache) > 32 {
		biliAccountCache = map[string]biliAccountItem{}
	}
	biliAccountCache[key] = biliAccountItem{acc: acc, err: err, at: time.Now()}
	biliAccountMu.Unlock()
	return acc, err
}

// fetchAccount is one nav call, typed.
//
// A -101 is an answer, not a failure: it means the cookie is not (or no longer)
// a session, and the account object says so with LoggedIn false. Only a real
// transport or protocol failure is returned as an error — the two lead to
// completely different advice, and confusing them sends the user to fix their
// proxy when what they need is to log in again.
func (d *BilibiliDriver) fetchAccount(ctx context.Context) (*BiliAccount, error) {
	raw, err := d.getJSON(ctx, "/x/web-interface/nav", nil)
	if errors.Is(err, errNotLoggedIn) {
		return &BiliAccount{LoggedIn: false, Checked: time.Now().Unix()}, nil
	}
	if err != nil {
		return nil, err
	}
	var nav struct {
		IsLogin bool   `json:"isLogin"`
		Mid     int64  `json:"mid"`
		Uname   string `json:"uname"`
		Face    string `json:"face"`
		Level   struct {
			Current int `json:"current_level"`
		} `json:"level_info"`
		VIPStatus  int    `json:"vipStatus"`
		VIPType    int    `json:"vipType"`
		VIPLabel   string `json:"vip_label"`
		VIPDueDate int64  `json:"vipDueDate"` // milliseconds
	}
	if err := json.Unmarshal(raw, &nav); err != nil {
		return nil, fmt.Errorf("bilibili: 账号信息无法解析: %w", err)
	}
	acc := &BiliAccount{
		LoggedIn: nav.IsLogin, Mid: nav.Mid, Uname: strings.TrimSpace(nav.Uname),
		Face: nav.Face, Level: nav.Level.Current,
		VIPStatus: nav.VIPStatus, VIPType: nav.VIPType,
		VIPLabel: strings.TrimSpace(nav.VIPLabel),
		Checked:  time.Now().Unix(),
	}
	if nav.VIPDueDate > 0 {
		acc.VIPDue = nav.VIPDueDate / 1000
	}
	if acc.VIPLabel == "" && acc.VIPStatus == 1 {
		acc.VIPLabel = "大会员"
	}
	return acc, nil
}

// Health is the verdict the settings page draws. It never fails: a dead cookie
// and an unreachable API are both answers, and telling them apart is the whole
// point — one is fixed by logging in again, the other by fixing the proxy.
func (d *BilibiliDriver) Health(ctx context.Context, refreshable bool) *BiliHealth {
	h := &BiliHealth{Configured: d.cookie != "", Refreshable: refreshable}
	if exp, ok := biliCookieExpiry(d.cookie); ok {
		h.CookieExpiry = exp.Unix()
		h.CookieExpired = time.Now().After(exp)
	}
	if !h.Configured {
		h.Message = "未配置账号:只能看免费集,清晰度最高 480P"
		h.Hint = "扫码登录或粘贴浏览器 Cookie 后,会员集与 1080P+ / 4K 才会出现"
		return h
	}
	acc, err := d.Account(ctx)
	if err != nil {
		h.Error = err.Error()
		h.Message = "连不上 bilibili:" + err.Error()
		h.Hint = "这是网络或代理的问题,不是 Cookie 的问题。检查存储源上的代理设置后再试"
		return h
	}
	h.Account = acc
	h.LoggedIn = acc.LoggedIn
	// Expiry is checked before VIPStatus, not after: bilibili normally flips
	// vipStatus to 0 the moment a membership lapses, but a cached or rounded
	// answer can still say 1 on the last day. "已登录,大会员有效" for an account
	// whose membership ran out yesterday is the same class of lie this whole
	// check exists to stop.
	expired := acc.VIPStatus == 1 && acc.VIPDue > 0 && time.Now().Unix() > acc.VIPDue
	switch {
	case !acc.LoggedIn:
		h.Message = "Cookie 已失效(账号未登录):会员集看不了,清晰度被压到 480P"
		if h.CookieExpiry > 0 && !h.CookieExpired {
			// The misleading case, and the common one: the string still claims
			// months of life because a rotation never rewrites the copy.
			h.Hint = "注意 SESSDATA 里写的有效期还没到 —— 浏览器每次刷新登录态都会作废旧值," +
				"复制出来的 Cookie 就是这样提前失效的。重新扫码即可"
		} else {
			h.Hint = "重新扫码登录,或从浏览器重新复制 Cookie"
		}
	case expired:
		h.Message = "已登录 " + acc.Uname + ",大会员已于 " +
			time.Unix(acc.VIPDue, 0).Format("2006-01-02") + " 到期"
		h.Hint = "会员集需要重新开通大会员;免费集不受影响"
	case acc.VIPActive():
		h.Message = "已登录 " + acc.Uname + ",大会员有效"
		if acc.VIPDue > 0 {
			h.Message += "(至 " + time.Unix(acc.VIPDue, 0).Format("2006-01-02") + ")"
		}
		h.Hint = "会员集与高码率可用"
	default:
		h.Message = "已登录 " + acc.Uname + ",但当前不是大会员"
		h.Hint = "会员集需要大会员;清晰度上限取决于账号等级"
	}
	return h
}

// ---------- QR login ----------

// BiliQR is a login QR waiting to be scanned.
type BiliQR struct {
	// Key identifies this attempt; the poll endpoint takes it back.
	Key string `json:"key"`
	// URL is what the code encodes — bilibili's own h5 login page.
	URL string `json:"url"`
	// PNG is the code as a data: URL, so the settings page needs no extra
	// request and no image host.
	PNG string `json:"png"`
	// ExpiresIn is how long bilibili keeps the code valid, in seconds.
	ExpiresIn int `json:"expires_in"`
}

// BiliQRState is one poll of a login QR.
type BiliQRState struct {
	// State is "waiting" (nobody scanned), "scanned" (scanned, not confirmed),
	// "confirmed" (logged in) or "expired".
	State   string `json:"state"`
	Message string `json:"message"`
	// Cookie and RefreshToken are the credentials, set only when State is
	// "confirmed". They are not serialised: the caller stores them and answers
	// with the account, never with the credential.
	Cookie       string       `json:"-"`
	RefreshToken string       `json:"-"`
	Account      *BiliAccount `json:"account,omitempty"`
}

// biliPassportDo sends one request to the login origin through this source's own
// client, so a machine that needs a proxy to reach bilibili needs no second
// configuration for logging in.
//
// withCookie is separate from the other calls on purpose: generating and polling
// a login QR must work with *no* session, because the whole reason to log in
// again is that the stored one is dead. Sending it would at best be noise and at
// worst tie the new session to the old, broken one. Refreshing, on the other
// hand, is exactly a request made *with* the session it is renewing.
func (d *BilibiliDriver) biliPassportDo(ctx context.Context, method, rawURL string, params url.Values, withCookie bool) (*http.Response, error) {
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		rawURL += sep + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	h := d.headers()
	if !withCookie {
		delete(h, "Cookie")
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return d.do(ctx, req)
}

// biliPassportJSON unwraps one passport answer: {"code":0,"message":"0","data":…}.
func biliPassportJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bilibili: 登录接口返回 HTTP %d", resp.StatusCode)
	}
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return err
	}
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("bilibili: 登录接口响应无法解析: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("bilibili: %s(%d)", strings.TrimSpace(env.Message), env.Code)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("bilibili: 登录接口响应无法解析: %w", err)
		}
	}
	return nil
}

// QRLoginStart asks for a fresh login QR. No account is needed: the endpoint is
// public, and that is what makes it usable when the stored cookie is already
// dead.
func (d *BilibiliDriver) QRLoginStart(ctx context.Context) (*BiliQR, error) {
	resp, err := d.biliPassportDo(ctx, "GET", biliPassportOrigin+"/x/passport-login/web/qrcode/generate", nil, false)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 申请扫码登录失败: %w", err)
	}
	var data struct {
		URL string `json:"url"`
		Key string `json:"qrcode_key"`
	}
	if err := biliPassportJSON(resp, &data); err != nil {
		return nil, err
	}
	data.URL, data.Key = strings.TrimSpace(data.URL), strings.TrimSpace(data.Key)
	if data.URL == "" || data.Key == "" {
		return nil, fmt.Errorf("bilibili: 扫码登录接口没有给出二维码")
	}
	png, err := qrcode.Encode(data.URL, qrcode.Medium, biliQRSize)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 生成二维码失败: %w", err)
	}
	return &BiliQR{
		Key: data.Key, URL: data.URL,
		PNG:       "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		ExpiresIn: 180,
	}, nil
}

// QRLoginPoll asks whether the QR has been scanned and confirmed yet.
//
// The confirmed answer carries the account's credential; it is returned to the
// caller to store, and the account itself is resolved here so the settings page
// can show who just logged in without a second round trip.
func (d *BilibiliDriver) QRLoginPoll(ctx context.Context, key string) (*BiliQRState, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("bilibili: 缺少扫码标识")
	}
	resp, err := d.biliPassportDo(ctx, "GET",
		biliPassportOrigin+"/x/passport-login/web/qrcode/poll", url.Values{"qrcode_key": {key}}, false)
	if err != nil {
		return nil, fmt.Errorf("bilibili: 查询扫码状态失败: %w", err)
	}
	// Read the headers before the body is closed.
	fromHeaders := biliCookieFromSetCookie(resp.Header)

	var data struct {
		URL          string `json:"url"`
		RefreshToken string `json:"refresh_token"`
		// Code is the QR state, and it sits inside data — the envelope's own
		// code is 0 for every one of these answers, including "expired".
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := biliPassportJSON(resp, &data); err != nil {
		return nil, err
	}
	switch data.Code {
	case 86101:
		return &BiliQRState{State: "waiting", Message: "等待扫码"}, nil
	case 86090:
		return &BiliQRState{State: "scanned", Message: "已扫码,请在手机上点确认"}, nil
	case 86038:
		return &BiliQRState{State: "expired", Message: "二维码已过期,请刷新"}, nil
	case 0:
	default:
		msg := strings.TrimSpace(data.Message)
		if msg == "" {
			msg = fmt.Sprintf("扫码状态异常(%d)", data.Code)
		}
		return nil, fmt.Errorf("bilibili: %s", msg)
	}

	add := biliCookieFromURL(data.URL)
	if add == nil {
		add = fromHeaders
	}
	if add == nil {
		return nil, fmt.Errorf("bilibili: 扫码成功但没拿到登录凭据,请重试")
	}
	cookie := biliCookieMerge("", add)
	// The account is read with a driver built on the new cookie: the stored one
	// is the dead credential this login exists to replace.
	fresh := &BilibiliDriver{cookie: cookie, client: d.client, codec: d.codec}
	acc, accErr := fresh.fetchAccount(ctx)
	state := &BiliQRState{
		State: "confirmed", Message: "登录成功",
		Cookie: cookie, RefreshToken: strings.TrimSpace(data.RefreshToken),
		Account: acc,
	}
	if accErr != nil {
		// The login itself worked; only the confirmation call failed. Say so
		// rather than throwing the credential away.
		state.Message = "登录成功(账号信息稍后刷新:" + accErr.Error() + ")"
	}
	return state, nil
}

// RefreshCookie rotates the stored session with the refresh_token that came with
// it.
//
// This is the mechanism that makes a scanned session outlive a pasted one: the
// browser does exactly this call on a timer, and a session that keeps doing it
// never expires. Both halves — the cookie and the new refresh_token — have to be
// stored together, because the old refresh_token is dead the moment this
// succeeds.
func (d *BilibiliDriver) RefreshCookie(ctx context.Context, refreshToken string) (cookie, newRefresh string, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", fmt.Errorf("bilibili: 这条会话没有 refresh_token,只能重新扫码")
	}
	csrf := biliCookieParts(d.cookie)["bili_jct"]
	resp, err := d.biliPassportDo(ctx, "POST",
		biliPassportOrigin+"/x/passport-login/web/cookie/refresh",
		url.Values{
			"refresh_token": {refreshToken},
			"csrf":          {csrf},
			"source":        {"main_web"},
		}, true)
	if err != nil {
		return "", "", fmt.Errorf("bilibili: 续期失败: %w", err)
	}
	fromHeaders := biliCookieFromSetCookie(resp.Header)

	var data struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := biliPassportJSON(resp, &data); err != nil {
		return "", "", err
	}
	if fromHeaders == nil {
		return "", "", fmt.Errorf("bilibili: 续期接口没有返回新的 Cookie")
	}
	return biliCookieMerge(d.cookie, fromHeaders), strings.TrimSpace(data.RefreshToken), nil
}

// readLimited reads a response body up to n bytes. A login answer is a few
// hundred bytes; the limit is there so a wrong host behind a proxy cannot hand
// back a video instead.
func readLimited(r io.Reader, n int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > n {
		return nil, fmt.Errorf("bilibili: 登录接口响应过大")
	}
	return raw, nil
}
