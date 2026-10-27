package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

func TestMain(m *testing.M) {
	if os.Getenv("DORIYA_EGRESS_HELPER") != "" {
		os.Exit(runEgressHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runEgressHelper(args []string) int {
	proxyRaw, target := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-http_proxy":
			if i+1 < len(args) {
				proxyRaw = args[i+1]
				i++
			}
		default:
			target = args[i]
		}
	}
	if proxyRaw == "" || target == "" {
		fmt.Fprintln(os.Stderr, "egress helper: missing proxy or target")
		return 2
	}
	proxyURL, err := url.Parse(proxyRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress helper: invalid proxy")
		return 2
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress helper: invalid target")
		return 2
	}
	if value := os.Getenv("DORIYA_EGRESS_HELPER_RANGE"); value != "" {
		req.Header.Set("Range", value)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress helper: request failed")
		return 3
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		fmt.Fprintln(os.Stderr, "egress helper: response read failed")
		return 3
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "egress helper: status %d\n", resp.StatusCode)
		return 3
	}
	if os.Getenv("DORIYA_EGRESS_HELPER_MODE") == "ffprobe" {
		_, _ = io.WriteString(os.Stdout,
			`{"format":{"format_name":"matroska"},"streams":[{"codec_type":"video","codec_name":"h264"}]}`)
		return 0
	}
	_, _ = os.Stdout.Write(body)
	return 0
}

type egressTestRoute struct {
	host   string
	ip     string
	target string
}

type egressDialRecorder struct {
	mu        sync.Mutex
	addresses []string
}

func (r *egressDialRecorder) record(address string) {
	r.mu.Lock()
	r.addresses = append(r.addresses, address)
	r.mu.Unlock()
}

func (r *egressDialRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addresses...)
}

func egressTestNetwork(
	t *testing.T,
	routes []egressTestRoute,
) (egressIPLookup, egressDialFunc, *egressDialRecorder) {
	t.Helper()
	lookupHosts := make(map[string][]string, len(routes))
	dialRoutes := make(map[string]string, len(routes))
	for _, route := range routes {
		serverURL, err := url.Parse(route.target)
		if err != nil || serverURL.Hostname() == "" || serverURL.Port() == "" {
			t.Fatalf("invalid test route %q: %v", route.target, err)
		}
		lookupHosts[route.host] = []string{route.ip}
		dialRoutes[net.JoinHostPort(route.ip, serverURL.Port())] =
			net.JoinHostPort(serverURL.Hostname(), serverURL.Port())
	}
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		values, ok := lookupHosts[egressCanonicalHost(host)]
		if !ok {
			return nil, fmt.Errorf("unexpected test lookup for %q", host)
		}
		out := make([]net.IPAddr, 0, len(values))
		for _, raw := range values {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("invalid test IP %q", raw)
			}
			out = append(out, net.IPAddr{IP: ip})
		}
		return out, nil
	}
	recorder := &egressDialRecorder{}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		recorder.record(address)
		target, ok := dialRoutes[address]
		if !ok {
			return nil, fmt.Errorf("unexpected test dial to %s", address)
		}
		return dialer.DialContext(ctx, network, target)
	}
	return lookup, dial, recorder
}

func egressTestProxyClient(t *testing.T, controller *egressController) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(controller.ProxyURL())
	if err != nil {
		t.Fatalf("parse controlled proxy URL: %v", err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
}

func TestEgressProxyRejectsRedirectsToInternalServices(t *testing.T) {
	tests := []struct {
		name     string
		location func(internal *httptest.Server) string
		route    func(internal *httptest.Server) []egressTestRoute
	}{
		{
			name: "loopback",
			location: func(internal *httptest.Server) string {
				return internal.URL + "/loopback"
			},
		},
		{
			name: "RFC1918",
			location: func(internal *httptest.Server) string {
				parsed, _ := url.Parse(internal.URL)
				return "http://private.example:" + parsed.Port() + "/lan"
			},
			route: func(internal *httptest.Server) []egressTestRoute {
				return []egressTestRoute{{
					host: "private.example", ip: "10.10.0.8", target: internal.URL,
				}}
			},
		},
		{
			name: "metadata",
			location: func(internal *httptest.Server) string {
				parsed, _ := url.Parse(internal.URL)
				return "http://169.254.169.254:" + parsed.Port() + "/latest/meta-data"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var internalHits atomic.Int32
			internal := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					internalHits.Add(1)
					_, _ = io.WriteString(w, "internal-secret")
				}))
			t.Cleanup(internal.Close)
			var publicHits atomic.Int32
			public := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					publicHits.Add(1)
					http.Redirect(w, r, tc.location(internal), http.StatusFound)
				}))
			t.Cleanup(public.Close)

			publicURL, _ := url.Parse(public.URL)
			target := "http://public.example:" + publicURL.Port() + "/start"
			routes := []egressTestRoute{{
				host: "public.example", ip: "203.0.113.10", target: public.URL,
			}}
			if tc.route != nil {
				routes = append(routes, tc.route(internal)...)
			}
			lookup, dial, _ := egressTestNetwork(t, routes)
			controller, err := newEgressController(
				[]egressTarget{{URL: target}},
				egressOptions{lookup: lookup, dial: dial})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()

			resp, err := egressTestProxyClient(t, controller).Get(target)
			if err != nil {
				t.Fatalf("proxy request failed before policy response: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("redirect status = %d, want 403", resp.StatusCode)
			}
			if internalHits.Load() != 0 {
				t.Fatalf("internal service was reached %d time(s)", internalHits.Load())
			}
			if publicHits.Load() != 1 {
				t.Fatalf("public redirect service hits = %d, want 1", publicHits.Load())
			}
		})
	}
}

func TestEgressProxyRejectsDNSRebinding(t *testing.T) {
	var publicHits atomic.Int32
	public := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			publicHits.Add(1)
			_, _ = io.WriteString(w, "public")
		}))
	t.Cleanup(public.Close)
	publicURL, _ := url.Parse(public.URL)
	target := "http://rebind.example:" + publicURL.Port() + "/video"

	var lookups atomic.Int32
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "rebind.example" {
			return nil, fmt.Errorf("unexpected lookup %q", host)
		}
		raw := "203.0.113.20"
		if lookups.Add(1) > 1 {
			raw = "10.20.0.9"
		}
		return []net.IPAddr{{IP: net.ParseIP(raw)}}, nil
	}
	var dials atomic.Int32
	dialer := &net.Dialer{Timeout: time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return dialer.DialContext(ctx, network, publicURL.Host)
	}
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	resp, err := egressTestProxyClient(t, controller).Get(target)
	if err != nil {
		t.Fatalf("proxy request failed before policy response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("rebinding status = %d, want 502", resp.StatusCode)
	}
	if dials.Load() != 0 {
		t.Fatalf("rebound private address was dialed %d time(s)", dials.Load())
	}
	if publicHits.Load() != 0 {
		t.Fatalf("public origin was reached after rebinding: %d", publicHits.Load())
	}
	if lookups.Load() < 2 {
		t.Fatalf("lookups = %d, want authorize and dial to resolve independently", lookups.Load())
	}
}

func TestEgressProxyAllowsAdministratorTrustedLAN(t *testing.T) {
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Unix(1_700_000_000, 0),
			strings.NewReader(body))
	}))
	t.Cleanup(server.Close)
	serverURL, _ := url.Parse(server.URL)
	port := serverURL.Port()
	lookup, dial, recorder := egressTestNetwork(t, []egressTestRoute{{
		host: "nas.example", ip: "10.30.0.8", target: server.URL,
	}})

	for _, tc := range []struct {
		name   string
		source models.Source
		target string
	}{
		{
			name: "WebDAV",
			source: models.Source{
				Type: "webdav",
				Config: fmt.Sprintf(
					`{"webdav_url":%q}`, "http://nas.example:"+port+"/dav"),
			},
			target: "http://nas.example:" + port + "/dav/movie.mp4",
		},
		{
			name: "OpenList",
			source: models.Source{
				Type: "openlist",
				Config: fmt.Sprintf(
					`{"base_url":%q}`, "nas.example:"+port),
			},
			target: "http://nas.example:" + port + "/p/movie.mp4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust := newEgressTrust()
			trust.addSource(&tc.source)
			controller, err := newEgressController(
				[]egressTarget{{URL: tc.target}},
				egressOptions{trust: trust, lookup: lookup, dial: dial})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()

			req, _ := http.NewRequest(http.MethodGet, tc.target, nil)
			req.Header.Set("Range", "bytes=2-5")
			resp, err := egressTestProxyClient(t, controller).Do(req)
			if err != nil {
				t.Fatalf("LAN proxy request: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent || string(raw) != "2345" {
				t.Fatalf("LAN response = %d %q, want 206 range bytes", resp.StatusCode, raw)
			}
			if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/36" {
				t.Errorf("Content-Range = %q", got)
			}
		})
	}

	trust := newEgressTrust()
	trust.addURL("http://nas.example:" + port + "/dav")
	for _, target := range []string{
		"http://nas.example:1/dav/movie.mp4",
		"https://nas.example:" + port + "/dav/movie.mp4",
	} {
		controller, err := newEgressController(
			[]egressTarget{{URL: target}},
			egressOptions{trust: trust, lookup: lookup, dial: dial})
		if err != nil {
			t.Fatalf("new mismatched-origin proxy: %v", err)
		}
		dialsBefore := len(recorder.snapshot())
		resp, err := egressTestProxyClient(t, controller).Get(target)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("mismatched origin %q status = %d, want 403", target, resp.StatusCode)
			}
		}
		controller.Close()
		if got := len(recorder.snapshot()); got != dialsBefore {
			t.Fatalf("mismatched origin %q dialed %d unexpected address(es)",
				target, got-dialsBefore)
		}
	}
	if len(recorder.snapshot()) != 2 {
		t.Fatalf("trusted requests dialed %d time(s), want 2 exact-origin dials",
			len(recorder.snapshot()))
	}
}

func TestEgressProxyAllowsSafePublicRange(t *testing.T) {
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Unix(1_700_000_000, 0),
			strings.NewReader(body))
	}))
	t.Cleanup(public.Close)
	publicURL, _ := url.Parse(public.URL)
	target := "http://public.example:" + publicURL.Port() + "/clip.mp4"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.30", target: public.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := egressTestProxyClient(t, controller).Do(req)
	if err != nil {
		t.Fatalf("public proxy request: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(raw) != "2345" {
		t.Fatalf("public response = %d %q, want 206 range bytes", resp.StatusCode, raw)
	}
}

func TestEgressProxyRoutesCrossOriginThroughSourceBridge(t *testing.T) {
	bridgeURL := &url.URL{Scheme: "http", Host: "127.0.0.1:43210"}
	clientConn, bridgeConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = bridgeConn.Close()
	})
	requestLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(bridgeConn).ReadString('\n')
		requestLine <- strings.TrimSpace(line)
		_, _ = io.WriteString(bridgeConn,
			"HTTP/1.1 200 Connection Established\r\n\r\n")
	}()

	var dialAddress string
	dial := func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "cdn.example" {
			return nil, fmt.Errorf("unexpected lookup %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.90")}}, nil
	}
	controller, err := newEgressController(
		[]egressTarget{{
			URL:   "https://initial.example:8443/start",
			Proxy: bridgeURL.String(),
		}},
		egressOptions{
			lookup: lookup,
			dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				dialAddress = address
				return dial(ctx, network, address)
			},
		})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	target, _ := url.Parse("https://cdn.example:9443/segment.m4s")
	conn, err := controller.policy.connect(context.Background(), target)
	if err != nil {
		t.Fatalf("cross-origin source bridge: %v", err)
	}
	_ = conn.Close()
	if dialAddress != bridgeURL.Host {
		t.Fatalf("dialed %q, want source bridge %q", dialAddress, bridgeURL.Host)
	}
	select {
	case got := <-requestLine:
		if got != "CONNECT 203.0.113.90:9443 HTTP/1.1" {
			t.Fatalf("source bridge request = %q, want validated IP", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for source bridge request")
	}
}

func TestEgressSourceBridgeConnectHonorsContext(t *testing.T) {
	clientConn, proxyConn := net.Pipe()
	stopProxy := make(chan struct{})
	t.Cleanup(func() {
		close(stopProxy)
		_ = clientConn.Close()
		_ = proxyConn.Close()
	})
	requestRead := make(chan error, 1)
	go func() {
		_, err := http.ReadRequest(bufio.NewReader(proxyConn))
		requestRead <- err
		<-stopProxy
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := dialHTTPProxyBridge(
			ctx,
			&url.URL{Scheme: "http", Host: "proxy.test:8080"},
			"203.0.113.74:443",
			func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			})
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("silent CONNECT error = %v, want context deadline", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("silent CONNECT took %s, want less than 1s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("silent CONNECT did not return within 1s")
	}
	select {
	case err := <-requestRead:
		if err != nil {
			t.Fatalf("read CONNECT request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source bridge did not receive the CONNECT request")
	}
}

func TestEgressSourceBridgeConnectClearsHandshakeDeadline(t *testing.T) {
	clientConn, proxyConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = proxyConn.Close()
	})
	proxyResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(proxyConn)
		if _, err := http.ReadRequest(reader); err != nil {
			proxyResult <- err
			return
		}
		if _, err := io.WriteString(proxyConn,
			"HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			proxyResult <- err
			return
		}
		var payload [1]byte
		_, err := io.ReadFull(proxyConn, payload[:])
		proxyResult <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := dialHTTPProxyBridge(
		ctx,
		&url.URL{Scheme: "http", Host: "proxy.test:8080"},
		"203.0.113.75:443",
		func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		})
	if err != nil {
		t.Fatalf("establish CONNECT tunnel: %v", err)
	}
	defer conn.Close()
	time.Sleep(150 * time.Millisecond)

	writeResult := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("x"))
		writeResult <- err
	}()
	select {
	case err := <-writeResult:
		if err != nil {
			t.Fatalf("write after handshake context expiry: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write after handshake context expiry did not return within 1s")
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("proxy tunnel read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive tunneled payload")
	}
}

func TestEgressTrustRequiresExactSchemeHostAndPort(t *testing.T) {
	trust := newEgressTrust()
	trust.addURL("https://nas.example:8443/dav")
	allowed, _ := url.Parse("https://nas.example:8443/movie.mp4")
	if !trust.allows(allowed) {
		t.Fatal("exact administrator origin was not trusted")
	}
	for _, raw := range []string{
		"http://nas.example:8443/movie.mp4",
		"https://nas.example:443/movie.mp4",
		"https://nas.example:8443.0/movie.mp4",
		"https://other.example:8443/movie.mp4",
	} {
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if trust.allows(parsed) {
			t.Errorf("non-exact origin %q was trusted", raw)
		}
	}
}

func TestEgressConfiguredOriginRequiresDriverTargetLease(t *testing.T) {
	const base = "http://nas.example:8080/dav"
	target := "http://nas.example:8080/dav/movie.mp4"
	targetURL, _ := url.Parse(target)
	trust := newEgressTrust()
	trust.addURL(base)

	if trust.targetView(target).allows(targetURL) {
		t.Fatal("configured origin trusted an arbitrary target without a lease")
	}
	if trust.leaseDriverTarget(&source.URLDriver{}, target) {
		t.Fatal("URLDriver obtained a private target lease")
	}
	if trust.targetView(target).allows(targetURL) {
		t.Fatal("failed lease attempt widened the trust boundary")
	}

	drv := source.NewWebDAV(base, "user", "pass")
	if !trust.leaseDriverTarget(drv, target) {
		t.Fatal("WebDAV target did not obtain a lease")
	}
	if !trust.targetView(target).allows(targetURL) {
		t.Fatal("leased WebDAV target was not trusted")
	}
	otherTarget := "http://nas.example:8080/dav/other.mp4"
	otherURL, _ := url.Parse(otherTarget)
	if trust.targetView(otherTarget).allows(otherURL) {
		t.Fatal("exact-target lease authorized a different path")
	}
}

func TestEgressProxyNeverTrustsMetadataAddress(t *testing.T) {
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://100.100.100.200/latest/meta-data",
		"http://168.63.129.16/machine",
		"http://[fd00:ec2::254]/latest/meta-data",
		"http://[64:ff9b::a9fe:a9fe]/latest/meta-data",
		"http://[2002:7f00:1::]/loopback",
	} {
		t.Run(target, func(t *testing.T) {
			trust := newEgressTrust()
			trust.addURL(target)
			controller, err := newEgressController(
				[]egressTarget{{URL: target}},
				egressOptions{trust: trust})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()
			resp, err := egressTestProxyClient(t, controller).Get(target)
			if err != nil {
				t.Fatalf("metadata request: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("metadata status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestEgressFFmpegChildProcessCannotReachInternalRedirects(t *testing.T) {
	tests := []struct {
		name     string
		location func(*httptest.Server) string
		route    func(*httptest.Server) []egressTestRoute
	}{
		{
			name: "loopback",
			location: func(internal *httptest.Server) string {
				return internal.URL + "/loopback"
			},
		},
		{
			name: "RFC1918",
			location: func(internal *httptest.Server) string {
				parsed, _ := url.Parse(internal.URL)
				return "http://internal.example:" + parsed.Port() + "/lan"
			},
			route: func(internal *httptest.Server) []egressTestRoute {
				return []egressTestRoute{{
					host: "internal.example", ip: "10.40.0.9", target: internal.URL,
				}}
			},
		},
		{
			name: "metadata",
			location: func(internal *httptest.Server) string {
				parsed, _ := url.Parse(internal.URL)
				return "http://169.254.169.254:" + parsed.Port() + "/latest/meta-data"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var internalHits atomic.Int32
			internal := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					internalHits.Add(1)
					_, _ = io.WriteString(w, "internal-secret")
				}))
			t.Cleanup(internal.Close)
			public := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, tc.location(internal), http.StatusFound)
				}))
			t.Cleanup(public.Close)
			publicURL, _ := url.Parse(public.URL)
			target := "http://public.example:" + publicURL.Port() + "/start"
			routes := []egressTestRoute{{
				host: "public.example", ip: "203.0.113.40", target: public.URL,
			}}
			if tc.route != nil {
				routes = append(routes, tc.route(internal)...)
			}
			lookup, dial, _ := egressTestNetwork(t, routes)
			controller, err := newEgressController(
				[]egressTarget{{URL: target}},
				egressOptions{lookup: lookup, dial: dial})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := runEgressChild(ctx, controller.ProxyURL(), target, "ffmpeg", "")
			if err == nil {
				t.Fatalf("child process unexpectedly succeeded: %s", output)
			}
			if internalHits.Load() != 0 {
				t.Fatalf("child process reached the internal service %d time(s)",
					internalHits.Load())
			}
			if strings.Contains(string(output), "internal-secret") {
				t.Fatalf("child process read internal content: %s", output)
			}
		})
	}
}

func TestEgressFFprobeChildProcessReachesAllowedLAN(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "lan-media")
	}))
	t.Cleanup(internal.Close)
	internalURL, _ := url.Parse(internal.URL)
	target := "http://nas.example:" + internalURL.Port() + "/dav/movie.mp4"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "nas.example", ip: "10.50.0.8", target: internal.URL,
	}})
	trust := newEgressTrust()
	trust.addURL("http://nas.example:" + internalURL.Port() + "/dav")
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{trust: trust, lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := runEgressChild(
		ctx, controller.ProxyURL(), target, "ffprobe", "")
	if err != nil {
		t.Fatalf("ffprobe helper failed: %v: %s", err, output)
	}
	var probe Probe
	if err := json.Unmarshal(output, &probe); err != nil || probe.Video() == nil {
		t.Fatalf("ffprobe helper output = %q, %v", output, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("allowed LAN hits = %d, want 1", hits.Load())
	}
}

func TestEgressFFmpegChildProcessPreservesPublicRange(t *testing.T) {
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("helper Range = %q", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Unix(1_700_000_000, 0),
			strings.NewReader(body))
	}))
	t.Cleanup(public.Close)
	publicURL, _ := url.Parse(public.URL)
	target := "http://public.example:" + publicURL.Port() + "/clip.mp4"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.50", target: public.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := runEgressChild(
		ctx, controller.ProxyURL(), target, "ffmpeg", "bytes=2-5")
	if err != nil {
		t.Fatalf("ffmpeg helper failed: %v: %s", err, output)
	}
	if string(output) != "2345" {
		t.Fatalf("ffmpeg helper body = %q, want range bytes", output)
	}
}

func TestEgressRealFFmpegNestedHLSHonorsInputProxy(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	playlist := filepath.Join(dir, "index.m3u8")
	if output, err := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1",
		"-c:v", "mpeg2video", "-f", "hls",
		"-hls_time", "0.5", "-hls_list_size", "0", playlist,
	).CombinedOutput(); err != nil {
		t.Skipf("cannot build HLS fixture: %v: %s", err, output)
	}

	var indexHits, segmentHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			indexHits.Add(1)
		} else {
			segmentHits.Add(1)
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	target := "http://public.example:" + upstreamURL.Port() + "/index.m3u8"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.95", target: upstream.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", egressRemoteProtocolWhitelist,
		"-http_proxy", controller.ProxyURL(),
		"-i", target,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
	// The environment points at a dead proxy, matching the multi-input deny-all
	// fallback. The explicit per-input option must remain authoritative for the
	// input and every nested request.
	cmd.Env = secureMediaEnv("http://127.0.0.1:1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg HLS through controlled proxy: %v: %s", err, output)
	}
	if indexHits.Load() == 0 || segmentHits.Load() == 0 {
		t.Fatalf("HLS requests index=%d segment=%d, both must use the proxy",
			indexHits.Load(), segmentHits.Load())
	}
}

func TestEgressRealFFmpegNestedHTTPSHLSHonorsInputProxy(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	playlist := filepath.Join(dir, "index.m3u8")
	if output, err := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1",
		"-c:v", "mpeg2video", "-f", "hls",
		"-hls_time", "0.5", "-hls_list_size", "0", playlist,
	).CombinedOutput(); err != nil {
		t.Skipf("cannot build HLS fixture: %v: %s", err, output)
	}

	var indexHits, segmentHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			indexHits.Add(1)
		} else {
			segmentHits.Add(1)
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	target := "https://public.example:" + upstreamURL.Port() + "/index.m3u8"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.98", target: upstream.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", egressRemoteProtocolWhitelist,
		"-http_proxy", controller.ProxyURL(),
		"-tls_verify", "0",
		"-i", target,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
	cmd.Env = secureMediaEnv("")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg HTTPS HLS through controlled proxy: %v: %s", err, output)
	}
	if indexHits.Load() == 0 || segmentHits.Load() == 0 {
		t.Fatalf("HTTPS HLS requests index=%d segment=%d, both must use the proxy",
			indexHits.Load(), segmentHits.Load())
	}
}

func TestEgressRealFFmpegNestedHLSRejectsLoopbackSegment(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		_, _ = io.WriteString(w, "internal-secret")
	}))
	t.Cleanup(internal.Close)

	var manifestHits atomic.Int32
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprintf(w,
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.5,\n%s/secret.ts\n#EXT-X-ENDLIST\n",
			internal.URL)
	}))
	t.Cleanup(public.Close)
	publicURL, _ := url.Parse(public.URL)
	target := "http://public.example:" + publicURL.Port() + "/index.m3u8"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.96", target: public.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", egressRemoteProtocolWhitelist,
		"-http_proxy", controller.ProxyURL(),
		"-i", target,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
	cmd.Env = secureMediaEnv("")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("ffmpeg accepted a loopback segment: %s", output)
	}
	if internalHits.Load() != 0 {
		t.Fatalf("nested HLS reached loopback %d time(s)", internalHits.Load())
	}
	if manifestHits.Load() == 0 {
		t.Fatal("nested HLS test never reached the public manifest")
	}
}

func TestEgressRealFFmpegNestedHLSRejectsNonHTTPProtocols(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	tests := []struct {
		name string
		body func(string) string
	}{
		{
			name: "segment tcp",
			body: func(target string) string {
				return "#EXTINF:0.5,\ntcp://" + target + "/secret.ts\n"
			},
		},
		{
			name: "segment tls",
			body: func(target string) string {
				return "#EXTINF:0.5,\ntls://" + target + "/secret.ts\n"
			},
		},
		{
			name: "key tcp",
			body: func(target string) string {
				return "#EXT-X-KEY:METHOD=AES-128,URI=\"tcp://" + target +
					"/secret.key\",IV=0x00000000000000000000000000000001\n" +
					"#EXTINF:0.5,\n0.ts\n"
			},
		},
		{
			name: "key tls",
			body: func(target string) string {
				return "#EXT-X-KEY:METHOD=AES-128,URI=\"tls://" + target +
					"/secret.key\",IV=0x00000000000000000000000000000001\n" +
					"#EXTINF:0.5,\n0.ts\n"
			},
		},
		{
			name: "init map tcp",
			body: func(target string) string {
				return "#EXT-X-MAP:URI=\"tcp://" + target + "/secret.mp4\"\n" +
					"#EXTINF:0.5,\n0.m4s\n"
			},
		},
		{
			name: "init map tls",
			body: func(target string) string {
				return "#EXT-X-MAP:URI=\"tls://" + target + "/secret.mp4\"\n" +
					"#EXTINF:0.5,\n0.m4s\n"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen for nested protocol probe: %v", err)
			}
			var internalHits atomic.Int32
			acceptDone := make(chan struct{})
			go func() {
				defer close(acceptDone)
				for {
					conn, acceptErr := listener.Accept()
					if acceptErr != nil {
						return
					}
					internalHits.Add(1)
					_ = conn.Close()
				}
			}()
			t.Cleanup(func() {
				_ = listener.Close()
				select {
				case <-acceptDone:
				case <-time.After(2 * time.Second):
				}
			})

			manifestBody := tc.body(listener.Addr().String())
			var manifestHits atomic.Int32
			public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				manifestHits.Add(1)
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				_, _ = io.WriteString(w,
					"#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:1\n"+
						"#EXT-X-MEDIA-SEQUENCE:0\n"+manifestBody+"#EXT-X-ENDLIST\n")
			}))
			t.Cleanup(public.Close)
			publicURL, _ := url.Parse(public.URL)
			target := "http://public.example:" + publicURL.Port() + "/index.m3u8"
			lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
				host: "public.example", ip: "203.0.113.97", target: public.URL,
			}})
			controller, err := newEgressController(
				[]egressTarget{{URL: target}},
				egressOptions{lookup: lookup, dial: dial})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "ffmpeg",
				"-hide_banner", "-loglevel", "error",
				"-protocol_whitelist", egressRemoteProtocolWhitelist,
				"-http_proxy", controller.ProxyURL(),
				"-i", target,
				"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
			cmd.Env = secureMediaEnv("")
			output, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Fatalf("ffmpeg accepted a direct non-HTTP URI: %s", output)
			}
			if internalHits.Load() != 0 {
				t.Fatalf("nested HLS opened a direct connection %d time(s): %s",
					internalHits.Load(), output)
			}
			if manifestHits.Load() == 0 {
				t.Fatal("nested protocol test never reached the public manifest")
			}
		})
	}
}

func TestEgressCredentialedHTTPPlaylistCannotTunnelCredentialsToHTTPS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	var tlsHits atomic.Int32
	tlsChild := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tlsHits.Add(1)
		if value := r.Header.Get("Authorization"); value != "" {
			t.Errorf("HTTPS child received Authorization %q", value)
		}
		_, _ = io.WriteString(w, "not media")
	}))
	t.Cleanup(tlsChild.Close)

	var manifestHits atomic.Int32
	manifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestHits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Basic source-secret" {
			t.Errorf("initial HTTP request Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprintf(w,
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.5,\n%s/secret.ts\n#EXT-X-ENDLIST\n",
			tlsChild.URL)
	}))
	t.Cleanup(manifest.Close)
	manifestURL, _ := url.Parse(manifest.URL)
	target := "http://public.example:" + manifestURL.Port() + "/index.m3u8"
	lookup, dial, _ := egressTestNetwork(t, []egressTestRoute{{
		host: "public.example", ip: "203.0.113.99", target: manifest.URL,
	}})
	controller, err := newEgressController(
		[]egressTarget{{URL: target}},
		egressOptions{lookup: lookup, dial: dial, denyConnect: true})
	if err != nil {
		t.Fatalf("new controlled proxy: %v", err)
	}
	defer controller.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-headers", "Authorization: Basic source-secret\r\n",
		"-protocol_whitelist", egressRemoteProtocolWhitelist,
		"-http_proxy", controller.ProxyURL(),
		"-i", target,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-")
	cmd.Env = secureMediaEnv("")
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("credentialed HTTP HLS unexpectedly reached an HTTPS child: %s", output)
	}
	if manifestHits.Load() == 0 {
		t.Fatal("playlist test never reached the initial HTTP manifest")
	}
	if tlsHits.Load() != 0 {
		t.Fatalf("HTTPS child accepted a tunnel %d time(s): %s", tlsHits.Load(), output)
	}
}

func runEgressChild(
	ctx context.Context,
	proxyRaw, target, mode, rangeValue string,
) ([]byte, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-http_proxy", proxyRaw, target)
	cmd.Env = append(os.Environ(),
		"DORIYA_EGRESS_HELPER=1",
		"DORIYA_EGRESS_HELPER_MODE="+mode,
		"DORIYA_EGRESS_HELPER_RANGE="+rangeValue,
	)
	return cmd.CombinedOutput()
}

func TestSecureFFmpegArgsUsesControlledProxyForEveryRemoteInput(t *testing.T) {
	args := []string{
		"-http_proxy", "http://127.0.0.1:1",
		"-i", "http://public.example/video.mkv",
		"-i", "/srv/local/audio.m4a",
		"-map", "0:v:0", "-map", "1:a:0", "-f", "mpegts", "pipe:1",
	}
	secured, controller, err := secureFFmpegArgs(args, newEgressTrust())
	if err != nil {
		t.Fatalf("secure ffmpeg args: %v", err)
	}
	if controller == nil {
		t.Fatal("remote input did not receive a controlled proxy")
	}
	defer controller.Close()
	proxyCount := 0
	for _, arg := range secured {
		if arg == "-http_proxy" {
			proxyCount++
		}
	}
	if proxyCount != 1 {
		t.Fatalf("controlled proxy count = %d, want 1: %#v", proxyCount, secured)
	}
	if !containsEgressArg(secured, "/srv/local/audio.m4a") {
		t.Fatal("local input was removed")
	}
	if containsEgressArg(secured, "http://127.0.0.1:1") {
		t.Fatal("source proxy was not replaced")
	}
	remoteProtocolOptions := 0
	localProtocolOptions := 0
	for i, arg := range secured {
		if arg == "-protocol_whitelist" {
			if i+1 >= len(secured) {
				t.Fatalf("protocol whitelist has no value: %#v", secured)
			}
			if containsDelimitedToken(secured[i+1], "file") {
				localProtocolOptions++
			} else {
				remoteProtocolOptions++
			}
		}
	}
	if remoteProtocolOptions != 1 || localProtocolOptions != 1 {
		t.Fatalf("protocol whitelists remote=%d local=%d, want 1 each: %#v",
			remoteProtocolOptions, localProtocolOptions, secured)
	}
	if controller.DefaultProxyURL() == "" {
		t.Fatal("single remote input did not receive an environment fallback")
	}
}

func TestSecureFFmpegArgsDeniesNestedNetworkForLocalInputs(t *testing.T) {
	local := filepath.Join(t.TempDir(), "movie.mkv")
	args := []string{
		"-http_proxy", "http://127.0.0.1:1",
		"-i", local,
		"-map", "0:v:0", "-f", "null", "-",
	}
	secured, controllers, err := secureFFmpegArgs(args, newEgressTrust())
	if err != nil {
		t.Fatalf("secure ffmpeg args: %v", err)
	}
	if controllers == nil {
		t.Fatal("local input did not receive an egress controller set")
	}
	defer controllers.Close()
	if len(controllers.controllers) != 0 {
		t.Fatalf("local input received trusted proxies: %d", len(controllers.controllers))
	}
	if controllers.fallback == nil {
		t.Fatal("local input did not receive a deny-all environment fallback")
	}
	if controllers.DefaultProxyURL() == "" {
		t.Fatal("local input did not receive a deny-all proxy")
	}
	if containsEgressArg(secured, "http://127.0.0.1:1") {
		t.Fatal("local input kept the caller's proxy")
	}
	found := false
	formatFound := false
	for i, arg := range secured {
		if arg == "-format_whitelist" {
			if i+1 >= len(secured) {
				t.Fatalf("format whitelist has no value: %#v", secured)
			}
			formatFound = true
			for _, rejected := range []string{"hls", "dash", "concat"} {
				if containsDelimitedToken(secured[i+1], rejected) {
					t.Fatalf("local format whitelist allows %s: %q", rejected, secured[i+1])
				}
			}
		}
		if arg == "-protocol_whitelist" {
			found = true
			if i+1 >= len(secured) {
				t.Fatalf("protocol whitelist has no value: %#v", secured)
			}
			value := secured[i+1]
			if !containsDelimitedToken(value, "file") {
				t.Fatalf("local protocol whitelist does not allow file: %q", value)
			}
			for _, networkScheme := range []string{"http", "https", "httpproxy", "tls", "tcp"} {
				if containsDelimitedToken(value, networkScheme) {
					t.Fatalf("local protocol whitelist allows %s: %q", networkScheme, value)
				}
			}
		}
	}
	if !found || !formatFound {
		t.Fatalf("local input has no protocol whitelist: %#v", secured)
	}
}

func TestSecureFFmpegRejectsLocalPlaylistsBeforeStartingChild(t *testing.T) {
	var networkHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networkHits.Add(1)
		_, _ = io.WriteString(w, "not media")
	}))
	t.Cleanup(upstream.Close)

	playlist := filepath.Join(t.TempDir(), "index.m3u8")
	body := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.5,\n" +
		upstream.URL + "/secret.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(playlist, []byte(body), 0o600); err != nil {
		t.Fatalf("write local playlist: %v", err)
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", playlist,
		"-t", "0.5", "-map", "0:v:0", "-f", "null", "-",
	}
	_, controllers, err := secureFFmpegArgs(args, newEgressTrust())
	if err == nil {
		if controllers != nil {
			controllers.Close()
		}
		t.Fatal("local playlist was accepted")
	}
	if networkHits.Load() != 0 {
		t.Fatalf("local playlist reached the network %d time(s)", networkHits.Load())
	}
}

func TestSecureProbeEgressForLocalTargetDeniesNetwork(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "not media")
	}))
	t.Cleanup(upstream.Close)

	controller, err := secureEgressProxy(
		filepath.Join(t.TempDir(), "movie.mkv"), "", "", newEgressTrust())
	if err != nil {
		t.Fatalf("create local probe egress: %v", err)
	}
	if controller == nil {
		t.Fatal("local probe received no egress controller")
	}
	defer controller.Close()

	resp, err := egressTestProxyClient(t, controller).Get(upstream.URL + "/child.ts")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("deny-all probe proxy returned success for %s", upstream.URL)
	}
	if hits.Load() != 0 {
		t.Fatalf("local probe reached the network %d time(s)", hits.Load())
	}
}

func TestSecureFFmpegRejectsSensitiveHeadersOnHTTPSInput(t *testing.T) {
	for _, header := range []string{
		"Authorization: Basic source-secret",
		"Cookie: session=source-secret",
		"X-OpenList-Token: source-secret",
	} {
		t.Run(strings.Split(header, ":")[0], func(t *testing.T) {
			args := []string{
				"-headers", header + "\r\n",
				"-i", "https://media.example.test/movie.mkv",
				"-f", "null", "-",
			}
			if _, _, err := secureFFmpegArgs(args, newEgressTrust()); err == nil {
				t.Fatalf("HTTPS input accepted sensitive header %q", header)
			}
		})
	}
}

func TestSecureFFmpegHTTPSHeaderPolicyKeepsReviewedHeadersOnly(t *testing.T) {
	t.Run("safe referer", func(t *testing.T) {
		args := []string{
			"-headers", "Referer: https://www.bilibili.com/\r\nUser-Agent: DoriyaMedia-Test\r\n",
			"-i", "https://media.example.test/movie.mkv",
			"-f", "null", "-",
		}
		secured, controllers, err := secureFFmpegArgs(args, newEgressTrust())
		if err != nil {
			t.Fatalf("safe HTTPS headers were rejected: %v", err)
		}
		if controllers == nil {
			t.Fatal("safe HTTPS input received no egress controller")
		}
		defer controllers.Close()
		if !containsEgressArg(secured, "Referer: https://www.bilibili.com/\r\nUser-Agent: DoriyaMedia-Test\r\n") {
			t.Fatalf("reviewed headers were not preserved: %#v", secured)
		}
	})

	t.Run("http credentials remain supported", func(t *testing.T) {
		args := []string{
			"-headers", "Authorization: Basic source-secret\r\n",
			"-i", "http://media.example.test/movie.mkv",
			"-f", "null", "-",
		}
		secured, controllers, err := secureFFmpegArgs(args, newEgressTrust())
		if err != nil {
			t.Fatalf("HTTP credentials were rejected: %v", err)
		}
		if controllers == nil {
			t.Fatal("HTTP input received no egress controller")
		}
		defer controllers.Close()
		if !containsEgressArg(secured, "Authorization: Basic source-secret\r\n") {
			t.Fatalf("HTTP credentials were not preserved: %#v", secured)
		}
		if len(controllers.controllers) != 1 || !controllers.controllers[0].policy.denyConnect {
			t.Fatal("HTTP credentials did not disable opaque TLS tunnels")
		}
		req := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
		req.Host = "secure.example.test:443"
		rec := httptest.NewRecorder()
		controllers.controllers[0].serveHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("credentialed HTTP input CONNECT status = %d, want 403", rec.Code)
		}
	})
}

func TestSecureEgressProxyDeniesTLSUpgradeForCredentialedHTTPProbe(t *testing.T) {
	controller, err := secureEgressProxy(
		"http://media.example.test/movie.mkv",
		"",
		"Authorization: Basic source-secret\r\n",
		newEgressTrust(),
	)
	if err != nil {
		t.Fatalf("secure probe egress: %v", err)
	}
	defer controller.Close()
	if !controller.policy.denyConnect {
		t.Fatal("credentialed HTTP probe permitted a TLS tunnel")
	}
}

func TestEgressHeaderConnectPolicyKeepsReviewedHTTPHeadersCompatible(t *testing.T) {
	deny, err := egressHeaderConnectPolicy(
		"http://media.example.test/index.m3u8",
		"Referer: https://www.bilibili.com/\r\nUser-Agent: DoriyaMedia-Test\r\n",
	)
	if err != nil {
		t.Fatalf("reviewed HTTP headers: %v", err)
	}
	if deny {
		t.Fatal("reviewed HTTP headers disabled TLS tunnels")
	}
}

func TestSecureFFmpegRejectsUnmodelledHeaderOptions(t *testing.T) {
	for _, option := range []string{"-cookies", "-referer", "-user_agent", "-user-agent", "-auth_type"} {
		t.Run(option, func(t *testing.T) {
			args := []string{
				option, "source-secret",
				"-i", "http://media.example.test/movie.mkv",
				"-f", "null", "-",
			}
			if _, controllers, err := secureFFmpegArgs(args, newEgressTrust()); err == nil {
				if controllers != nil {
					controllers.Close()
				}
				t.Fatalf("secure args accepted %s", option)
			}
		})
	}
}

func TestEgressRejectsLocalManifestPathsAndContent(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		file string
		body string
	}{
		{
			name: "hls extension",
			file: "index.m3u8",
			body: "#EXTM3U\n#EXTINF:1,\nfile:C:\\outside\\secret.ts\n",
		},
		{
			name: "dash extension",
			file: "index.mpd",
			body: `<?xml version="1.0"?><MPD><Period/></MPD>`,
		},
		{
			name: "hls content without extension",
			file: "movie.bin",
			body: "\xef\xbb\xbf  #EXTM3U\n#EXTINF:1,\n\\\\server\\share\\secret.ts\n",
		},
		{
			name: "dash content without extension",
			file: "movie.dat",
			body: "<?xml version=\"1.0\"?><!-- source --><MPD xmlns=\"urn:mpeg:dash:schema:mpd:2011\">" +
				"<Period/></MPD>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, controllers, err := secureFFmpegArgs(
				[]string{"-i", path, "-f", "null", "-"},
				newEgressTrust(),
			); err == nil {
				if controllers != nil {
					controllers.Close()
				}
				t.Fatalf("local manifest %q was accepted", path)
			}
		})
	}
}

func TestSecureFFmpegArgsAllowsLocalExtM3UMetadataButRejectsPlaylistAtStart(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		file    string
		body    string
		wantErr bool
	}{
		{
			name: "metadata marker",
			file: "metadata.mkv",
			body: "MATROSKA\x00\x00comment=#EXTM3U is ordinary metadata, not a playlist\n" +
				"payload\x00\x00",
		},
		{
			name:    "playlist at start",
			file:    "playlist-at-start.mkv",
			body:    "#EXTM3U\n#EXTINF:1,\nfile:C:\\outside\\secret.ts\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, controllers, err := secureFFmpegArgs(
				[]string{"-i", path, "-f", "null", "-"},
				newEgressTrust(),
			)
			if tc.wantErr {
				if err == nil {
					if controllers != nil {
						controllers.Close()
					}
					t.Fatal("local playlist beginning with #EXTM3U was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("local media with non-leading #EXTM3U metadata was rejected: %v", err)
			}
			if controllers == nil {
				t.Fatal("local media did not receive an egress controller")
			}
			controllers.Close()
		})
	}
}

func containsDelimitedToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if part == token {
			return true
		}
	}
	return false
}

func TestSecureFFmpegArgsIsolatesRemoteInputs(t *testing.T) {
	args := []string{
		"-i", "http://public.example/video.mkv",
		"-i", "http://nas.example/audio.m4a",
		"-map", "0:v:0", "-map", "1:a:0", "-f", "mpegts", "pipe:1",
	}
	secured, controllers, err := secureFFmpegArgs(args, newEgressTrust())
	if err != nil {
		t.Fatalf("secure ffmpeg args: %v", err)
	}
	if controllers == nil {
		t.Fatal("remote inputs did not receive controllers")
	}
	defer controllers.Close()
	if len(controllers.controllers) != 2 {
		t.Fatalf("controllers = %d, want one per remote input", len(controllers.controllers))
	}
	var proxies []string
	for i := 0; i+1 < len(secured); i++ {
		if secured[i] == "-http_proxy" {
			proxies = append(proxies, secured[i+1])
		}
	}
	if len(proxies) != 2 {
		t.Fatalf("controlled proxies = %d, want 2: %#v", len(proxies), secured)
	}
	if proxies[0] == proxies[1] {
		t.Fatalf("remote inputs shared one proxy origin boundary: %q", proxies[0])
	}
	if controllers.fallback == nil {
		t.Fatal("multiple remote inputs did not receive a deny-all environment fallback")
	}
	fallbackTarget, _ := url.Parse("https://public.example/stream.m3u8")
	if _, _, err := controllers.fallback.policy.authorize(
		context.Background(), fallbackTarget); err == nil {
		t.Fatal("deny-all environment fallback authorized a destination")
	}
}

func TestPackagerRemoteInputDoesNotAllowFileProtocol(t *testing.T) {
	args, err := packagerArgs(PackagerOptions{
		Target:       "https://public.example/video.m3u8",
		VideoMode:    "copy",
		AudioOrdinal: -1,
		Dir:          t.TempDir(),
	})
	if err != nil {
		t.Fatalf("packager args: %v", err)
	}
	secured, controllers, err := secureFFmpegArgs(args, newEgressTrust())
	if err != nil {
		t.Fatalf("secure packager args: %v", err)
	}
	if controllers == nil {
		t.Fatal("packager remote input did not receive a controlled proxy")
	}
	defer controllers.Close()

	found := false
	for i, arg := range secured {
		if arg != "-protocol_whitelist" {
			continue
		}
		found = true
		if i+1 >= len(secured) ||
			containsDelimitedToken(secured[i+1], "file") {
			t.Fatalf("packager remote input allowed file: %#v", secured)
		}
	}
	if !found {
		t.Fatalf("packager remote input has no protocol whitelist: %#v", secured)
	}
}

func TestSecureMediaEnvDropsInheritedProxyConfiguration(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://inherited.example:8080")
	t.Setenv("https_proxy", "http://inherited.example:8443")
	t.Setenv("ALL_PROXY", "socks5://inherited.example:1080")
	t.Setenv("No_Proxy", "nas.example")
	t.Setenv("DORIYA_EGRESS_KEEP", "yes")

	var sawKeep, sawHTTPProxy, sawHTTPSProxy bool
	for _, entry := range secureMediaEnv("http://127.0.0.1:43210") {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			if strings.Contains(entry, "inherited.example") {
				t.Fatalf("inherited proxy variable survived: %s", name)
			}
			if name == "http_proxy" || strings.EqualFold(name, "HTTP_PROXY") {
				sawHTTPProxy = strings.HasSuffix(entry, "http://127.0.0.1:43210")
			}
			if strings.EqualFold(name, "HTTPS_PROXY") {
				sawHTTPSProxy = strings.HasSuffix(entry, "http://127.0.0.1:43210")
			}
		case "DORIYA_EGRESS_KEEP":
			sawKeep = true
		}
	}
	if !sawKeep {
		t.Fatal("unrelated environment variable was removed")
	}
	if !sawHTTPProxy || !sawHTTPSProxy {
		t.Fatal("controlled proxy was not installed as the HTTP fallback")
	}
}

func TestEgressTrustedOriginCannotChangeAddressClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		next  string
	}{
		{name: "public to private", first: "203.0.113.80", next: "10.90.0.8"},
		{name: "private to public", first: "10.90.0.8", next: "203.0.113.80"},
		{name: "private to loopback", first: "10.90.0.8", next: "127.0.0.1"},
		{name: "loopback to private", first: "127.0.0.1", next: "10.90.0.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			lookup := func(context.Context, string) ([]net.IPAddr, error) {
				raw := tc.first
				if calls.Add(1) > 1 {
					raw = tc.next
				}
				return []net.IPAddr{{IP: net.ParseIP(raw)}}, nil
			}
			trust := newEgressTrust()
			trust.addURL("https://nas.example:8443")
			controller, err := newEgressController(
				[]egressTarget{{URL: "https://nas.example:8443/video"}},
				egressOptions{trust: trust, lookup: lookup})
			if err != nil {
				t.Fatalf("new controlled proxy: %v", err)
			}
			defer controller.Close()
			target, _ := url.Parse("https://nas.example:8443/video")
			if _, _, err := controller.policy.authorize(
				context.Background(), target); err != nil {
				t.Fatalf("first resolution was rejected: %v", err)
			}
			if _, _, err := controller.policy.authorize(
				context.Background(), target); err == nil {
				t.Fatal("changed address class was accepted")
			}
		})
	}
}

func containsEgressArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestEgressControllerAllowsValidatedHTTPSOriginOnSameHostPort(t *testing.T) {
	trust := newEgressTrust()
	trust.addURL("https://nas.example:8443")
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "nas.example" {
			return nil, errors.New("unexpected host")
		}
		return []net.IPAddr{{IP: net.ParseIP("10.60.0.8")}}, nil
	}
	var dials atomic.Int32
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("stop after policy")
	}
	controller, err := newEgressController(
		[]egressTarget{{URL: "https://nas.example:8443/media"}},
		egressOptions{trust: trust, lookup: lookup, dial: dial})
	if err != nil {
		t.Fatalf("new HTTPS controlled proxy: %v", err)
	}
	defer controller.Close()
	policy := controller.policy
	target, _ := url.Parse("https://nas.example:8443/media")
	if _, _, err := policy.authorize(context.Background(), target); err != nil {
		t.Fatalf("exact HTTPS origin was rejected: %v", err)
	}
	ctx := context.WithValue(context.Background(), egressSchemeContextKey{}, "https")
	if _, err := policy.dialContext(ctx, "tcp", "nas.example:8443"); err == nil {
		t.Fatal("policy dial unexpectedly succeeded past the test dial hook")
	}
	if dials.Load() != 1 {
		t.Fatalf("validated HTTPS dials = %d, want 1", dials.Load())
	}
	httpCtx := context.WithValue(context.Background(), egressSchemeContextKey{}, "http")
	if _, err := policy.dialContext(httpCtx, "tcp", "nas.example:8443"); err == nil {
		t.Fatal("HTTP request reused the HTTPS administrator trust entry")
	}
}

func TestEgressOriginCanonicalizesDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: "HTTPS://NAS.EXAMPLE./media", want: "https://nas.example:443"},
		{raw: "http://nas.example:80/media", want: "http://nas.example:80"},
		{raw: "http://[fd00::8]:8080/media", want: "http://[fd00::8]:8080"},
	} {
		parsed, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		got, err := egressOrigin(parsed)
		if err != nil {
			t.Fatalf("origin %q: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("origin %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestEgressRemoteTargetRejectsProtocolBypasses(t *testing.T) {
	for _, target := range []string{
		"concat:http://public.example/a|http://public.example/b",
		"tcp://127.0.0.1:9000/video.ts",
		"crypto:tcp://127.0.0.1:9000/video.ts",
		"file://server/share/movie.mkv",
		"http://public.example/%zz",
		"httpproxy:http://127.0.0.1:9000/video.ts",
	} {
		if remote, err := egressRemoteTarget(target); err == nil || remote {
			t.Errorf("target %q = remote=%v err=%v, want a closed failure",
				target, remote, err)
		}
	}
	for _, target := range []string{
		"/srv/media/movie.mkv",
		`C:\media\movie.mkv`,
		"/srv/media/100% real.mkv",
		`C:\media\100% real.mkv`,
		"/srv/media/part:1.mkv",
		"file:///C:/media/movie.mkv",
	} {
		if remote, err := egressRemoteTarget(target); err != nil || remote {
			t.Errorf("local target %q = remote=%v err=%v, want local", target, remote, err)
		}
	}
}
