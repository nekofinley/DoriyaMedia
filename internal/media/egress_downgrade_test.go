package media

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func egressDowngradeProxyClient(t *testing.T, controller *egressController) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(controller.ProxyURL())
	if err != nil {
		t.Fatalf("parse controlled proxy URL: %v", err)
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		// The fixture certificates are self-signed; the controlled proxy still
		// establishes a real TLS tunnel to the test origin.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestEgressHTTPSInitialRejectsRedirectToHTTP(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = io.WriteString(w, "plaintext-secret")
	}))
	t.Cleanup(plain.Close)
	plainURL, _ := url.Parse(plain.URL)
	plainTarget := "http://plain.example:" + plainURL.Port() + "/segment"

	var secureHits atomic.Int32
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHits.Add(1)
		http.Redirect(w, r, plainTarget, http.StatusFound)
	}))
	t.Cleanup(secure.Close)
	secureURL, _ := url.Parse(secure.URL)
	target := "https://secure.example:" + secureURL.Port() + "/start"

	lookup, dial, recorder := egressTestNetwork(t, []egressTestRoute{
		{host: "secure.example", ip: "203.0.113.120", target: secure.URL},
		{host: "plain.example", ip: "203.0.113.121", target: plain.URL},
	})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	t.Cleanup(controller.Close)

	resp, err := egressDowngradeProxyClient(t, controller).Get(target)
	if err != nil {
		t.Fatalf("HTTPS request through controlled proxy: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("downgrade response = %d, want 403", resp.StatusCode)
	}
	if secureHits.Load() != 1 {
		t.Fatalf("HTTPS origin hits = %d, want 1", secureHits.Load())
	}
	if plainHits.Load() != 0 {
		t.Fatalf("plaintext redirect target was reached %d time(s)", plainHits.Load())
	}
	plainDial := "203.0.113.121:" + plainURL.Port()
	for _, address := range recorder.snapshot() {
		if address == plainDial {
			t.Fatalf("proxy dialed plaintext redirect target %q", address)
		}
	}
}

func TestEgressRealFFmpegHTTPSManifestRejectsHTTPChild(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}

	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = io.WriteString(w, "plaintext-segment")
	}))
	t.Cleanup(plain.Close)
	plainURL, _ := url.Parse(plain.URL)
	child := "http://plain.example:" + plainURL.Port() + "/secret.ts"

	var manifestHits atomic.Int32
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprintf(w,
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.5,\n%s\n#EXT-X-ENDLIST\n",
			child)
	}))
	t.Cleanup(secure.Close)
	secureURL, _ := url.Parse(secure.URL)
	target := "https://secure.example:" + secureURL.Port() + "/index.m3u8"

	lookup, dial, recorder := egressTestNetwork(t, []egressTestRoute{
		{host: "secure.example", ip: "203.0.113.122", target: secure.URL},
		{host: "plain.example", ip: "203.0.113.123", target: plain.URL},
	})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	t.Cleanup(controller.Close)

	cmd := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", egressRemoteProtocolWhitelist,
		"-http_proxy", controller.ProxyURL(),
		"-tls_verify", "0",
		"-i", target,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
	cmd.Env = secureMediaEnv("")
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("ffmpeg accepted an HTTP child of an HTTPS manifest: %s", output)
	}
	if manifestHits.Load() == 0 {
		t.Fatal("HTTPS manifest was never reached")
	}
	if plainHits.Load() != 0 {
		t.Fatalf("HTTP HLS child was reached %d time(s): %s", plainHits.Load(), output)
	}
	plainDial := "203.0.113.123:" + plainURL.Port()
	for _, address := range recorder.snapshot() {
		if address == plainDial {
			t.Fatalf("proxy dialed plaintext HLS child %q", address)
		}
	}
}

func TestEgressHTTPInitialStillAllowsHTTPAndHTTPS(t *testing.T) {
	var tlsHits atomic.Int32
	tlsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tlsHits.Add(1)
		_, _ = io.WriteString(w, "upgraded-media")
	}))
	t.Cleanup(tlsOrigin.Close)
	tlsURL, _ := url.Parse(tlsOrigin.URL)
	upgradeTarget := "https://upgraded.example:" + tlsURL.Port() + "/media"

	var httpHits atomic.Int32
	httpOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits.Add(1)
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/media", http.StatusFound)
		case "/upgrade":
			http.Redirect(w, r, upgradeTarget, http.StatusFound)
		case "/media":
			_, _ = io.WriteString(w, "plain-media")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(httpOrigin.Close)
	httpURL, _ := url.Parse(httpOrigin.URL)
	target := "http://source.example:" + httpURL.Port() + "/start"

	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{
		{host: "source.example", ip: "203.0.113.124", target: httpOrigin.URL},
		{host: "upgraded.example", ip: "203.0.113.125", target: tlsOrigin.URL},
	})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	t.Cleanup(controller.Close)
	client := egressDowngradeProxyClient(t, controller)

	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("HTTP-to-HTTP request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "plain-media" {
		t.Fatalf("HTTP-to-HTTP response = %d %q", resp.StatusCode, body)
	}

	upgradeURL := "http://source.example:" + httpURL.Port() + "/upgrade"
	resp, err = client.Get(upgradeURL)
	if err != nil {
		t.Fatalf("HTTP-to-HTTPS request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "upgraded-media" {
		t.Fatalf("HTTP-to-HTTPS response = %d %q", resp.StatusCode, body)
	}
	if httpHits.Load() < 3 || tlsHits.Load() != 1 {
		t.Fatalf("normal HTTP source hits: HTTP=%d HTTPS=%d", httpHits.Load(), tlsHits.Load())
	}
}

func TestEgressHTTPCrossOriginStillStripsSourceHeaders(t *testing.T) {
	childHeaders := make(chan http.Header, 1)
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		childHeaders <- r.Header.Clone()
		_, _ = io.WriteString(w, "child")
	}))
	t.Cleanup(child.Close)
	childURL, _ := url.Parse(child.URL)
	childTarget := "http://child.example:" + childURL.Port() + "/segment"

	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, childTarget, http.StatusFound)
	}))
	t.Cleanup(initial.Close)
	initialURL, _ := url.Parse(initial.URL)
	target := "http://initial.example:" + initialURL.Port() + "/start"

	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{
		{host: "initial.example", ip: "203.0.113.126", target: initial.URL},
		{host: "child.example", ip: "203.0.113.127", target: child.URL},
	})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	t.Cleanup(controller.Close)

	client := egressDowngradeProxyClient(t, controller)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new initial request: %v", err)
	}
	setSourceHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("initial request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("initial status = %d, want 302", resp.StatusCode)
	}

	req, err = http.NewRequest(http.MethodGet, childTarget, nil)
	if err != nil {
		t.Fatalf("new child request: %v", err)
	}
	setSourceHeaders(req.Header)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("child request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("child status = %d, want 200", resp.StatusCode)
	}

	select {
	case headers := <-childHeaders:
		for _, name := range []string{
			"Authorization", "Cookie", "Referer", "X-Source-Token",
		} {
			if value := headers.Get(name); value != "" {
				t.Errorf("cross-origin child received %s=%q", name, value)
			}
		}
		if got := headers.Get("Range"); got != "bytes=0-31" {
			t.Errorf("cross-origin Range = %q, want bytes=0-31", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child request")
	}
}

func setSourceHeaders(header http.Header) {
	header.Set("Authorization", "Bearer source-secret")
	header.Set("Cookie", "session=source-secret")
	header.Set("Referer", "https://source.example/start?signature=secret")
	header.Set("X-Source-Token", "source-secret")
	header.Set("Range", "bytes=0-31")
}

func TestEgressHTTPRedirectLoopRemainsBounded(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	originURL, _ := url.Parse(origin.URL)
	target := "http://loop.example:" + originURL.Port() + "/loop"

	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "loop.example", ip: "203.0.113.128", target: origin.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	t.Cleanup(controller.Close)

	resp, err := egressDowngradeProxyClient(t, controller).Get(target)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("redirect loop unexpectedly succeeded")
	}
	if hits.Load() > 11 {
		t.Fatalf("redirect loop made %d requests, want at most 11", hits.Load())
	}
}

func TestEgressSourceProxyRemainsLoopbackOnly(t *testing.T) {
	_, err := newEgressController(
		[]egressTarget{{
			URL:   "https://secure.example/media",
			Proxy: "http://proxy.example:8080",
		}},
		egressOptions{})
	if err == nil || !strings.Contains(err.Error(), "loopback bridge") {
		t.Fatalf("non-loopback source proxy error = %v", err)
	}
}

func TestEgressMixedInitialSchemesFailClosed(t *testing.T) {
	_, err := newEgressController(
		[]egressTarget{
			{URL: "http://plain.example/media"},
			{URL: "https://secure.example/media"},
		},
		egressOptions{})
	if err == nil || !strings.Contains(err.Error(), "mixed initial URL schemes") {
		t.Fatalf("mixed-scheme controller error = %v", err)
	}
}
