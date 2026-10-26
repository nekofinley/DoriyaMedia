package source

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func sourceRedirectRequest(t *testing.T, target string, header map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	return req
}

func assertHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	if got := header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func assertHeaderAbsent(t *testing.T, header http.Header, name string) {
	t.Helper()
	if got := header.Get(name); got != "" {
		t.Fatalf("%s leaked across origin: %q", name, got)
	}
}

func waitForSourceRedirectHeader(t *testing.T, got <-chan http.Header) http.Header {
	t.Helper()
	select {
	case header := <-got:
		return header
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for redirected request")
		return nil
	}
}

func TestSourceHTTPClientKeepsHeadersForSameOriginRedirect(t *testing.T) {
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	req := sourceRedirectRequest(t, srv.URL+"/start", map[string]string{
		"Authorization":  "Bearer source-secret",
		"Cookie":         "session=source-secret",
		"Origin":         "https://source.example",
		"Referer":        "https://source.example/library",
		"X-Source-Token": "source-secret",
		"Range":          "bytes=0-99",
		"User-Agent":     "DoriyaMedia-Test",
	})
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET same-origin redirect: %v", err)
	}
	resp.Body.Close()

	header := waitForSourceRedirectHeader(t, got)
	for name, want := range map[string]string{
		"Authorization":  "Bearer source-secret",
		"Cookie":         "session=source-secret",
		"Origin":         "https://source.example",
		"Referer":        "https://source.example/library",
		"X-Source-Token": "source-secret",
		"Range":          "bytes=0-99",
		"User-Agent":     "DoriyaMedia-Test",
	} {
		assertHeader(t, header, name, want)
	}
}

func TestSourceHTTPClientStripsHeadersWhenPortChanges(t *testing.T) {
	got := make(chan http.Header, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			got <- r.Header.Clone()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hop", http.StatusFound)
	}))
	defer origin.Close()

	req := sourceRedirectRequest(t, origin.URL+"/start", map[string]string{
		"Authorization":       "Bearer source-secret",
		"Cookie":              "session=source-secret",
		"Proxy-Authorization": "Basic source-secret",
		"Origin":              "https://source.example",
		"Referer":             "https://source.example/library",
		"X-Api-Key":           "source-secret",
		"X-Source-Token":      "source-secret",
		"Range":               "bytes=0-99",
		"Accept":              "video/*",
		"User-Agent":          "DoriyaMedia-Test",
	})
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET port-changing redirect: %v", err)
	}
	resp.Body.Close()

	header := waitForSourceRedirectHeader(t, got)
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"Proxy-Authorization",
		"Origin",
		"Referer",
		"X-Api-Key",
		"X-Source-Token",
	} {
		assertHeaderAbsent(t, header, name)
	}
	for name, want := range map[string]string{
		"Range":      "bytes=0-99",
		"Accept":     "video/*",
		"User-Agent": "DoriyaMedia-Test",
	} {
		assertHeader(t, header, name, want)
	}
}

func TestSourceHTTPClientStripsHeadersWhenHostChanges(t *testing.T) {
	got := make(chan http.Header, 1)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			u, err := url.Parse(srv.URL)
			if err != nil {
				http.Error(w, "bad test server URL", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "http://other.example.test:"+u.Port()+"/final", http.StatusFound)
		case "/final":
			got <- r.Header.Clone()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	defer transport.CloseIdleConnections()
	client := newSourceHTTPClient(&http.Client{Transport: transport})

	req := sourceRedirectRequest(t, srv.URL+"/start", map[string]string{
		"Authorization":  "Bearer source-secret",
		"Cookie":         "session=source-secret",
		"Origin":         "https://source.example",
		"Referer":        "https://source.example/library",
		"X-Source-Token": "source-secret",
	})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET host-changing redirect: %v", err)
	}
	resp.Body.Close()

	header := waitForSourceRedirectHeader(t, got)
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"Origin",
		"Referer",
		"X-Source-Token",
	} {
		assertHeaderAbsent(t, header, name)
	}
}

func TestSourceHTTPClientRejectsHTTPSDowngrade(t *testing.T) {
	targetHit := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	client := newSourceHTTPClient(origin.Client())
	resp, err := client.Get(origin.URL + "/start")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errSourceHTTPSDowngrade) {
		t.Fatalf("HTTPS downgrade error = %v, want errSourceHTTPSDowngrade", err)
	}
	select {
	case <-targetHit:
		t.Fatal("HTTP target was reached after HTTPS downgrade")
	default:
	}
}

func TestNewBilibiliWrapsProxyClientRedirectPolicy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()

	driver, err := NewBilibili("", "avc", proxy.URL, false)
	if err != nil {
		t.Fatalf("NewBilibili: %v", err)
	}
	if driver.client == nil {
		t.Fatal("proxy client is nil")
	}
	if driver.client.CheckRedirect == nil {
		t.Fatal("netproxy client does not carry the source redirect policy")
	}
}

func TestStripSourceCrossOriginHeadersUsesAllowlist(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://other.example/final", nil)
	for name, value := range map[string]string{
		"Authorization":       "secret",
		"Cookie":              "secret",
		"Proxy-Authorization": "secret",
		"Origin":              "https://source.example",
		"Referer":             "https://source.example/",
		"X-Any-Caller-Header": "secret",
		"Range":               "bytes=1-2",
		"If-Range":            `"etag"`,
		"User-Agent":          "DoriyaMedia-Test",
	} {
		req.Header.Set(name, value)
	}

	stripSourceCrossOriginHeaders(req.Header)

	for _, name := range []string{
		"Authorization",
		"Cookie",
		"Proxy-Authorization",
		"Origin",
		"Referer",
		"X-Any-Caller-Header",
	} {
		assertHeaderAbsent(t, req.Header, name)
	}
	for name, want := range map[string]string{
		"Range":      "bytes=1-2",
		"If-Range":   `"etag"`,
		"User-Agent": "DoriyaMedia-Test",
	} {
		assertHeader(t, req.Header, name, want)
	}
}

func TestSourceHTTPClientPreservesExistingRedirectHook(t *testing.T) {
	called := false
	got := make(chan http.Header, 1)
	original := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			called = true
			req.Header.Set("X-Added-By-Hook", "source-secret")
			return nil
		},
	}
	client := newSourceHTTPClient(original)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "http://other.example.test/final", http.StatusFound)
		case "/final":
			got <- r.Header.Clone()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	defer transport.CloseIdleConnections()
	client.Transport = transport

	resp, err := client.Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("GET redirect: %v", err)
	}
	resp.Body.Close()
	if !called {
		t.Fatal("wrapped client did not call the existing redirect hook")
	}
	if original.CheckRedirect == nil {
		t.Fatal("wrapping mutated the original client")
	}
	assertHeaderAbsent(t, waitForSourceRedirectHeader(t, got), "X-Added-By-Hook")
}

func TestSourceHTTPClientCustomRedirectHookCannotRemoveHopLimit(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	client := newSourceHTTPClient(&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	})
	resp, err := client.Get(srv.URL + "/start")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("redirect loop was accepted")
	}
	if hits > 10 {
		t.Fatalf("followed %d redirects, want at most 10", hits)
	}
}
