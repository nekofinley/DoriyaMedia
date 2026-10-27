package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyProxyCrossOriginRedirectStripsSourceHeaders(t *testing.T) {
	const (
		authCanary   = "cross-origin-auth-canary"
		cookieCanary = "cross-origin-cookie-canary"
		headerCanary = "cross-origin-header-canary"
		refererURL   = "https://private.example.test/secret?token=referer-canary"
	)

	body := strings.Repeat("0123456789", 100)
	var targetHeaders http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/final?from=redirect", http.StatusFound)
			return
		}
		targetHeaders = r.Header.Clone()
		if r.Header.Get("Range") != "bytes=100-199" {
			t.Errorf("redirected Range = %q, want bytes=100-199", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 100-199/1000")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body[100:200])
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Source-Token"); got != headerCanary {
			t.Errorf("initial source header = %q, want source canary", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+authCanary {
			t.Errorf("initial Authorization = %q, want source canary", got)
		}
		if got := r.Header.Get("User-Agent"); got != "SourceMedia/1.0" {
			t.Errorf("initial User-Agent = %q, want source canary", got)
		}
		http.Redirect(w, r, target.URL+"/first?from=source", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=100-199")
	req.Header.Set("If-Range", `"media-validator"`)
	req.Header.Set("Accept", "video/mp4")
	rec := httptest.NewRecorder()
	headers := map[string]string{
		"Authorization":       "Bearer " + authCanary,
		"Cookie":              "session=" + cookieCanary,
		"X-Source-Token":      headerCanary,
		"Referer":             refererURL,
		"Accept":              "video/mp4",
		"Accept-Encoding":     "gzip",
		"Proxy-Authorization": "Basic should-not-cross-origins",
		"User-Agent":          "SourceMedia/1.0",
	}

	client := source.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Set("Authorization", "Bearer callback-must-not-leak")
		req.Header.Set("X-Source-Token", "callback-must-not-leak")
		req.Header.Set("Accept-Encoding", "gzip")
		return nil
	}
	proxyRemoteVia(rec, req, source.URL+"/start?token=source-query", headers, client)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body[100:200] {
		t.Fatalf("body length = %d, want 100", len(got))
	}
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"X-Source-Token",
		"Proxy-Authorization",
		"Referer",
	} {
		if got := targetHeaders.Get(name); got != "" {
			t.Errorf("cross-origin redirect leaked %s = %q", name, got)
		}
	}
	if got := targetHeaders.Get("User-Agent"); got == "SourceMedia/1.0" {
		t.Errorf("cross-origin redirect leaked source User-Agent")
	}
	for name, want := range map[string]string{
		"Accept":          "video/mp4",
		"Accept-Encoding": "identity",
		"If-Range":        `"media-validator"`,
		"Range":           "bytes=100-199",
	} {
		if got := targetHeaders.Get(name); got != want {
			t.Errorf("redirected %s = %q, want %q", name, got, want)
		}
	}
}

func TestLegacyProxySameOriginRedirectKeepsSourceHeaders(t *testing.T) {
	const canary = "same-origin-custom-canary"
	var targetHeaders http.Header
	var source *httptest.Server
	source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, source.URL+"/target", http.StatusFound)
		case "/target":
			targetHeaders = r.Header.Clone()
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(source.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, source.URL+"/start", map[string]string{
		"Authorization":  "Bearer same-origin-auth",
		"X-Source-Token": canary,
	}, source.Client())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := targetHeaders.Get("X-Source-Token"); got != canary {
		t.Errorf("same-origin custom header = %q, want %q", got, canary)
	}
	if got := targetHeaders.Get("Authorization"); got != "Bearer same-origin-auth" {
		t.Errorf("same-origin Authorization = %q", got)
	}
}

func TestLegacyProxyRejectsHTTPSDowngradeRedirect(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/plaintext", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, source.URL+"/start", map[string]string{
		"Authorization": "Bearer must-not-leak",
	}, source.Client())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if targetHit {
		t.Fatal("HTTPS-to-HTTP redirect reached the plaintext target")
	}
}

func TestLegacyProxyForcesIdentityAndRejectsEncodedMedia(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPartialContent} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			requestEncoding := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestEncoding = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", "4")
				if status == http.StatusPartialContent {
					w.Header().Set("Content-Range", "bytes 0-3/1000")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "gzip")
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=0-3")
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/encoded", map[string]string{
				"Accept-Encoding": "gzip, br",
			}, upstream.Client())

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if requestEncoding != "identity" {
				t.Errorf("upstream Accept-Encoding = %q, want identity", requestEncoding)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("encoded response exposed Content-Encoding = %q", got)
			}
		})
	}
}

func TestLegacyProxyIfRangeMatchAndMismatch(t *testing.T) {
	const validator = `"media-version-1"`
	body := strings.Repeat("z", 1000)

	tests := []struct {
		name        string
		request     string
		wantStatus  int
		wantIfRange string
		wantBody    string
	}{
		{
			name:        "matching validator",
			request:     validator,
			wantStatus:  http.StatusPartialContent,
			wantIfRange: validator,
			wantBody:    body[100:200],
		},
		{
			name:        "mismatched validator",
			request:     `"old-version"`,
			wantStatus:  http.StatusOK,
			wantIfRange: `"old-version"`,
			wantBody:    body,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seenIfRange string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenIfRange = r.Header.Get("If-Range")
				w.Header().Set("Content-Type", "video/mp4")
				if r.Header.Get("If-Range") == validator {
					w.Header().Set("Content-Range", "bytes 100-199/1000")
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, body[100:200])
					return
				}
				w.Header().Set("Content-Length", "1000")
				w.Header().Set("Content-Range", "bytes 100-199/1000")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=100-199")
			req.Header.Set("If-Range", tc.request)
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if seenIfRange != tc.wantIfRange {
				t.Errorf("upstream If-Range = %q, want %q", seenIfRange, tc.wantIfRange)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body length = %d, want %d", len(got), len(tc.wantBody))
			}
			if tc.wantStatus == http.StatusOK {
				if got := rec.Header().Get("Content-Range"); got != "" {
					t.Errorf("mismatched If-Range fabricated Content-Range = %q", got)
				}
			}
		})
	}
}

func TestLegacyProxyRejectsPartialWithConflictingIfRangeValidator(t *testing.T) {
	tests := []struct {
		name         string
		ifRange      string
		etag         string
		lastModified string
	}{
		{
			name:    "entity tag",
			ifRange: `"v1"`,
			etag:    `"v2"`,
		},
		{
			name:         "modification time",
			ifRange:      "Mon, 26 Oct 2026 10:00:00 GMT",
			lastModified: "Sun, 25 Oct 2026 10:00:00 GMT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 100-199/1000")
				w.Header().Set("Content-Length", "100")
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				if tc.lastModified != "" {
					w.Header().Set("Last-Modified", tc.lastModified)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, strings.Repeat("x", 100))
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=100-199")
			req.Header.Set("If-Range", tc.ifRange)
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q",
					rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Range"); got != "" {
				t.Errorf("conflicting validator leaked Content-Range %q", got)
			}
			if got := rec.Header().Get("ETag"); got != "" {
				t.Errorf("conflicting validator leaked ETag %q", got)
			}
		})
	}
}

func TestLegacyProxyDoesNotForwardIfRangeWithoutRange(t *testing.T) {
	var seenIfRange string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenIfRange = r.Header.Get("If-Range")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("If-Range", `"client-validator"`)
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", map[string]string{
		"If-Range": `"source-validator"`,
	}, upstream.Client())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if seenIfRange != "" {
		t.Errorf("upstream If-Range = %q, want no header without Range", seenIfRange)
	}
}

func TestLegacyProxyRejectsInconsistentPartialResponses(t *testing.T) {
	tests := []struct {
		name         string
		contentRange string
		length       string
	}{
		{name: "wrong start", contentRange: "bytes 99-198/1000", length: "100"},
		{name: "past requested end", contentRange: "bytes 100-200/1000", length: "101"},
		{name: "end past total", contentRange: "bytes 100-199/199", length: "100"},
		{name: "wrong content length", contentRange: "bytes 100-199/1000", length: "99"},
		{name: "unknown total", contentRange: "bytes 100-199/*", length: "100"},
		{name: "signed start", contentRange: "bytes +100-199/1000", length: "100"},
		{name: "signed end", contentRange: "bytes 100-+199/1000", length: "100"},
		{name: "space before dash", contentRange: "bytes 100 -199/1000", length: "100"},
		{name: "space after dash", contentRange: "bytes 100- 199/1000", length: "100"},
		{name: "space before slash", contentRange: "bytes 100-199 /1000", length: "100"},
		{name: "space after slash", contentRange: "bytes 100-199/ 1000", length: "100"},
		{name: "extra space after unit", contentRange: "bytes  100-199/1000", length: "100"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: legacyProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusPartialContent,
					Status:     "206 Partial Content",
					Header: http.Header{
						"Content-Type":   {"video/mp4"},
						"Content-Range":  {tc.contentRange},
						"Content-Length": {tc.length},
					},
					Body:    io.NopCloser(strings.NewReader(strings.Repeat("x", 100))),
					Request: req,
				}, nil
			})}
			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=100-199")
			rec := httptest.NewRecorder()

			proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Range"); got != "" {
				t.Errorf("invalid upstream Content-Range leaked as %q", got)
			}
		})
	}
}

func TestLegacyProxyRejectsInvalidContentLength(t *testing.T) {
	for _, value := range []string{
		"+4",
		"-0",
		"four",
		"",
		"9223372036854775808",
	} {
		t.Run(value, func(t *testing.T) {
			client := &http.Client{Transport: legacyProxyRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header: http.Header{
							"Content-Type":   {"video/mp4"},
							"Content-Length": {value},
						},
						Body:          io.NopCloser(strings.NewReader("media")),
						ContentLength: -1,
						Request:       req,
					}, nil
				})}
			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			rec := httptest.NewRecorder()

			proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Errorf("invalid upstream Content-Length leaked as %q", got)
			}
		})
	}
}

func TestLegacyProxyRejectsInvalidRangeSyntax(t *testing.T) {
	for _, value := range []string{
		"bytes=+100-199",
		"bytes=100-+199",
		"bytes= 100-199",
		"bytes=100 -199",
		"bytes=100- 199",
		"bytes=\t100-199",
	} {
		t.Run(value, func(t *testing.T) {
			called := false
			client := &http.Client{Transport: legacyProxyRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					called = true
					return nil, fmt.Errorf("must not be called")
				})}
			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", value)
			rec := httptest.NewRecorder()

			proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			if called {
				t.Error("invalid Range reached the upstream")
			}
		})
	}
}

func TestLegacyProxyRejectsMalformedUnsatisfiableContentRange(t *testing.T) {
	for _, value := range []string{
		"bytes */+1000",
		"bytes */ 1000",
		"bytes  */1000",
		"bytes\t*/1000",
		"bytes */1000x",
		"bytes 0-0/1000",
	} {
		t.Run(value, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: legacyProxyRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					requests++
					return &http.Response{
						StatusCode: http.StatusRequestedRangeNotSatisfiable,
						Status:     "416 Range Not Satisfiable",
						Header: http.Header{
							"Content-Type":  {"text/plain"},
							"Content-Range": {value},
						},
						Body:    io.NopCloser(strings.NewReader("private upstream body")),
						Request: req,
					}, nil
				})}
			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=1000-")
			rec := httptest.NewRecorder()

			proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if requests != 1 {
				t.Errorf("upstream requests = %d, want no fallback probe", requests)
			}
			if got := rec.Header().Get("Content-Range"); got != "" {
				t.Errorf("malformed upstream Content-Range leaked as %q", got)
			}
			if strings.Contains(rec.Body.String(), "private upstream body") {
				t.Errorf("upstream body leaked: %q", rec.Body.String())
			}
		})
	}
}

func TestLegacyProxyUnsatisfiableIgnoredRange(t *testing.T) {
	const total = 1000
	body := strings.Repeat("q", total)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=2000-2099")
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */1000" {
		t.Errorf("Content-Range = %q, want bytes */1000", got)
	}
}

func TestLegacyProxyPreservesUnsatisfiableHeadRange(t *testing.T) {
	var probeGets int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			probeGets++
		}
		w.Header().Set("Content-Range", "bytes */100")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodHead, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=200-")
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */100" {
		t.Errorf("Content-Range = %q, want bytes */100", got)
	}
	if probeGets != 0 {
		t.Fatalf("HEAD 416 triggered %d fallback GET(s), want 0", probeGets)
	}
}

func TestLegacyProxyRecoversUnsatisfiableRangeFromProbe(t *testing.T) {
	var probeGets int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		probeGets++
		got := r.Header.Get("Range")
		if got == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/100")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "x")
			return
		}
		t.Errorf("unexpected probe Range = %q", got)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodHead, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=200-")
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */100" {
		t.Errorf("Content-Range = %q, want bytes */100", got)
	}
	if probeGets != 1 {
		t.Fatalf("probe GETs = %d, want 1", probeGets)
	}
}

func TestLegacyProxyRejectsUnsatisfiableRangeWithConflictingIfRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Content-Range", "bytes */1000")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=2000-")
	req.Header.Set("If-Range", `"v1"`)
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("conflicting 416 leaked Content-Range %q", got)
	}
}

func TestLegacyProxySynthesisesSuffixRange(t *testing.T) {
	const total = 1000
	body := strings.Repeat("0123456789", 100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=-100" {
			t.Errorf("upstream Range = %q, want bytes=-100", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=-100")
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body[900:] {
		t.Errorf("body length = %d, want suffix bytes", len(got))
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 900-999/1000" {
		t.Errorf("Content-Range = %q, want bytes 900-999/1000", got)
	}
}

func TestLegacyProxyRejectsMultipartAndRepeatedRanges(t *testing.T) {
	t.Run("multipart", func(t *testing.T) {
		called := false
		client := &http.Client{Transport: legacyProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("unexpected upstream request")
		})}
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		req.Header.Set("Range", "bytes=0-1,3-4")
		rec := httptest.NewRecorder()

		proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
		if called {
			t.Error("multipart Range reached the upstream")
		}
	})

	t.Run("repeated", func(t *testing.T) {
		called := false
		client := &http.Client{Transport: legacyProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("unexpected upstream request")
		})}
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		req.Header.Add("Range", "bytes=0-1")
		req.Header.Add("Range", "bytes=3-4")
		rec := httptest.NewRecorder()

		proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
		if called {
			t.Error("repeated Range reached the upstream")
		}
	})
}

func TestLegacyProxySanitizesActiveContentTypes(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		contentType  string
		requestRange string
		contentRange string
		body         string
	}{
		{
			name:        "html 200",
			status:      http.StatusOK,
			contentType: "text/html; charset=utf-8",
			body:        "<html><script>alert(1)</script></html>",
		},
		{
			name:         "svg 206",
			status:       http.StatusPartialContent,
			contentType:  "image/svg+xml",
			requestRange: "bytes=0-3",
			contentRange: "bytes 0-3/4",
			body:         "<svg",
		},
		{
			name:        "xhtml",
			status:      http.StatusOK,
			contentType: "application/xhtml+xml",
			body:        "<html></html>",
		},
		{
			name:        "javascript",
			status:      http.StatusOK,
			contentType: "application/javascript",
			body:        "alert(1)",
		},
		{
			name:        "text javascript",
			status:      http.StatusOK,
			contentType: "text/javascript",
			body:        "alert(1)",
		},
		{
			name:        "xml",
			status:      http.StatusOK,
			contentType: "application/xml",
			body:        "<root/>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			if tc.requestRange != "" {
				req.Header.Set("Range", tc.requestRange)
			}
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream", got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestLegacyProxyPreservesSafeMediaContentTypes(t *testing.T) {
	tests := []struct {
		name string
		sent string
		want string
	}{
		{name: "video", sent: "video/mp4", want: "video/mp4"},
		{name: "audio", sent: "audio/mpeg", want: "audio/mpeg"},
		{name: "byte stream", sent: "application/octet-stream", want: "application/octet-stream"},
		{name: "webvtt", sent: "text/vtt; charset=utf-8", want: "text/vtt"},
		{name: "hls", sent: "application/vnd.apple.mpegurl", want: "application/vnd.apple.mpegurl"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.sent)
				w.Header().Set("Content-Length", "4")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data")
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/media", nil, upstream.Client())

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestLegacyProxyHidesUpstreamErrorBodies(t *testing.T) {
	const tokenCanary = "upstream-error-token-f6c1"

	for _, status := range []int{
		http.StatusNotFound,
		http.StatusRequestedRangeNotSatisfiable,
		http.StatusInternalServerError,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("X-Upstream-Secret", tokenCanary)
				if status == http.StatusRequestedRangeNotSatisfiable {
					w.Header().Set("Content-Range", "bytes */4")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "<html>source error token="+tokenCanary+"</html>")
			}))
			t.Cleanup(upstream.Close)

			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			req.Header.Set("Range", "bytes=0-3")
			if status == http.StatusRequestedRangeNotSatisfiable {
				req.Header.Set("Range", "bytes=4-7")
			}
			rec := httptest.NewRecorder()
			proxyRemoteVia(rec, req, upstream.URL+"/missing?token="+tokenCanary, nil, upstream.Client())

			wantStatus := http.StatusBadGateway
			if status == http.StatusRequestedRangeNotSatisfiable {
				wantStatus = http.StatusRequestedRangeNotSatisfiable
			}
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d; body=%q",
					rec.Code, wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), tokenCanary) {
				t.Errorf("source error body leaked token: %q", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "<html>") {
				t.Errorf("source error page was relayed: %q", rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want sanitized JSON error", got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := rec.Header().Get("X-Upstream-Secret"); got != "" {
				t.Errorf("upstream header leaked as %q", got)
			}
			if status == http.StatusRequestedRangeNotSatisfiable {
				if got := rec.Header().Get("Content-Range"); got != "bytes */4" {
					t.Errorf("Content-Range = %q, want bytes */4", got)
				}
			}
		})
	}
}

func TestLegacyProxyAbortsShortBodyCopies(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		headers   http.Header
		body      string
		rangeHdr  string
		wantPanic bool
	}{
		{
			name:   "partial",
			status: http.StatusPartialContent,
			headers: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Range":  {"bytes 0-99/1000"},
				"Content-Length": {"100"},
			},
			body:     strings.Repeat("x", 50),
			rangeHdr: "bytes=0-99",
		},
		{
			name:   "full",
			status: http.StatusOK,
			headers: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Length": {"100"},
			},
			body: strings.Repeat("x", 50),
		},
		{
			name:   "sliced",
			status: http.StatusOK,
			headers: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Length": {"1000"},
			},
			body:     strings.Repeat("x", 100),
			rangeHdr: "bytes=50-149",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: legacyProxyRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					declared, _ := strconv.ParseInt(
						tc.headers.Get("Content-Length"), 10, 64)
					return &http.Response{
						StatusCode:    tc.status,
						Status:        strconv.Itoa(tc.status),
						Header:        tc.headers.Clone(),
						Body:          io.NopCloser(strings.NewReader(tc.body)),
						ContentLength: declared,
						Request:       req,
					}, nil
				})}
			req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
			if tc.rangeHdr != "" {
				req.Header.Set("Range", tc.rangeHdr)
			}
			rec := httptest.NewRecorder()

			var recovered any
			func() {
				defer func() { recovered = recover() }()
				proxyRemoteVia(rec, req, "https://media.example.test/video.mp4", nil, client)
			}()
			if recovered != http.ErrAbortHandler {
				t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
			}
		})
	}
}
