package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bilibili_account_test.go: the account half of the source.
//
// The failure these tests are about is the quiet one: a pasted browser cookie
// dies when the browser rotates the session, and nothing reports it. playurl
// answers code 0 with a ladder capped at 480P, and a 会员 episode answers
// is_preview 1 — which reads as "this account has no membership" on an account
// that does. Every assertion here is about naming that correctly.

// ---------- cookie parsing ----------

// A SESSDATA carries the expiry bilibili wrote into it, URL-encoded. Reading it
// is what lets the settings page contrast "says 2027" with "bilibili says you
// are not logged in" — the exact confusion a rotated cookie causes.
func TestBiliCookieExpiry(t *testing.T) {
	// 1807764825 = 2027-04-15 in +08:00.
	cookie := "SESSDATA=2eab3e7d%2C1807764825%2C3b291%2Axx; bili_jct=abc; buvid3=def"
	got, ok := biliCookieExpiry(cookie)
	if !ok {
		t.Fatal("no expiry read from a well-formed SESSDATA")
	}
	if got.Unix() != 1807764825 {
		t.Fatalf("expiry = %s (%d), want 1807764825", got, got.Unix())
	}
	// An unencoded comma is what some exports produce; it has to read the same.
	if got2, ok := biliCookieExpiry("SESSDATA=2eab3e7d,1807764825,3b291"); !ok || got2.Unix() != 1807764825 {
		t.Fatalf("unencoded SESSDATA read as %v/%v", got2, ok)
	}
	for _, bad := range []string{"", "bili_jct=abc", "SESSDATA=", "SESSDATA=only-one-field", "SESSDATA=x,notanumber,y"} {
		if _, ok := biliCookieExpiry(bad); ok {
			t.Errorf("biliCookieExpiry(%q) claimed an expiry", bad)
		}
	}
}

// A login replaces the credential fields and leaves the rest of the browser
// export alone: bilibili's risk control is happier with the fields it expects.
func TestBiliCookieMerge(t *testing.T) {
	old := "buvid3=device; PVID=1; SESSDATA=stale; bili_jct=old"
	got := biliCookieMerge(old, map[string]string{
		"SESSDATA": "fresh", "bili_jct": "new", "DedeUserID": "42",
	})
	for _, want := range []string{"SESSDATA=fresh", "bili_jct=new", "DedeUserID=42", "buvid3=device", "PVID=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged cookie %q is missing %q", got, want)
		}
	}
	for _, gone := range []string{"SESSDATA=stale", "bili_jct=old"} {
		if strings.Contains(got, gone) {
			t.Errorf("merged cookie still carries the stale value %q", gone)
		}
	}
	if n := strings.Count(got, "SESSDATA="); n != 1 {
		t.Errorf("merged cookie has %d SESSDATA fields, want 1: %q", n, got)
	}
	// An empty value is a removal, which is how a field gets dropped.
	if out := biliCookieMerge("a=1; b=2", map[string]string{"b": ""}); strings.Contains(out, "b=") {
		t.Errorf("an empty replacement kept the field: %q", out)
	}
	if out := biliCookieMerge("a=1", nil); out != "a=1" {
		t.Errorf("a nil merge changed the cookie: %q", out)
	}
}

// The confirmed poll hands the credential over as the query string of a
// crossDomain redirect, not as Set-Cookie. Reading only one of the two shapes is
// how a scan-to-login would report success and store nothing.
func TestBiliCookieFromURL(t *testing.T) {
	raw := "https://passport.biligame.com/crossDomain?DedeUserID=42&DedeUserID__ckMd5=m&" +
		"Expires=1807764825&SESSDATA=abc%2C1807764825%2Cdef&bili_jct=csrf&gourl=https%3A%2F%2Fwww.bilibili.com"
	got := biliCookieFromURL(raw)
	if got["SESSDATA"] != "abc,1807764825,def" {
		t.Fatalf("SESSDATA = %q, want the decoded value", got["SESSDATA"])
	}
	if got["bili_jct"] != "csrf" || got["DedeUserID"] != "42" {
		t.Fatalf("credential fields = %+v", got)
	}
	if _, ok := got["gourl"]; ok {
		t.Error("an unrelated query field was taken for a cookie")
	}
	if biliCookieFromURL("https://example.test/x?a=1") != nil {
		t.Error("a URL without SESSDATA produced a cookie")
	}
	if biliCookieFromURL("://not a url") != nil {
		t.Error("a malformed URL produced a cookie")
	}
}

func TestBiliCookieFromSetCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "SESSDATA=abc%2C1%2Cx; Path=/; Domain=.bilibili.com")
	h.Add("Set-Cookie", "bili_jct=csrf; Path=/")
	h.Add("Set-Cookie", "buvid3=ignored; Path=/")
	got := biliCookieFromSetCookie(h)
	if got["SESSDATA"] != "abc%2C1%2Cx" || got["bili_jct"] != "csrf" {
		t.Fatalf("Set-Cookie fields = %+v", got)
	}
	if _, ok := got["buvid3"]; ok {
		t.Error("a non-credential cookie was collected")
	}
	if biliCookieFromSetCookie(http.Header{}) != nil {
		t.Error("an empty header produced a cookie")
	}
}

// ---------- account state ----------

// accountFixture is the shape /x/web-interface/nav answers with, reduced to what
// the driver reads.
//
// vip_label is an OBJECT in the live answer — {"path","text","label_theme",
// "text_color",…} — and was a plain string in older ones. The fixture uses the
// live shape on purpose: it used to use the string, which is exactly why a
// struct that declared `VIPLabel string` passed every test here and then failed
// against the real API, taking the whole account check down with it.
func accountFixture(loggedIn bool, uname string, vipStatus int, vipDueMS int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"code": 0, "message": "0",
		"data": map[string]any{
			"isLogin":    loggedIn,
			"mid":        42,
			"uname":      uname,
			"vipStatus":  vipStatus,
			"vipType":    2,
			"vipDueDate": vipDueMS,
			"vip_label": map[string]any{
				"path":        "http://i0.hdslb.com/bfs/vip/label_annual.png",
				"text":        "年度大会员",
				"label_theme": "annual_vip",
				"text_color":  "#FFFFFF",
				"bg_style":    1,
				"bg_color":    "#FB7299",
			},
			"level_info": map[string]any{"current_level": 6},
			// Fields the driver does not read, present in the live answer and
			// capable of changing shape at any time. None of them may break the
			// one call that answers "am I logged in".
			"pendant":  map[string]any{"pid": 0, "name": "", "expire": 0},
			"wallet":   map[string]any{"mid": 42, "bcoin_balance": 0},
			"official": map[string]any{"role": 0, "title": "", "type": -1},
			"wbi_img":  map[string]any{"img_url": "x", "sub_url": "y"},
		},
	})
	return body
}

// A live membership is reported as one — and, just as important, a dead cookie is
// not reported as "you have no membership", which is the mistake that sends a
// paying member to buy what they already own.
func TestHealthDistinguishesDeadCookieFromNoMembership(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	due := time.Now().Add(200 * 24 * time.Hour).UnixMilli()
	cases := []struct {
		name       string
		cookie     string
		nav        []byte
		wantMsg    string
		wantDead   bool
		wantHint   string
		configured bool
	}{
		{
			name: "no cookie at all", cookie: "", nav: accountFixture(false, "", 0, 0),
			wantMsg: "未配置账号", configured: false,
		},
		{
			name: "live VIP", cookie: "SESSDATA=x%2C9999999999%2Cy", nav: accountFixture(true, "某人", 1, due),
			wantMsg: "大会员有效", configured: true,
		},
		{
			// The case this whole change exists for: the string still claims a
			// future expiry, bilibili says nobody is logged in.
			name:   "rotated cookie, still claims a future expiry",
			cookie: "SESSDATA=x%2C1807764825%2Cy", nav: accountFixture(false, "", 0, 0),
			wantMsg: "Cookie 已失效", wantDead: true, wantHint: "还没到", configured: true,
		},
		{
			name:   "logged in but the membership ran out",
			cookie: "SESSDATA=x%2C1%2Cy", nav: accountFixture(true, "某人", 1, time.Now().Add(-48*time.Hour).UnixMilli()),
			wantMsg: "到期", configured: true,
		},
		{
			name:   "logged in, never a member",
			cookie: "SESSDATA=x%2C1%2Cy", nav: accountFixture(true, "某人", 0, 0),
			wantMsg: "不是大会员", configured: true,
		},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/x/web-interface/nav" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(c.nav)
		}))
		oldAPI := biliAPI
		biliAPI = srv.URL
		d := &BilibiliDriver{cookie: c.cookie, codec: "avc", client: srv.Client()}
		h := d.Health(context.Background(), false)
		srv.Close()
		biliAPI = oldAPI
		resetBiliCache()

		if h.Configured != c.configured {
			t.Errorf("%s: configured = %v, want %v", c.name, h.Configured, c.configured)
		}
		if !strings.Contains(h.Message, c.wantMsg) {
			t.Errorf("%s: message %q does not mention %q", c.name, h.Message, c.wantMsg)
		}
		if errors.Is(errCookieDead, nil) && c.wantDead && h.LoggedIn {
			t.Errorf("%s: a dead cookie was reported as logged in", c.name)
		}
		if c.wantHint != "" && !strings.Contains(h.Hint, c.wantHint) {
			t.Errorf("%s: hint %q does not explain the expiry contradiction (%q)", c.name, h.Hint, c.wantHint)
		}
	}
}

// A transport failure is not a dead cookie: one is fixed by logging in, the other
// by fixing the proxy, and telling a user the wrong one wastes their afternoon.
func TestHealthSeparatesNetworkFailureFromDeadCookie(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=x%2C1%2Cy", codec: "avc", client: srv.Client()}
	h := d.Health(context.Background(), false)
	if h.Error == "" {
		t.Fatalf("a 502 was reported as a verdict: %+v", h)
	}
	if strings.Contains(h.Message, "Cookie 已失效") {
		t.Errorf("a network failure was blamed on the cookie: %q", h.Message)
	}
	if !strings.Contains(h.Message, "连不上") {
		t.Errorf("message %q does not say the API was unreachable", h.Message)
	}
}
// ---------- refusal wording ----------

// The playurl answer cannot tell "no membership" from "no session": both are
// code 0, is_preview 1, no DASH. The nav call is what separates them, and the
// wording has to follow it — otherwise a paying member is told to buy a
// membership they already have.
func TestRefusalIsAttributedToTheCookieWhenNobodyIsLoggedIn(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			_, _ = w.Write(accountFixture(false, "", 0, 0))
		case "/pgc/player/web/playurl":
			// A 会员 episode with no session: the trial answer, no DASH.
			_, _ = w.Write([]byte(`{"code":0,"message":"success","result":{"is_preview":1,"quality":32}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=dead", codec: "avc", client: srv.Client()}
	_, err := d.playInfo(context.Background(), biliEpisodeRef{Cid: 1, Aid: 2}, 3)
	if err == nil {
		t.Fatal("a trial answer resolved to streams")
	}
	if !errors.Is(err, errCookieDead) {
		t.Fatalf("a dead-cookie refusal was not classified as one: %v", err)
	}
	if !strings.Contains(err.Error(), "Cookie 已失效") {
		t.Errorf("message %q does not lead with the real cause", err)
	}
	if !strings.Contains(err.Error(), "不需要重新开通大会员") {
		t.Errorf("message %q does not stop the user from buying a second membership", err)
	}
}

// With a live session the same answer keeps its own wording: there the
// membership really is the missing piece.
func TestRefusalKeepsTheMembershipWordingWhenLoggedIn(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			_, _ = w.Write(accountFixture(true, "某人", 0, 0))
		case "/pgc/player/web/playurl":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","result":{"is_preview":1,"quality":32}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=live", codec: "avc", client: srv.Client()}
	_, err := d.playInfo(context.Background(), biliEpisodeRef{Cid: 1, Aid: 2}, 3)
	if err == nil {
		t.Fatal("a trial answer resolved to streams")
	}
	if errors.Is(err, errCookieDead) {
		t.Fatalf("a logged-in refusal was blamed on the cookie: %v", err)
	}
	if !strings.Contains(err.Error(), "大会员") {
		t.Errorf("message %q does not mention the membership", err)
	}
}

// A region lock is bilibili's verdict about the route, not about the account, and
// must not be re-labelled as a cookie problem.
func TestRefusalLeavesRegionLockAlone(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			_, _ = w.Write(accountFixture(false, "", 0, 0))
		case "/pgc/player/web/playurl":
			_, _ = w.Write([]byte(`{"code":-10403,"message":"抱歉您所在地区不可观看！"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{codec: "avc", client: srv.Client()}
	_, err := d.playInfo(context.Background(), biliEpisodeRef{Cid: 1, Aid: 2}, 3)
	if !errors.Is(err, errRegionLocked) {
		t.Fatalf("a region lock was not classified as one: %v", err)
	}
	if errors.Is(err, errCookieDead) {
		t.Fatalf("a region lock was blamed on the cookie: %v", err)
	}
}

// ---------- QR login ----------

// A confirmed poll stores the credential the redirect carries, and reports who
// logged in. The QR flow is the fix for a dead cookie, so it has to work with the
// stored one already dead — which is why the generate call sends no cookie at all.
func TestQRLoginStoresTheCredentialFromTheConfirmedPoll(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var generateSentCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/passport-login/web/qrcode/generate":
			generateSentCookie = r.Header.Get("Cookie")
			_, _ = w.Write([]byte(`{"code":0,"message":"0","data":{"url":"https://account.bilibili.com/h5/scan?qrcode_key=k1","qrcode_key":"k1"}}`))
		case "/x/passport-login/web/qrcode/poll":
			if got := r.URL.Query().Get("qrcode_key"); got != "k1" {
				t.Errorf("poll sent qrcode_key=%q, want k1", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"0","data":{` +
				`"url":"https://passport.biligame.com/crossDomain?DedeUserID=42&SESSDATA=new%2C1807764825%2Cx&bili_jct=csrf",` +
				`"refresh_token":"rt-1","code":0,"message":"登录成功"}}`))
		case "/x/web-interface/nav":
			// The new cookie must be what the confirmation is read with.
			if !strings.Contains(r.Header.Get("Cookie"), "SESSDATA=new") {
				t.Errorf("the confirmation was read with the old credential: %q", r.Header.Get("Cookie"))
			}
			_, _ = w.Write(accountFixture(true, "某人", 1, time.Now().Add(100*24*time.Hour).UnixMilli()))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI, oldPassport := biliAPI, biliPassportOrigin
	biliAPI, biliPassportOrigin = srv.URL, srv.URL
	defer func() { biliAPI, biliPassportOrigin = oldAPI, oldPassport }()

	// The stored cookie is the dead one this login exists to replace.
	d := &BilibiliDriver{cookie: "SESSDATA=dead", codec: "avc", client: srv.Client()}

	qr, err := d.QRLoginStart(context.Background())
	if err != nil {
		t.Fatalf("QRLoginStart: %v", err)
	}
	if qr.Key != "k1" || !strings.HasPrefix(qr.PNG, "data:image/png;base64,") {
		t.Fatalf("QR = key %q, png %q", qr.Key, qr.PNG[:min(30, len(qr.PNG))])
	}
	if generateSentCookie != "" {
		t.Errorf("the QR request carried the dead cookie (%q); it must work without one", generateSentCookie)
	}

	st, err := d.QRLoginPoll(context.Background(), "k1")
	if err != nil {
		t.Fatalf("QRLoginPoll: %v", err)
	}
	if st.State != "confirmed" {
		t.Fatalf("state = %q, want confirmed", st.State)
	}
	if !strings.Contains(st.Cookie, "SESSDATA=new") || !strings.Contains(st.Cookie, "bili_jct=csrf") {
		t.Errorf("stored cookie %q is missing the credential the redirect carried", st.Cookie)
	}
	if st.RefreshToken != "rt-1" {
		t.Errorf("refresh_token = %q, want rt-1 (it is what keeps the session alive)", st.RefreshToken)
	}
	if st.Account == nil || !st.Account.LoggedIn || st.Account.Uname != "某人" {
		t.Errorf("confirmed account = %+v", st.Account)
	}
}

// Every non-final poll answer is a state, not an error: the settings page polls
// this every second or two and must not paint an error for "not scanned yet".
func TestQRLoginPollStates(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	cases := map[string]struct{ body, state string }{
		"86101": {`{"code":0,"message":"0","data":{"code":86101,"message":"未扫码"}}`, "waiting"},
		"86090": {`{"code":0,"message":"0","data":{"code":86090,"message":"二维码已扫码未确认"}}`, "scanned"},
		"86038": {`{"code":0,"message":"0","data":{"code":86038,"message":"二维码已失效"}}`, "expired"},
	}
	for name, c := range cases {
		body := c.body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		oldPassport := biliPassportOrigin
		biliPassportOrigin = srv.URL
		d := &BilibiliDriver{client: srv.Client()}
		st, err := d.QRLoginPoll(context.Background(), "k")
		srv.Close()
		biliPassportOrigin = oldPassport
		if err != nil {
			t.Errorf("%s: poll answered with an error: %v", name, err)
			continue
		}
		if st.State != c.state {
			t.Errorf("%s: state = %q, want %q", name, st.State, c.state)
		}
	}
	// A confirmed answer with no credential anywhere is a failure, not a login.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"0","data":{"code":0,"message":"登录成功","url":"","refresh_token":"rt"}}`))
	}))
	defer srv.Close()
	oldPassport := biliPassportOrigin
	biliPassportOrigin = srv.URL
	defer func() { biliPassportOrigin = oldPassport }()
	d := &BilibiliDriver{client: srv.Client()}
	if _, err := d.QRLoginPoll(context.Background(), "k"); err == nil {
		t.Fatal("a confirmed poll without a credential was accepted")
	}
}

// A refresh rotates both halves and hands back the merged cookie. Keeping the old
// refresh token would leave the source with one that is already dead.
func TestRefreshCookieRotatesBothHalves(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var sentCSRF, sentToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/passport-login/web/cookie/refresh" {
			http.NotFound(w, r)
			return
		}
		sentToken = r.URL.Query().Get("refresh_token")
		sentCSRF = r.URL.Query().Get("csrf")
		w.Header().Add("Set-Cookie", "SESSDATA=rotated%2C1807764825%2Cz; Path=/")
		w.Header().Add("Set-Cookie", "bili_jct=csrf2; Path=/")
		_, _ = w.Write([]byte(`{"code":0,"message":"0","data":{"refresh_token":"rt-2","status":0}}`))
	}))
	defer srv.Close()
	oldPassport := biliPassportOrigin
	biliPassportOrigin = srv.URL
	defer func() { biliPassportOrigin = oldPassport }()

	d := &BilibiliDriver{cookie: "SESSDATA=old%2C1%2Cx; bili_jct=csrf1; buvid3=dev", client: srv.Client()}
	cookie, refresh, err := d.RefreshCookie(context.Background(), "rt-1")
	if err != nil {
		t.Fatalf("RefreshCookie: %v", err)
	}
	if sentToken != "rt-1" {
		t.Errorf("refresh_token sent = %q, want rt-1", sentToken)
	}
	if sentCSRF != "csrf1" {
		t.Errorf("csrf sent = %q, want the cookie's own bili_jct", sentCSRF)
	}
	if !strings.Contains(cookie, "SESSDATA=rotated%2C1807764825%2Cz") || !strings.Contains(cookie, "bili_jct=csrf2") {
		t.Errorf("rotated cookie %q does not carry the new values", cookie)
	}
	if !strings.Contains(cookie, "buvid3=dev") {
		t.Errorf("rotated cookie %q lost the device id", cookie)
	}
	if refresh != "rt-2" {
		t.Errorf("refresh token = %q, want rt-2", refresh)
	}
	// No token means no refresh, and the answer has to say so rather than
	// silently do nothing.
	if _, _, err := d.RefreshCookie(context.Background(), "  "); err == nil {
		t.Fatal("a refresh without a token was attempted")
	}
}

// A failed refresh must not be reported as a success with empty values: the
// caller stores what it is given.
func TestRefreshCookieFailureIsAnError(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"message":"账号未登录","data":null}`))
	}))
	defer srv.Close()
	oldPassport := biliPassportOrigin
	biliPassportOrigin = srv.URL
	defer func() { biliPassportOrigin = oldPassport }()

	d := &BilibiliDriver{cookie: "bili_jct=csrf", client: srv.Client()}
	if _, _, err := d.RefreshCookie(context.Background(), "rt"); err == nil {
		t.Fatal("a -101 refresh was accepted")
	}
}

// The QR is a real PNG of the URL bilibili gave, so the page can draw it without
// any extra request or dependency.
func TestQRLoginStartEncodesTheURL(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var want string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want = "https://account.bilibili.com/h5/account-h5/auth/scan-web?qrcode_key=abc"
		body, _ := json.Marshal(map[string]any{
			"code": 0, "message": "0",
			"data": map[string]any{"url": want, "qrcode_key": "abc"},
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	oldPassport := biliPassportOrigin
	biliPassportOrigin = srv.URL
	defer func() { biliPassportOrigin = oldPassport }()

	d := &BilibiliDriver{client: srv.Client()}
	qr, err := d.QRLoginStart(context.Background())
	if err != nil {
		t.Fatalf("QRLoginStart: %v", err)
	}
	if qr.URL != want || qr.Key != "abc" {
		t.Fatalf("QR = %+v", qr)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(qr.PNG, prefix) {
		t.Fatalf("QR payload is not a data: PNG: %.40q", qr.PNG)
	}
	if len(qr.PNG) < len(prefix)+200 {
		t.Fatalf("QR payload is %d bytes — too short to be a code", len(qr.PNG))
	}
	if qr.ExpiresIn <= 0 {
		t.Errorf("ExpiresIn = %d", qr.ExpiresIn)
	}
}

// The account answer is cached so a dead cookie does not cost an API call per
// play, but the cache is keyed by the cookie: storing a new one takes effect at
// once rather than a minute later.
func TestAccountCacheIsKeyedByCookieAndClearable(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only nav is counted: a driver whose cookie carries no buvid3 also
		// fetches the device id once, and that call says nothing about caching.
		if r.URL.Path != "/x/web-interface/nav" {
			http.NotFound(w, r)
			return
		}
		calls++
		loggedIn := strings.Contains(r.Header.Get("Cookie"), "SESSDATA=fresh")
		_, _ = w.Write(accountFixture(loggedIn, "某人", 1, 0))
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	dead := &BilibiliDriver{cookie: "SESSDATA=dead", client: srv.Client()}
	if acc, err := dead.Account(context.Background()); err != nil || acc.LoggedIn {
		t.Fatalf("dead cookie account = %+v, err %v", acc, err)
	}
	if _, err := dead.Account(context.Background()); err != nil {
		t.Fatalf("second Account: %v", err)
	}
	if calls != 1 {
		t.Errorf("the account was asked %d times, want 1 (it is cached)", calls)
	}

	fresh := &BilibiliDriver{cookie: "SESSDATA=fresh", client: srv.Client()}
	if acc, err := fresh.Account(context.Background()); err != nil || !acc.LoggedIn {
		t.Fatalf("a new cookie reused the old answer: %+v, err %v", acc, err)
	}

	ClearBiliCache()
	if _, err := dead.Account(context.Background()); err != nil {
		t.Fatalf("Account after clear: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls after clear = %d, want 3 (the cache was dropped)", calls)
	}
}

// A cookie whose SESSDATA claims a future expiry is still dead if bilibili says
// so — and Health has to say both things, because "it expires in 2027" is the
// reason nobody suspects the cookie.
func TestHealthReadsTheClaimedExpiryWithoutTheAPI(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(accountFixture(false, "", 0, 0))
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=id%2C1807764825%2Csig", client: srv.Client()}
	h := d.Health(context.Background(), false)
	if h.CookieExpiry != 1807764825 {
		t.Fatalf("CookieExpiry = %d", h.CookieExpiry)
	}
	if h.CookieExpired {
		t.Error("a 2027 expiry was reported as passed")
	}
	if !strings.Contains(h.Hint, "还没到") {
		t.Errorf("hint %q does not point out that the claimed expiry has not passed", h.Hint)
	}
}

// A -101 from nav is the answer the whole check exists to give, not a failure.
// Reporting it as a transport error sends the user to fix their proxy when what
// they need is to log in again — which is exactly the bug the live run caught.
func TestAccountReadsMinusOneAsLoggedOutNotAsAnError(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"message":"账号未登录","data":{"isLogin":false,"wbi_img":{"img_url":"x","sub_url":"y"}}}`))
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=dead", codec: "avc", client: srv.Client()}
	acc, err := d.Account(context.Background())
	if err != nil {
		t.Fatalf("-101 was reported as a failure: %v", err)
	}
	if acc == nil || acc.LoggedIn {
		t.Fatalf("account = %+v, want a logged-out answer", acc)
	}

	h := d.Health(context.Background(), false)
	if h.Error != "" {
		t.Errorf("health reported an error for a plain logged-out answer: %q", h.Error)
	}
	if !strings.Contains(h.Message, "Cookie 已失效") {
		t.Errorf("message %q does not name the dead cookie", h.Message)
	}
	if strings.Contains(h.Hint, "代理") {
		t.Errorf("hint %q blames the proxy for a dead cookie", h.Hint)
	}
}

// vip_label has been a string and is now an object, and it is a decoration: the
// membership is decided by vipStatus. A label shape this build does not know must
// therefore cost the label, never the account — the failure mode that made the
// settings page say "连不上 bilibili" while the picture was fine.
func TestVIPLabelShapesNeverFailTheAccount(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	shapes := map[string]string{
		"object (the live shape)": `{"path":"http://x/label.png","text":"年度大会员","label_theme":"annual_vip"}`,
		"bare string (the older shape)": `"年度大会员"`,
		"empty object":                  `{}`,
		"null":                          `null`,
		"a number (unknown)":            `123`,
		"an array (unknown)":            `["a"]`,
	}
	for name, label := range shapes {
		body := []byte(`{"code":0,"message":"0","data":{"isLogin":true,"mid":42,"uname":"某人",` +
			`"vipStatus":1,"vipType":2,"vipDueDate":` + strconv.FormatInt(time.Now().Add(100*24*time.Hour).UnixMilli(), 10) + `,` +
			`"vip_label":` + label + `,"level_info":{"current_level":6}}}`)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
		}))
		oldAPI := biliAPI
		biliAPI = srv.URL
		d := &BilibiliDriver{cookie: "SESSDATA=x%2C1%2Cy", client: srv.Client()}
		acc, err := d.Account(context.Background())
		h := d.Health(context.Background(), false)
		srv.Close()
		biliAPI = oldAPI
		resetBiliCache()

		if err != nil {
			t.Errorf("%s: the account failed to parse: %v", name, err)
			continue
		}
		if acc == nil || !acc.LoggedIn || acc.Uname != "某人" {
			t.Errorf("%s: account = %+v, want a logged-in answer", name, acc)
			continue
		}
		// The membership must be read from vipStatus regardless of the label.
		if !acc.VIPActive() {
			t.Errorf("%s: a vipStatus 1 account was not seen as a member", name)
		}
		if !strings.Contains(h.Message, "大会员有效") {
			t.Errorf("%s: health message %q does not report the live membership", name, h.Message)
		}
		// The known shapes still yield the label; the unknown ones fall back to
		// the generic name rather than an empty badge.
		if name == "object (the live shape)" || name == "bare string (the older shape)" {
			if acc.VIPLabel != "年度大会员" {
				t.Errorf("%s: label = %q, want 年度大会员", name, acc.VIPLabel)
			}
		}
	}
}

// A response this build cannot read is not a network failure: the fix is an
// upgrade, not a proxy. Reporting it as "连不上 bilibili" sent the user to check a
// proxy that was working.
func TestUnreadableAccountAnswerIsNotBlamedOnTheNetwork(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "isLogin" as an object: a shape change this build cannot read.
		_, _ = w.Write([]byte(`{"code":0,"message":"0","data":{"isLogin":{"x":1},"mid":42,"uname":"某人"}}`))
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=x%2C1%2Cy", client: srv.Client()}
	_, err := d.Account(context.Background())
	if err == nil {
		t.Fatal("an unreadable answer was accepted")
	}
	var dec *biliDecodeError
	if !errors.As(err, &dec) {
		t.Fatalf("a decode failure was not classified as one: %v", err)
	}

	h := d.Health(context.Background(), false)
	if strings.Contains(h.Message, "连不上") {
		t.Errorf("a parse failure was reported as unreachable: %q", h.Message)
	}
	if !strings.Contains(h.Message, "格式变了") {
		t.Errorf("message %q does not say the answer's shape changed", h.Message)
	}
	if strings.Contains(h.Hint, "代理") {
		t.Errorf("hint %q blames the proxy for a parsing problem", h.Hint)
	}
}

// A -101 straight from playurl needs no nav call to confirm, and the message must
// not stack "账号未登录" twice around the reason.
func TestRefusalFromMinusOneDoesNotRepeatItself(t *testing.T) {
	resetBiliCache()
	defer resetBiliCache()

	var navCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			navCalls++
			_, _ = w.Write(accountFixture(false, "", 0, 0))
		case "/pgc/player/web/playurl":
			_, _ = w.Write([]byte(`{"code":-101,"message":"账号未登录"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := biliAPI
	biliAPI = srv.URL
	defer func() { biliAPI = oldAPI }()

	d := &BilibiliDriver{cookie: "SESSDATA=dead", codec: "avc", client: srv.Client()}
	_, err := d.playInfo(context.Background(), biliEpisodeRef{Cid: 1, Aid: 2}, 3)
	if !errors.Is(err, errCookieDead) {
		t.Fatalf("-101 was not classified as a dead cookie: %v", err)
	}
	if n := strings.Count(err.Error(), "账号未登录"); n > 1 {
		t.Errorf("message repeats 账号未登录 %d times: %q", n, err.Error())
	}
	if navCalls != 0 {
		t.Errorf("-101 cost %d nav calls; the API already said nobody is logged in", navCalls)
	}
}

// min is the small helper the QR test uses to shorten a string for a message.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = url.Values{}
