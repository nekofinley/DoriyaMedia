package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

func TestPlaybackSecurityCrossOriginRedirectStripsSourceHeaders(t *testing.T) {
	const (
		authorization = "Bearer source-authorization-secret"
		cookie        = "source_session=source-cookie-secret"
		sourceToken   = "source-header-secret"
	)

	initialHeaders := make(chan http.Header, 1)
	finalHeaders := make(chan http.Header, 1)

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data")
	}))
	t.Cleanup(final.Close)

	var finalPublicURL string
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initialHeaders <- r.Header.Clone()
		http.Redirect(w, r, finalPublicURL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	finalURL, _ := url.Parse(final.URL)
	redirectURL, _ := url.Parse(redirector.URL)
	finalPublicURL = "http://public-final.example:" + finalURL.Port() + "/media.mp4"
	startTarget := "http://public-start.example:" + redirectURL.Port() +
		"/start?token=source-query-secret"
	lookup, dial := playbackSecurityPublicNetwork(t, map[string]string{
		"public-start.example": redirector.URL,
		"public-final.example": final.URL,
	})

	sourceHeaders := map[string]string{
		"Authorization":  authorization,
		"Cookie":         cookie,
		"X-Source-Token": sourceToken,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, startTarget, "", sourceHeaders, nil, lookup, dial)
	if err != nil {
		t.Fatalf("create playback proxy client: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/playback/media-sessions/ms_test/stream", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	serveV1ProxyMedia(rec, req, startTarget, sourceHeaders, client)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	first := receivePlaybackSecurityHeaders(t, initialHeaders)
	for name, want := range map[string]string{
		"Authorization":  authorization,
		"Cookie":         cookie,
		"X-Source-Token": sourceToken,
	} {
		if got := first.Get(name); got != want {
			t.Errorf("initial upstream %s = %q, want %q", name, got, want)
		}
	}

	last := receivePlaybackSecurityHeaders(t, finalHeaders)
	for _, name := range []string{"Authorization", "Cookie", "X-Source-Token", "Referer"} {
		if got := last.Get(name); got != "" {
			t.Errorf("cross-origin redirect forwarded %s = %q", name, got)
		}
	}
}

func TestOpenSourceReadRejectsLoopbackURLDriver(t *testing.T) {
	var hits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, "internal-secret")
	}))
	t.Cleanup(internal.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rc, err := openSourceRead(ctx, &source.URLDriver{}, internal.URL+"/secret")
	if err == nil {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatal("URLDriver read reached a loopback target")
	}
	if hits != 0 {
		t.Fatalf("loopback target was reached %d time(s)", hits)
	}
}

func TestPlaybackSecurityRedirectStateRejectsUnsafeTargets(t *testing.T) {
	previous := newPlaybackSecurityRequest(t, "https://public.example/start")
	lookup := playbackSecurityLookup(t, map[string][]string{
		"public.example": {"203.0.113.10"},
		"media.example":  {"203.0.113.11"},
	})
	tests := []struct {
		name   string
		target string
	}{
		{name: "HTTPS downgrade", target: "http://public.example/live"},
		{name: "private IPv4", target: "https://10.10.0.8/live"},
		{name: "private IPv6", target: "https://[fd00::8]/live"},
		{name: "loopback IPv4", target: "http://127.0.0.1/live"},
		{name: "loopback IPv6", target: "https://[::1]/live"},
		{name: "localhost name", target: "https://printer.localhost/live"},
		{name: "link-local metadata", target: "http://169.254.169.254/latest/meta-data"},
		{name: "shared address space", target: "http://100.64.0.1/live"},
		{name: "userinfo", target: "https://user:password@public.example/live"},
		{name: "unsupported scheme", target: "ftp://public.example/live"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &v1PlaybackRedirectState{
				initialOrigin:  "https://public.example:443",
				initialPrivate: false,
				lookup:         lookup,
			}
			candidate := newPlaybackSecurityRequest(t, tc.target)
			if err := state.check(candidate, []*http.Request{previous}); err == nil {
				t.Fatalf("redirect to %q was accepted", tc.target)
			}
		})
	}

	state := &v1PlaybackRedirectState{
		initialOrigin:  "https://public.example:443",
		initialPrivate: false,
		lookup:         lookup,
	}
	sameOrigin := newPlaybackSecurityRequest(t, "https://public.example/next-path")
	if err := state.check(sameOrigin, []*http.Request{previous}); err != nil {
		t.Fatalf("same-origin public redirect was rejected: %v", err)
	}
	publicTarget := newPlaybackSecurityRequest(t, "https://media.example/live")
	if err := state.check(publicTarget, []*http.Request{previous}); err != nil {
		t.Fatalf("same-class public HTTPS redirect was rejected: %v", err)
	}
}

func TestPlaybackSecurityRedirectStateStripsHeadersAcrossWholeChain(t *testing.T) {
	state := &v1PlaybackRedirectState{
		initialOrigin:  "https://public-start.example:443",
		initialPrivate: false,
		lookup: playbackSecurityLookup(t, map[string][]string{
			"public-start.example": {"203.0.113.10"},
			"public-middle.example": {
				"203.0.113.11",
			},
		}),
		headerNames: []string{"Authorization", "Cookie", "X-Source-Token"},
	}
	first := newPlaybackSecurityRequest(t, "https://public-start.example/start")
	middle := newPlaybackSecurityRequest(t, "https://public-middle.example/start")
	final := newPlaybackSecurityRequest(t, "https://public-middle.example/collect")
	for _, name := range state.headerNames {
		final.Header.Set(name, "source-secret")
	}
	final.Header.Set("Referer", "https://public-start.example/start?token=secret")

	if err := state.check(final, []*http.Request{first, middle}); err != nil {
		t.Fatalf("second same-origin hop was rejected: %v", err)
	}
	for _, name := range append(append([]string{}, state.headerNames...), "Referer") {
		if got := final.Header.Get(name); got != "" {
			t.Errorf("multi-hop redirect forwarded %s = %q", name, got)
		}
	}
}

func TestPlaybackSecurityRedirectStateBoundsHops(t *testing.T) {
	state := &v1PlaybackRedirectState{
		initialOrigin:  "https://public.example:443",
		initialPrivate: false,
		lookup: playbackSecurityLookup(t, map[string][]string{
			"public.example": {"203.0.113.10"},
		}),
	}
	target := newPlaybackSecurityRequest(t, "https://public.example/final")

	fourHops := make([]*http.Request, 4)
	for i := range fourHops {
		fourHops[i] = newPlaybackSecurityRequest(t, "https://public.example/step")
	}
	if err := state.check(target, fourHops); err != nil {
		t.Fatalf("four redirect hops were rejected: %v", err)
	}

	fiveHops := make([]*http.Request, 5)
	for i := range fiveHops {
		fiveHops[i] = newPlaybackSecurityRequest(t, "https://public.example/step")
	}
	err := state.check(target, fiveHops)
	if err == nil {
		t.Fatal("five redirect hops were accepted")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("redirect limit error = %q", err)
	}
}

func TestPlaybackSecurityPrivateInitialRedirectPolicy(t *testing.T) {
	state := &v1PlaybackRedirectState{
		initialOrigin:        "https://10.0.0.8:443",
		initialPrivate:       true,
		trustedPrivateOrigin: "https://10.0.0.8:443",
	}
	previous := newPlaybackSecurityRequest(t, "https://10.0.0.8/start")

	for _, target := range []string{
		"https://127.0.0.1/latest/meta-data",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.9/other-private-host",
	} {
		t.Run(target, func(t *testing.T) {
			candidate := newPlaybackSecurityRequest(t, target)
			if err := state.check(candidate, []*http.Request{previous}); err == nil {
				t.Fatalf("private initial source redirected to %q", target)
			}
		})
	}

	sameHost := newPlaybackSecurityRequest(t, "https://10.0.0.8:8443/next-path")
	if err := state.check(sameHost, []*http.Request{previous}); err == nil {
		t.Fatal("private redirect to the same host on another port was accepted")
	}
	sameOrigin := newPlaybackSecurityRequest(t, "https://10.0.0.8/next-path")
	if err := state.check(sameOrigin, []*http.Request{previous}); err != nil {
		t.Fatalf("same-origin private redirect was rejected: %v", err)
	}
}

func TestPlaybackSecurityRejectsMixedAndResolvedPrivateDNS(t *testing.T) {
	lookup := playbackSecurityLookup(t, map[string][]string{
		"public.example": {"203.0.113.10"},
		"mixed.example":  {"203.0.113.20", "10.20.0.5"},
		"private.example": {
			"10.30.0.6",
		},
	})
	ctx := context.Background()

	if _, _, err := v1PlaybackResolveHost(ctx, "mixed.example", lookup); err == nil {
		t.Fatal("initial mixed public/private resolution was accepted")
	}
	if _, _, err := v1PlaybackResolveHost(ctx, "private.example", lookup); err != nil {
		t.Fatalf("private-only resolution failed: %v", err)
	}

	previous := newPlaybackSecurityRequest(t, "https://public.example/start")
	state := &v1PlaybackRedirectState{
		initialOrigin:  "https://public.example:443",
		initialPrivate: false,
		lookup:         lookup,
	}
	for _, target := range []string{
		"https://mixed.example/live",
		"https://private.example/live",
	} {
		t.Run(target, func(t *testing.T) {
			candidate := newPlaybackSecurityRequest(t, target)
			if err := state.check(candidate, []*http.Request{previous}); err == nil {
				t.Fatalf("public source redirected to %q", target)
			}
		})
	}
}

func TestPlaybackSecurityProxyClientRejectsMixedDNSInitialHost(t *testing.T) {
	lookup := playbackSecurityLookup(t, map[string][]string{
		"mixed.example": {"203.0.113.20", "10.20.0.5"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientWithLookup(
		ctx, "https://mixed.example/media", "", nil, lookup)
	if err == nil {
		client.CloseIdleConnections()
		t.Fatal("proxy client accepted a mixed public/private initial host")
	}
	if !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("mixed DNS error = %q", err)
	}
}

func TestPlaybackSecurityDialPolicyRejectsResolvedPrivateAddresses(t *testing.T) {
	tests := []struct {
		name string
		host string
		ips  []string
	}{
		{name: "private only", host: "private.example", ips: []string{"10.40.0.7"}},
		{
			name: "mixed DNS",
			host: "mixed.example",
			ips:  []string{"203.0.113.40", "10.40.0.8"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := &v1PlaybackDialPolicy{
				lookup: playbackSecurityLookup(t, map[string][]string{tc.host: tc.ips}),
				dial: func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("test dial must not be reached")
				},
			}
			ctx := context.WithValue(
				context.Background(), v1PlaybackSchemeContextKey{}, "https")
			if _, err := policy.dialContext(
				ctx, "tcp", tc.host+":443"); err == nil {
				t.Fatalf("dial policy accepted %s resolving to %v", tc.host, tc.ips)
			}
		})
	}
}

func TestPlaybackSecurityPrivateProxyRequiresConfiguredExactOrigin(t *testing.T) {
	lookup := playbackSecurityLookup(t, map[string][]string{
		"nas.example": {"10.0.0.8"},
	})
	ctx := context.Background()
	webDAV := source.NewWebDAV("http://nas.example:8080/dav", "user", "pass")
	openList, err := source.NewOpenList("http://nas.example:8080", "token")
	if err != nil {
		t.Fatalf("create OpenList driver: %v", err)
	}

	tests := []struct {
		name    string
		target  string
		drv     source.Driver
		wantErr bool
	}{
		{
			name:    "arbitrary URL source",
			target:  "http://nas.example:8080/media.mp4",
			drv:     &source.URLDriver{},
			wantErr: true,
		},
		{
			name:   "WebDAV exact origin",
			target: "http://nas.example:8080/media.mp4",
			drv:    webDAV,
		},
		{
			name:   "OpenList exact origin",
			target: "http://nas.example:8080/p/media.mp4",
			drv:    openList,
		},
		{
			name:    "same host different port",
			target:  "http://nas.example:8081/media.mp4",
			drv:     webDAV,
			wantErr: true,
		},
		{
			name:    "same host different scheme",
			target:  "https://nas.example:8080/media.mp4",
			drv:     webDAV,
			wantErr: true,
		},
		{
			name:    "metadata is never trusted",
			target:  "http://169.254.169.254/latest/meta-data",
			drv:     source.NewWebDAV("http://169.254.169.254", "", ""),
			wantErr: true,
		},
		{
			name:    "Alibaba metadata is never trusted",
			target:  "http://100.100.100.200/latest/meta-data",
			drv:     source.NewWebDAV("http://100.100.100.200", "", ""),
			wantErr: true,
		},
		{
			name:    "Azure platform endpoint is never trusted",
			target:  "http://168.63.129.16/machine",
			drv:     source.NewWebDAV("http://168.63.129.16", "", ""),
			wantErr: true,
		},
		{
			name:    "IPv6 metadata is never trusted",
			target:  "http://[fd00:ec2::254]/latest/meta-data",
			drv:     source.NewWebDAV("http://[fd00:ec2::254]", "", ""),
			wantErr: true,
		},
		{
			name:    "NAT64 metadata is never trusted",
			target:  "http://[64:ff9b::a9fe:a9fe]/latest/meta-data",
			drv:     source.NewWebDAV("http://[64:ff9b::a9fe:a9fe]", "", ""),
			wantErr: true,
		},
		{
			name:    "6to4 loopback is never trusted",
			target:  "http://[2002:7f00:1::]/loopback",
			drv:     source.NewWebDAV("http://[2002:7f00:1::]", "", ""),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := v1PlaybackProxyClientForDriverWithLookup(
				ctx, tc.target, "", nil, tc.drv, lookup)
			if err == nil {
				client.CloseIdleConnections()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("client error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestPlaybackSecurityDialRejectsDNSRebind(t *testing.T) {
	var (
		lookupCalls int
		dialCalls   int
	)
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookupCalls++
		raw := "203.0.113.60"
		if lookupCalls > 1 {
			raw = "10.60.0.9"
		}
		return []net.IPAddr{{IP: net.ParseIP(raw)}}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("test dial must not be reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, "http://rebind.example:8080/media.mp4", "", nil, nil, lookup, dial)
	if err != nil {
		t.Fatalf("create rebinding client: %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://rebind.example:8080/media.mp4", nil)
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("DNS rebinding to a private address was accepted")
	}
	if lookupCalls < 2 {
		t.Fatalf("lookup calls = %d, want the connection to resolve again", lookupCalls)
	}
	if dialCalls != 0 {
		t.Fatalf("private rebinding attempted %d dial(s)", dialCalls)
	}
}

func TestPlaybackSecurityTrustedLANRejectsLoopbackRebind(t *testing.T) {
	var (
		lookupCalls int
		dialCalls   int
	)
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookupCalls++
		raw := "10.60.0.8"
		if lookupCalls > 1 {
			raw = "127.0.0.1"
		}
		return []net.IPAddr{{IP: net.ParseIP(raw)}}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("test dial must not be reached")
	}
	drv := source.NewWebDAV("http://nas.example:8080/dav", "", "")
	target := "http://nas.example:8080/dav/movie.mp4"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, target, "", nil, drv, lookup, dial)
	if err != nil {
		t.Fatalf("create trusted LAN client: %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("trusted LAN hostname rebinding to loopback was accepted")
	}
	if lookupCalls < 2 {
		t.Fatalf("lookup calls = %d, want independent redirect/dial resolution", lookupCalls)
	}
	if dialCalls != 0 {
		t.Fatalf("loopback rebinding attempted %d dial(s)", dialCalls)
	}
}

func TestPlaybackSecurityDialUsesTheResolvedIP(t *testing.T) {
	lookup := playbackSecurityLookup(t, map[string][]string{
		"media.example": {"203.0.113.61"},
	})
	var dialTarget string
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialTarget = address
		return nil, errors.New("stop after observing address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, "http://media.example:8080/video.mp4", "", nil, nil, lookup, dial)
	if err != nil {
		t.Fatalf("create proxy client: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://media.example:8080/video.mp4", nil)
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("test dial unexpectedly succeeded")
	}
	if dialTarget != "203.0.113.61:8080" {
		t.Fatalf("dial target = %q, want the validated IP", dialTarget)
	}
}

func TestPlaybackSecurityPublicRedirectCannotReachPrivateNetwork(t *testing.T) {
	targets := []struct {
		name     string
		location string
		lookup   map[string][]string
	}{
		{name: "loopback", location: "http://127.0.0.1:9/secret"},
		{
			name:     "RFC1918",
			location: "http://private.example:8080/secret",
			lookup:   map[string][]string{"private.example": {"10.70.0.8"}},
		},
		{name: "metadata", location: "http://169.254.169.254/latest/meta-data"},
	}
	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, tc.location, http.StatusFound)
				}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			lookup, routeDial := playbackSecurityPublicNetwork(t, map[string]string{
				"public.example": upstream.URL,
			})
			privateDialCalls := 0
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				for _, ips := range tc.lookup {
					for _, raw := range ips {
						if strings.HasPrefix(address, raw+":") {
							privateDialCalls++
						}
					}
				}
				if strings.HasPrefix(address, "127.0.0.1:") ||
					strings.HasPrefix(address, "169.254.") {
					privateDialCalls++
				}
				return routeDial(ctx, network, address)
			}
			baseLookup := lookup
			lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				ips, err := baseLookup(ctx, host)
				if err == nil {
					return ips, nil
				}
				if raw, ok := tc.lookup[host]; ok {
					out := make([]net.IPAddr, 0, len(raw))
					for _, value := range raw {
						out = append(out, net.IPAddr{IP: net.ParseIP(value)})
					}
					return out, nil
				}
				return nil, err
			}
			target := "http://public.example:" + upstreamURL.Port() + "/start"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
				ctx, target, "", nil, nil, lookup, dial)
			if err != nil {
				t.Fatalf("create proxy client: %v", err)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			resp, err := client.Do(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("public redirect to a private destination was accepted")
			}
			if privateDialCalls != 0 {
				t.Fatalf("private redirect attempted %d dial(s)", privateDialCalls)
			}
		})
	}
}

func TestPlaybackSecurityAllowsExactLANSourcesAndPreservesRange(t *testing.T) {
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Unix(1_700_000_000, 0),
			strings.NewReader(body))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	port := upstreamURL.Port()
	lookup := playbackSecurityLookup(t, map[string][]string{
		"nas.example": {"10.80.0.8"},
	})
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upstreamURL.Host)
	}
	openList, err := source.NewOpenList("http://nas.example:"+port, "token")
	if err != nil {
		t.Fatalf("create OpenList driver: %v", err)
	}
	tests := []struct {
		name   string
		target string
		drv    source.Driver
	}{
		{
			name:   "WebDAV",
			target: "http://nas.example:" + port + "/dav/clip.mp4",
			drv:    source.NewWebDAV("http://nas.example:"+port+"/dav", "", ""),
		},
		{
			name:   "OpenList",
			target: "http://nas.example:" + port + "/p/clip.mp4",
			drv:    openList,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
				ctx, tc.target, "", nil, tc.drv, lookup, dial)
			if err != nil {
				t.Fatalf("create LAN proxy client: %v", err)
			}
			defer client.CloseIdleConnections()

			rec := runPlaybackProxy(t, http.MethodGet, tc.target, nil, client,
				map[string]string{"Range": "bytes=2-5"})
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != "2345" {
				t.Fatalf("body = %q, want range bytes", got)
			}
			if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/36" {
				t.Errorf("Content-Range = %q", got)
			}
		})
	}
}

func TestPlaybackSecurityLocalIfRangeRequiresCurrentFileVersionETag(t *testing.T) {
	const etag = `"a-1-2-3"`
	tests := []struct {
		name    string
		raw     string
		etag    string
		matches bool
	}{
		{name: "matching strong ETag", raw: etag, etag: etag, matches: true},
		{name: "stale strong ETag", raw: `"old"`, etag: etag},
		{name: "weak ETag", raw: "W/" + etag, etag: etag},
		{
			name: "modification date",
			raw:  time.Unix(1_700_000_000, 0).UTC().Format(http.TimeFormat),
			etag: etag,
		},
		{name: "no reliable file version", raw: etag},
		{name: "malformed ETag", raw: `"a-1-2-3"junk`, etag: etag},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := v1LocalIfRangeMatches(tc.raw, tc.etag); got != tc.matches {
				t.Fatalf("v1LocalIfRangeMatches(%q, %q) = %v, want %v",
					tc.raw, tc.etag, got, tc.matches)
			}
		})
	}
}

func TestPlaybackSecuritySourceBridgeReceivesValidatedIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for source bridge: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requestLine := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		requestLine <- strings.TrimSpace(line)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
	}()

	lookup := playbackSecurityLookup(t, map[string][]string{
		"media.example": {"203.0.113.70"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, "http://media.example:8080/video.mp4",
		"http://"+listener.Addr().String(), nil, nil, lookup, nil)
	if err != nil {
		t.Fatalf("create bridged proxy client: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://media.example:8080/video.mp4", nil)
	resp, _ := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	select {
	case got := <-requestLine:
		if got != "CONNECT 203.0.113.70:8080 HTTP/1.1" {
			t.Fatalf("source bridge request = %q, want validated IP", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for source bridge request")
	}
}

func TestPlaybackSecurityServerProxyConfiguresPhaseTimeouts(t *testing.T) {
	lookup := playbackSecurityLookup(t, map[string][]string{
		"media.example": {"203.0.113.71"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientWithLookup(
		ctx, "https://media.example/video.mp4", "", nil, lookup)
	if err != nil {
		t.Fatalf("create playback proxy client: %v", err)
	}
	transport, ok := client.Transport.(*v1PlaybackSchemeTransport)
	if !ok || transport.base == nil {
		t.Fatalf("transport = %T, want *v1PlaybackSchemeTransport", client.Transport)
	}
	if got := transport.base.TLSHandshakeTimeout; got != v1PlaybackTLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s",
			got, v1PlaybackTLSHandshakeTimeout)
	}
	if got := transport.base.ResponseHeaderTimeout; got != v1PlaybackResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s",
			got, v1PlaybackResponseHeaderTimeout)
	}
	if client.Timeout != 0 {
		t.Errorf("total client timeout = %s, want no total timeout", client.Timeout)
	}
}

func TestPlaybackSecurityServerProxySilentSourceHonorsContext(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			listener := newPlaybackSecuritySilentListener(t)
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatalf("split silent listener address: %v", err)
			}
			lookup, dial := playbackSecurityPublicNetwork(t, map[string]string{
				"silent.example": "http://" + listener.Addr().String(),
			})
			target := scheme + "://silent.example:" + port + "/video.mp4"
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
				ctx, target, "", nil, nil, lookup, dial)
			if err != nil {
				t.Fatalf("create playback proxy client: %v", err)
			}
			t.Cleanup(client.CloseIdleConnections)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				t.Fatalf("new silent source request: %v", err)
			}

			result := make(chan error, 1)
			started := time.Now()
			go func() {
				resp, err := client.Do(req)
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				result <- err
			}()

			select {
			case err := <-result:
				if err == nil {
					t.Fatal("silent source request unexpectedly succeeded")
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("silent source error = %v, want context deadline", err)
				}
				if elapsed := time.Since(started); elapsed > time.Second {
					t.Fatalf("silent source request took %s, want less than 1s", elapsed)
				}
			case <-time.After(time.Second):
				t.Fatal("silent source request did not return within 1s")
			}
		})
	}
}

func TestPlaybackSecurityBridgeConnectHonorsContext(t *testing.T) {
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
		_, err := v1PlaybackDialHTTPProxy(
			ctx, &url.URL{Scheme: "http"}, "proxy.test:8080",
			"203.0.113.72:443",
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

func TestPlaybackSecurityBridgeConnectClearsHandshakeDeadline(t *testing.T) {
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
	conn, err := v1PlaybackDialHTTPProxy(
		ctx, &url.URL{Scheme: "http"}, "proxy.test:8080",
		"203.0.113.73:443",
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

func TestPlaybackSecurityHLSRejectsQueryBeforeAuthorizationAndSegment(t *testing.T) {
	capability := "ms_" + strings.Repeat("a", 43)
	for _, asset := range []string{"index.m3u8", "0.ts"} {
		t.Run(asset, func(t *testing.T) {
			store := &fakeV1HLSStore{access: v1HLSAccess("remux")}
			engine := &fakeV1HLSEngine{probe: v1HLSProbe(30, 2)}
			h := v1HLSTestHandler(store, engine)
			path := "/api/v1/playback/media-sessions/" + capability + "/" + asset +
				"?m=remux-aac&a=2&burn=1&q=custom&start=99&ss=99&is=secret"
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.SetPathValue("id", capability)
			req.SetPathValue("segment", asset)
			rec := httptest.NewRecorder()

			if asset == "index.m3u8" {
				h.servePlaylist(rec, req)
			} else {
				h.serveSegment(rec, req)
			}

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeV1HLSError(t, rec); got != V1ErrorInvalidRequest {
				t.Fatalf("error code = %q, want %q", got, V1ErrorInvalidRequest)
			}
			if len(store.authorizeLog) != 0 {
				t.Fatalf("query request authorized %d time(s)", len(store.authorizeLog))
			}
			if engine.playlistCalls != 0 || engine.segmentCalls != 0 {
				t.Fatalf("query request reached media engine: playlist=%d segment=%d",
					engine.playlistCalls, engine.segmentCalls)
			}
		})
	}
}

func TestPlaybackSecurityHLSPlaylistIsCapabilityOnly(t *testing.T) {
	capability := "ms_" + strings.Repeat("b", 43)
	store := &fakeV1HLSStore{
		access: v1HLSAccess("remux-aac"),
		file: &models.MediaFile{
			ID:   77,
			Path: "https://source.example/private/movie.mkv?token=source-token-secret",
		},
	}
	engine := &fakeV1HLSEngine{probe: v1HLSProbe(22, 3)}
	h := v1HLSTestHandler(store, engine)
	rec := httptest.NewRecorder()

	h.servePlaylist(rec, v1HLSPlaylistRequest(capability))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var gotURIs []string
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			gotURIs = append(gotURIs, line)
		}
	}
	wantURIs := []string{
		"/api/v1/playback/media-sessions/" + capability + "/0.ts",
		"/api/v1/playback/media-sessions/" + capability + "/1.ts",
		"/api/v1/playback/media-sessions/" + capability + "/2.ts",
	}
	if strings.Join(gotURIs, "\n") != strings.Join(wantURIs, "\n") {
		t.Fatalf("playlist URIs:\n%s\nwant:\n%s",
			strings.Join(gotURIs, "\n"), strings.Join(wantURIs, "\n"))
	}
	for _, uri := range gotURIs {
		if !strings.HasSuffix(uri, ".ts") {
			t.Errorf("playlist URI does not use a TS segment: %q", uri)
		}
		if strings.ContainsAny(uri, "?#") || strings.Contains(uri, "://") {
			t.Errorf("playlist URI is not capability-only: %q", uri)
		}
	}
	body := rec.Body.String()
	for _, secret := range []string{
		"source.example", "/private/", "source-token-secret", "token=",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("playlist leaked %q:\n%s", secret, body)
		}
	}
}

func TestPlaybackSecurityHEADStreamErrorHasNoBody(t *testing.T) {
	const targetSecret = "head-target-secret"
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed for " + req.URL.String())
	})}
	rec := runPlaybackProxy(t, http.MethodHead,
		"https://source.example/private/movie.mp4?token="+targetSecret,
		map[string]string{"Authorization": "Bearer " + targetSecret},
		client, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD error body length = %d, want 0: %q", rec.Body.Len(), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), targetSecret) {
		t.Fatalf("HEAD error leaked the source target: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != v1ProxyErrorCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, v1ProxyErrorCacheControl)
	}
}

func TestPlaybackSecurityProxyRedactsHeadersAndFailureLogs(t *testing.T) {
	const (
		querySecret  = "query-log-secret"
		headerSecret = "header-log-secret"
	)
	logs := capturePlaybackSecurityLogs(t)
	receivedHeaders := make(chan http.Header, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Set-Cookie", "source_session="+headerSecret)
		w.Header().Set("Authorization", "Bearer "+headerSecret)
		w.Header().Set("WWW-Authenticate", `Basic realm="`+headerSecret+`"`)
		w.Header().Set("X-Source-Secret", headerSecret)
		w.Header().Set("Location", "https://private.example/?token="+querySecret)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "upstream failure "+querySecret+" "+headerSecret)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	target := "http://source.example:" + upstreamURL.Port() +
		"/private/movie.mp4?token=" + querySecret
	lookup, dial := playbackSecurityPublicNetwork(t, map[string]string{
		"source.example": upstream.URL,
	})
	sourceHeaders := map[string]string{
		"Authorization":  "Bearer " + headerSecret,
		"X-Source-Token": headerSecret,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := v1PlaybackProxyClientForDriverWithLookupAndDial(
		ctx, target, "", sourceHeaders, nil, lookup, dial)
	if err != nil {
		t.Fatalf("create playback proxy client: %v", err)
	}
	rec := runPlaybackProxy(t, http.MethodGet, target, sourceHeaders, client, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	gotHeaders := receivePlaybackSecurityHeaders(t, receivedHeaders)
	if got := gotHeaders.Get("Authorization"); got != "Bearer "+headerSecret {
		t.Errorf("upstream Authorization = %q", got)
	}
	if got := gotHeaders.Get("X-Source-Token"); got != headerSecret {
		t.Errorf("upstream X-Source-Token = %q", got)
	}
	for _, name := range []string{
		"Set-Cookie", "Authorization", "WWW-Authenticate", "X-Source-Secret", "Location",
	} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("sensitive response header %s leaked: %q", name, got)
		}
	}
	if strings.Contains(rec.Body.String(), querySecret) ||
		strings.Contains(rec.Body.String(), headerSecret) {
		t.Fatalf("proxy error leaked source secrets: %q", rec.Body.String())
	}

	parsedTarget, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	gotLogs := logs.String()
	if !strings.Contains(gotLogs, "host="+parsedTarget.Hostname()) {
		t.Errorf("logs do not contain the sanitized host: %q", gotLogs)
	}
	for _, secret := range []string{
		querySecret, headerSecret, "/private/movie.mp4", "?token=",
	} {
		if strings.Contains(gotLogs, secret) {
			t.Errorf("logs leaked %q: %q", secret, gotLogs)
		}
	}
}

func newPlaybackSecurityRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request %q: %v", rawURL, err)
	}
	return req
}

func playbackSecurityPublicNetwork(
	t *testing.T,
	servers map[string]string,
) (v1PlaybackIPLookup, v1PlaybackDialFunc) {
	t.Helper()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	lookupHosts := make(map[string][]string, len(names))
	routes := make(map[string]string, len(names))
	for i, name := range names {
		serverURL, err := url.Parse(servers[name])
		if err != nil || serverURL.Hostname() == "" || serverURL.Port() == "" {
			t.Fatalf("invalid test server URL %q: %v", servers[name], err)
		}
		ip := fmt.Sprintf("203.0.113.%d", i+10)
		lookupHosts[name] = []string{ip}
		routes[net.JoinHostPort(ip, serverURL.Port())] = net.JoinHostPort(
			serverURL.Hostname(), serverURL.Port())
	}

	dialer := &net.Dialer{Timeout: 2 * time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		target, ok := routes[address]
		if !ok {
			return nil, fmt.Errorf("unexpected test dial to %s", address)
		}
		return dialer.DialContext(ctx, network, target)
	}
	return playbackSecurityLookup(t, lookupHosts), dial
}

func playbackSecurityLookup(
	t *testing.T,
	hosts map[string][]string,
) v1PlaybackIPLookup {
	t.Helper()
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		values, ok := hosts[v1PlaybackCanonicalHost(host)]
		if !ok {
			return nil, fmt.Errorf("unexpected lookup for %q", host)
		}
		ips := make([]net.IPAddr, 0, len(values))
		for _, raw := range values {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("invalid test IP %q", raw)
			}
			ips = append(ips, net.IPAddr{IP: ip})
		}
		return ips, nil
	}
}

func newPlaybackSecuritySilentListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for silent source: %v", err)
	}
	accepted := make(chan net.Conn, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})
	return listener
}

func receivePlaybackSecurityHeaders(t *testing.T, headers <-chan http.Header) http.Header {
	t.Helper()
	select {
	case got := <-headers:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream headers")
		return nil
	}
}

func capturePlaybackSecurityLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})
	return &output
}
