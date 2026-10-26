package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type playbackProxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f playbackProxyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func runPlaybackProxy(t *testing.T, method, target string, headers map[string]string, client *http.Client, requestHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/playback/media-sessions/ms_test/stream", nil)
	for name, value := range requestHeaders {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	serveV1ProxyMedia(rec, req, target, headers, client)
	return rec
}

func playbackProxyErrorCode(t *testing.T, rec *httptest.ResponseRecorder) V1ErrorCode {
	t.Helper()
	var envelope v1ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode v1 error: %v; body=%q", err, rec.Body.String())
	}
	return envelope.Error.Code
}

func TestPlaybackProxyRejectsUnsupportedMethods(t *testing.T) {
	called := false
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})}
	rec := runPlaybackProxy(t, http.MethodPost, "https://media.example.test/video.mp4", nil, client, nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
		t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
	}
	if called {
		t.Error("unsupported method reached the upstream")
	}
}

func TestPlaybackProxyFullGetAndHeadSanitizesHeaders(t *testing.T) {
	body := strings.Repeat("full-response-", 32)
	const lastModified = "Mon, 26 Oct 2026 10:00:00 GMT"
	const etag = `"source-etag"`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer source-secret" {
			t.Errorf("upstream Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Last-Modified", lastModified)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=999")
		w.Header().Set("Set-Cookie", "source_session=must-not-leak")
		w.Header().Set("Location", "https://private.example.test/redirect?token=must-not-leak")
		w.Header().Set("Authorization", "Bearer must-not-leak")
		w.Header().Set("WWW-Authenticate", `Basic realm="must-not-leak"`)
		w.Header().Set("X-Source-Secret", "must-not-leak")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(upstream.Close)

	target := upstream.URL + "/private/video.mp4?source_token=must-not-leak"
	sourceHeaders := map[string]string{"Authorization": "Bearer source-secret"}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := runPlaybackProxy(t, method, target, sourceHeaders, upstream.Client(), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
				}
			} else if got := rec.Body.String(); got != body {
				t.Errorf("body length = %d, want %d", len(got), len(body))
			}
			assertPlaybackProxyHeader(t, rec, "Content-Type", "video/mp4")
			assertPlaybackProxyHeader(t, rec, "Content-Length", strconv.Itoa(len(body)))
			assertPlaybackProxyHeader(t, rec, "Accept-Ranges", "bytes")
			assertPlaybackProxyHeader(t, rec, "Last-Modified", lastModified)
			assertPlaybackProxyHeader(t, rec, "ETag", etag)
			assertPlaybackProxyHeader(t, rec, "Cache-Control", v1ProxyCacheControl)
			for _, name := range []string{"Set-Cookie", "Location", "Authorization", "WWW-Authenticate", "X-Source-Secret"} {
				if got := rec.Header().Values(name); len(got) != 0 {
					t.Errorf("unsafe response header %s leaked: %q", name, got)
				}
			}
			if strings.Contains(rec.Body.String(), "must-not-leak") {
				t.Errorf("client response leaked a source secret: %q", rec.Body.String())
			}
		})
	}

}

func TestPlaybackProxyForcesSafeContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<h1>")
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/not-media", nil, upstream.Client(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertPlaybackProxyHeader(t, rec, "Content-Type", "application/octet-stream")
	assertPlaybackProxyHeader(t, rec, "X-Content-Type-Options", "nosniff")
}

func TestPlaybackProxySingleRangeGetAndHead(t *testing.T) {
	body := strings.Repeat("0123456789", 100)
	modTime := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=100-199" {
			t.Errorf("upstream Range = %q, want bytes=100-199", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", `"range-etag"`)
		w.Header().Set("Set-Cookie", "must-not-leak=1")
		http.ServeContent(w, r, "clip.mp4", modTime, strings.NewReader(body))
	}))
	t.Cleanup(upstream.Close)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := runPlaybackProxy(t, method, upstream.URL+"/clip.mp4?signature=source-secret", nil, upstream.Client(), map[string]string{
				"Range": "bytes=100-199",
			})
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
			}
			assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 100-199/1000")
			assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
			assertPlaybackProxyHeader(t, rec, "Accept-Ranges", "bytes")
			assertPlaybackProxyHeader(t, rec, "Cache-Control", v1ProxyCacheControl)
			assertPlaybackProxyHeader(t, rec, "ETag", `"range-etag"`)
			if method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
				}
			} else if got := rec.Body.String(); got != body[100:200] {
				t.Errorf("body = %q, want requested range", got)
			}
			if got := rec.Header().Get("Set-Cookie"); got != "" {
				t.Errorf("Set-Cookie leaked: %q", got)
			}
		})
	}
}

func TestPlaybackProxyUnsatisfiableRange(t *testing.T) {
	body := strings.Repeat("x", 2048)
	modTime := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "clip.mp4", modTime, strings.NewReader(body))
	}))
	t.Cleanup(upstream.Close)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := runPlaybackProxy(t, method, upstream.URL+"/clip.mp4", nil, upstream.Client(), map[string]string{
				"Range": "bytes=2048-",
			})
			if rec.Code != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d, want 416; body=%q", rec.Code, rec.Body.String())
			}
			assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes */2048")
			assertPlaybackProxyHeader(t, rec, "Cache-Control", v1ProxyErrorCacheControl)
			if method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
				}
			} else if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
				t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
			}
		})
	}
}

func TestPlaybackProxyOpenEndedRange(t *testing.T) {
	body := strings.Repeat("r", 500)
	modTime := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	honest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "clip.mp4", modTime, strings.NewReader(body))
	}))
	t.Cleanup(honest.Close)
	ignoresRange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ignoresRange.Close)

	for name, upstream := range map[string]*httptest.Server{
		"honest":        honest,
		"ignores-range": ignoresRange,
	} {
		t.Run(name, func(t *testing.T) {
			rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil, upstream.Client(), map[string]string{
				"Range": "bytes=400-",
			})
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
			}
			assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 400-499/500")
			assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
			if got := rec.Body.String(); got != body[400:] {
				t.Errorf("body = %q, want the final 100 bytes", got)
			}
		})
	}
}

func TestPlaybackProxyRejectsBadUpstreamContentRange(t *testing.T) {
	for _, contentRange := range []string{
		"bytes nonsense",
		"bytes 0-99/*",
		"bytes 0-99/50",
	} {
		t.Run(contentRange, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", contentRange)
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Set-Cookie", "must-not-leak=1")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "junk")
			}))
			t.Cleanup(upstream.Close)

			rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil, upstream.Client(), map[string]string{
				"Range": "bytes=0-3",
			})
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
				t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
			}
			if strings.Contains(rec.Body.String(), "junk") {
				t.Errorf("upstream body leaked: %q", rec.Body.String())
			}
			if got := rec.Header().Get("Set-Cookie"); got != "" {
				t.Errorf("Set-Cookie leaked: %q", got)
			}
		})
	}
}

func TestPlaybackProxyRejectsEncodedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "junk")
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil, upstream.Client(), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding leaked: %q", got)
	}
}

func TestPlaybackProxySlicesRangeIgnoredByUpstream(t *testing.T) {
	body := strings.Repeat("abcdefghij", 100)
	const lastModified = "Mon, 26 Oct 2026 10:00:00 GMT"
	const etag = `"ignored-range-etag"`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=250-349" {
			t.Errorf("upstream Range = %q, want bytes=250-349", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Last-Modified", lastModified)
		w.Header().Set("ETag", etag)
		w.Header().Set("Set-Cookie", "must-not-leak=1")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(upstream.Close)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := runPlaybackProxy(t, method, upstream.URL+"/clip.mp4?source=secret", nil, upstream.Client(), map[string]string{
				"Range": "bytes=250-349",
			})
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
			}
			assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 250-349/1000")
			assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
			assertPlaybackProxyHeader(t, rec, "Accept-Ranges", "bytes")
			assertPlaybackProxyHeader(t, rec, "Cache-Control", v1ProxyCacheControl)
			assertPlaybackProxyHeader(t, rec, "Content-Type", "video/mp4")
			assertPlaybackProxyHeader(t, rec, "X-Content-Type-Options", "nosniff")
			assertPlaybackProxyHeader(t, rec, "Last-Modified", lastModified)
			assertPlaybackProxyHeader(t, rec, "ETag", etag)
			if method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
				}
			} else if got := rec.Body.String(); got != body[250:350] {
				t.Errorf("body = %q, want sliced range", got)
			}
			if got := rec.Header().Get("Set-Cookie"); got != "" {
				t.Errorf("Set-Cookie leaked: %q", got)
			}
		})
	}

	t.Run("if-range mismatch is not synthesized", func(t *testing.T) {
		rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4?source=secret", nil, upstream.Client(), map[string]string{
			"Range":    "bytes=250-349",
			"If-Range": `"different-validator"`,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != body {
			t.Errorf("body length = %d, want full representation %d", len(got), len(body))
		}
	})
}

func TestPlaybackProxyRejectsUnsliceableIgnoredRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, strings.Repeat("z", 4096))
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil, upstream.Client(), map[string]string{
		"Range": "bytes=0-99",
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
}

func TestPlaybackProxyHeadFallbackUsesRangeProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Set-Cookie", "must-not-leak=1")
			http.Error(w, "HEAD not supported", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Errorf("probe Range = %q, want bytes=0-0", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/500")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodHead, upstream.URL+"/clip.mp4", nil, upstream.Client(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertPlaybackProxyHeader(t, rec, "Content-Length", "500")
	assertPlaybackProxyHeader(t, rec, "Accept-Ranges", "bytes")
	assertPlaybackProxyHeader(t, rec, "Cache-Control", v1ProxyCacheControl)
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie leaked: %q", got)
	}
}

func TestPlaybackProxyHeadProbeFailureMapsToSourceUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := "upstream failure body"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusInternalServerError)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodHead, upstream.URL+"/clip.mp4", nil, upstream.Client(), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

func TestPlaybackProxyErrorDropsSourceMediaHeaders(t *testing.T) {
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Length": {"1000"},
				"Last-Modified":  {"Mon, 26 Oct 2026 10:00:00 GMT"},
				"ETag":           {`"must-not-leak"`},
			},
			Body:          io.NopCloser(strings.NewReader("short")),
			ContentLength: 1000,
			Request:       req,
		}, nil
	})}
	rec := runPlaybackProxy(t, http.MethodGet, "https://media.example.test/clip.mp4", nil, client, map[string]string{
		"Range": "bytes=900-999",
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
	for _, name := range []string{"Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("source media header %s leaked into an error: %q", name, got)
		}
	}
	if strings.Contains(rec.Body.String(), "short") || strings.Contains(rec.Body.String(), "must-not-leak") {
		t.Errorf("upstream bytes leaked into an error: %q", rec.Body.String())
	}
}

func TestPlaybackProxyRejectsInvalidClientRange(t *testing.T) {
	called := false
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})}
	rec := runPlaybackProxy(t, http.MethodGet, "https://media.example.test/video.mp4", nil, client, map[string]string{
		"Range": "bytes=200-100",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
		t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
	}
	if called {
		t.Error("invalid range reached the upstream")
	}
}

func TestPlaybackProxyMapsAndRedactsUpstreamFailure(t *testing.T) {
	var logs bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	const querySecret = "must-not-log-query-secret"
	const headerSecret = "must-not-log-header-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session="+headerSecret)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "upstream body "+querySecret)
	}))
	t.Cleanup(upstream.Close)

	target := upstream.URL + "/private/movie.mp4?token=" + querySecret
	rec := runPlaybackProxy(t, http.MethodGet, target, map[string]string{
		"Authorization": "Bearer " + headerSecret,
	}, upstream.Client(), nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
	if strings.Contains(rec.Body.String(), querySecret) || strings.Contains(rec.Body.String(), headerSecret) {
		t.Errorf("response leaked a source secret: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie leaked: %q", got)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	gotLogs := logs.String()
	if !strings.Contains(gotLogs, "host="+parsed.Hostname()) {
		t.Errorf("logs do not contain the sanitized host: %q", gotLogs)
	}
	for _, secret := range []string{querySecret, headerSecret, "/private/movie.mp4", "?token="} {
		if strings.Contains(gotLogs, secret) {
			t.Errorf("logs leaked %q: %q", secret, gotLogs)
		}
	}
}

func TestPlaybackProxyTransportErrorIsRedacted(t *testing.T) {
	var logs bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	const secret = "transport-error-secret"
	target := "https://media.example.test/private/video.mp4?token=" + secret
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network failure at " + req.URL.String())
	})}
	rec := runPlaybackProxy(t, http.MethodGet, target, nil, client, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "/private/") {
		t.Errorf("transport logs leaked source details: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "host=media.example.test") {
		t.Errorf("transport logs omit sanitized host: %q", logs.String())
	}
}

func TestPlaybackProxyInvalidTargetIsInternalError(t *testing.T) {
	rec := runPlaybackProxy(t, http.MethodGet, "://not-a-url", nil, nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorInternal {
		t.Errorf("error code = %q, want %q", got, V1ErrorInternal)
	}
	if strings.Contains(rec.Body.String(), "not-a-url") {
		t.Errorf("internal error leaked the target: %q", rec.Body.String())
	}
}

func TestPlaybackProxyRejectsRepeatedUpstreamContentRange(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "partial", status: http.StatusPartialContent},
		{name: "unsatisfiable", status: http.StatusRequestedRangeNotSatisfiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				if tc.status == http.StatusPartialContent {
					w.Header().Add("Content-Range", "bytes 2-4/10")
					w.Header().Add("Content-Range", "bytes 2-4/10")
					w.Header().Set("Content-Length", "3")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "234")
					return
				}
				w.Header().Add("Content-Range", "bytes */10")
				w.Header().Add("Content-Range", "bytes */10")
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(upstream.Close)

			rangeValue := "bytes=2-4"
			if tc.status == http.StatusRequestedRangeNotSatisfiable {
				rangeValue = "bytes=10-"
			}
			rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil,
				upstream.Client(), map[string]string{"Range": rangeValue})
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
				t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
			}
		})
	}
}

func TestPlaybackProxyRejectsInvalidUpstreamContentLength(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header []string
	}{
		{
			name: "full repeated", status: http.StatusOK,
			header: []string{"4", "4"},
		},
		{
			name: "full conflicting", status: http.StatusOK,
			header: []string{"4", "5"},
		},
		{
			name: "partial repeated", status: http.StatusPartialContent,
			header: []string{"3", "3"},
		},
		{
			name: "partial malformed", status: http.StatusPartialContent,
			header: []string{"three"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				header := make(http.Header)
				for _, value := range tc.header {
					header.Add("Content-Length", value)
				}
				header.Set("Content-Type", "video/mp4")
				if tc.status == http.StatusPartialContent {
					header.Set("Content-Range", "bytes 2-4/10")
				}
				return &http.Response{
					StatusCode: tc.status,
					Status:     strconv.Itoa(tc.status),
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("media")),
					Request:    req,
				}, nil
			})}
			requestHeaders := map[string]string{}
			if tc.status == http.StatusPartialContent {
				requestHeaders["Range"] = "bytes=2-4"
			}
			rec := runPlaybackProxy(t, http.MethodGet,
				"https://media.example.test/clip.mp4", nil, client, requestHeaders)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
				t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Errorf("invalid upstream Content-Length leaked as %q", got)
			}
		})
	}
}

func TestPlaybackProxyRejectsShortPartialRange(t *testing.T) {
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Status:     "206 Partial Content",
			Header: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Range":  {"bytes 2-3/10"},
				"Content-Length": {"2"},
			},
			Body:    io.NopCloser(strings.NewReader("23")),
			Request: req,
		}, nil
	})}

	rec := runPlaybackProxy(t, http.MethodGet, "https://media.example.test/clip.mp4",
		nil, client, map[string]string{"Range": "bytes=2-4"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
}

func TestPlaybackProxyPartialIfRangeValidation(t *testing.T) {
	const (
		etag         = `"current-etag"`
		lastModified = "Mon, 26 Oct 2026 10:00:00 GMT"
	)
	tests := []struct {
		name             string
		ifRange          string
		responseETag     []string
		responseModified []string
		wantStatus       int
	}{
		{
			name: "matching entity tag", ifRange: etag,
			responseETag: []string{etag}, wantStatus: http.StatusPartialContent,
		},
		{
			name: "conflicting entity tag", ifRange: etag,
			responseETag: []string{`"stale-etag"`}, wantStatus: http.StatusBadGateway,
		},
		{
			name: "repeated entity tags", ifRange: etag,
			responseETag: []string{etag, etag}, wantStatus: http.StatusBadGateway,
		},
		{
			name: "missing entity tag trusts source", ifRange: etag,
			wantStatus: http.StatusPartialContent,
		},
		{
			name: "matching modification date", ifRange: lastModified,
			responseModified: []string{lastModified}, wantStatus: http.StatusPartialContent,
		},
		{
			name: "conflicting modification date", ifRange: lastModified,
			responseModified: []string{"Sun, 25 Oct 2026 10:00:00 GMT"},
			wantStatus:       http.StatusBadGateway,
		},
		{
			name: "missing modification date trusts source", ifRange: lastModified,
			wantStatus: http.StatusPartialContent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Range"); got != "bytes=2-4" {
					t.Errorf("upstream Range = %q, want bytes=2-4", got)
				}
				if got := r.Header.Get("If-Range"); got != tc.ifRange {
					t.Errorf("upstream If-Range = %q, want %q", got, tc.ifRange)
				}
				for _, value := range tc.responseETag {
					w.Header().Add("ETag", value)
				}
				for _, value := range tc.responseModified {
					w.Header().Add("Last-Modified", value)
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 2-4/10")
				w.Header().Set("Content-Length", "3")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "234")
			}))
			t.Cleanup(upstream.Close)

			rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil,
				upstream.Client(), map[string]string{
					"Range": "bytes=2-4", "If-Range": tc.ifRange,
				})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q",
					rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusPartialContent {
				if got := rec.Body.String(); got != "234" {
					t.Errorf("body = %q, want ranged bytes", got)
				}
				return
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
				t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
			}
		})
	}
}

func TestPlaybackProxyUnsatisfiableRangeRejectsConflictingIfRange(t *testing.T) {
	var probes int
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		probes++
		header := http.Header{
			"Content-Type":  {"application/json"},
			"Content-Range": {"bytes */1000"},
		}
		header.Set("ETag", `"v2"`)
		return &http.Response{
			StatusCode: http.StatusRequestedRangeNotSatisfiable,
			Status:     "416 Range Not Satisfiable",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"error":"private"}`)),
			Request:    req,
		}, nil
	})}

	rec := runPlaybackProxy(t, http.MethodGet, "https://media.example.test/clip.mp4",
		nil, client, map[string]string{
			"Range": "bytes=2000-", "If-Range": `"v1"`,
		})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorSourceUnavailable {
		t.Errorf("error code = %q, want %q", got, V1ErrorSourceUnavailable)
	}
	if probes != 1 {
		t.Fatalf("upstream requests = %d, want 1", probes)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("conflicting 416 leaked Content-Range %q", got)
	}
}

func TestPlaybackProxyHeadProbeDoesNotInventConditionalPartial(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Errorf("probe Range = %q, want bytes=0-0", got)
		}
		if got := r.Header.Get("If-Range"); got != "" {
			t.Errorf("probe If-Range = %q, want empty", got)
		}
		w.Header().Set("Content-Range", "bytes 0-0/500")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodHead, upstream.URL+"/clip.mp4", nil,
		upstream.Client(), map[string]string{
			"Range": "bytes=2-4", "If-Range": `"client-etag"`,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertPlaybackProxyHeader(t, rec, "Content-Length", "500")
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range = %q, want absent for the full representation", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

func TestPlaybackProxyDoesNotForwardIfRangeWithoutRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-Range"); got != "" {
			t.Errorf("upstream If-Range = %q, want absent", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "full")
	}))
	t.Cleanup(upstream.Close)

	rec := runPlaybackProxy(t, http.MethodGet, upstream.URL+"/clip.mp4", nil,
		upstream.Client(), map[string]string{"If-Range": `"client-etag"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestPlaybackProxyRejectsRepeatedIfRange(t *testing.T) {
	called := false
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/ms_test/stream", nil)
	req.Header.Set("Range", "bytes=0-1")
	req.Header.Add("If-Range", `"one"`)
	req.Header.Add("If-Range", `"two"`)
	rec := httptest.NewRecorder()
	serveV1ProxyMedia(rec, req, "https://media.example.test/video.mp4", nil, client)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
		t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
	}
	if called {
		t.Error("repeated If-Range reached the upstream")
	}
}

func assertPlaybackProxyHeader(t *testing.T, rec *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rec.Header().Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}
