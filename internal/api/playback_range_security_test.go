package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPlaybackProxyRejectsUnsupportedV1RangeShapes(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "multiple ranges", value: "bytes=0-1,3-4"},
		{name: "suffix range", value: "bytes=-100"},
		{name: "empty range", value: ""},
		{name: "whitespace range", value: " "},
		{name: "wrong unit", value: "items=0-1"},
		{name: "backward range", value: "bytes=10-9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			client := &http.Client{Transport: playbackProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, io.ErrUnexpectedEOF
			})}
			rec := runPlaybackProxy(t, http.MethodGet, "https://media.example.test/video.mp4", nil, client,
				map[string]string{"Range": tc.value})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
				t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
			}
			if called {
				t.Error("invalid v1 range reached the upstream")
			}
		})
	}

	t.Run("repeated header", func(t *testing.T) {
		called := false
		client := &http.Client{Transport: playbackProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, io.ErrUnexpectedEOF
		})}
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/playback/media-sessions/ms_test/stream", nil)
		req.Header.Add("Range", "bytes=0-1")
		req.Header.Add("Range", "bytes=3-4")
		rec := httptest.NewRecorder()

		serveV1ProxyMedia(rec, req, "https://media.example.test/video.mp4", nil, client)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
		if called {
			t.Error("repeated v1 Range header reached the upstream")
		}
	})
}

func TestPlaybackProxyIgnoredRangeSanitizesActiveContentType(t *testing.T) {
	body := strings.Repeat("abcdefghij", 100)
	activeTypes := []string{
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/javascript",
		"text/plain",
	}
	for _, contentType := range activeTypes {
		t.Run(contentType, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = io.WriteString(w, body)
				}
			}))
			t.Cleanup(upstream.Close)

			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method, func(t *testing.T) {
					rec := runPlaybackProxy(t, method, upstream.URL+"/active", nil, upstream.Client(),
						map[string]string{"Range": "bytes=250-349"})
					if rec.Code != http.StatusPartialContent {
						t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
					}
					assertPlaybackProxyHeader(t, rec, "Content-Type", "application/octet-stream")
					assertPlaybackProxyHeader(t, rec, "X-Content-Type-Options", "nosniff")
					assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 250-349/1000")
					assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
					if method == http.MethodGet {
						if got := rec.Body.String(); got != body[250:350] {
							t.Errorf("body = %q, want requested range", got)
						}
					} else if rec.Body.Len() != 0 {
						t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
					}
				})
			}
		})
	}
}

func TestPlaybackProxyAbortsTruncatedIgnoredRange(t *testing.T) {
	const (
		queryCanary  = "truncated-query-canary-3f8a"
		headerCanary = "truncated-header-canary-91bd"
	)
	logs := capturePlaybackSecurityLogs(t)
	body := strings.Repeat("x", 950)
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Length": {"1000"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: req,
		}, nil
	})}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/ms_test/stream", nil)
	req.Header.Set("Range", "bytes=900-999")
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		serveV1ProxyMedia(rec, req,
			"https://media.example.test/private/clip.mp4?token="+queryCanary,
			map[string]string{"Authorization": "Bearer " + headerCanary}, client)
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
	assertPlaybackProxyHeader(t, rec, "Content-Type", "video/mp4")
	assertPlaybackProxyHeader(t, rec, "X-Content-Type-Options", "nosniff")
	if rec.Body.Len() != 50 {
		t.Errorf("truncated range body length = %d, want 50", rec.Body.Len())
	}

	gotLogs := logs.String()
	if !strings.Contains(gotLogs, "host=media.example.test") {
		t.Errorf("body failure log omits sanitized host: %q", gotLogs)
	}
	for _, secret := range []string{
		queryCanary, headerCanary, "/private/", "?token=", "Authorization",
	} {
		if strings.Contains(gotLogs, secret) {
			t.Errorf("body failure log leaked %q: %q", secret, gotLogs)
		}
	}
}

func TestPlaybackProxyAbortsTruncatedUpstreamPartial(t *testing.T) {
	body := strings.Repeat("y", 50)
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Status:     "206 Partial Content",
			Header: http.Header{
				"Content-Type":   {"video/mp4"},
				"Content-Length": {"100"},
				"Content-Range":  {"bytes 100-199/1000"},
			},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: 100,
			Request:       req,
		}, nil
	})}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/ms_test/stream", nil)
	req.Header.Set("Range", "bytes=100-199")
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		serveV1ProxyMedia(rec, req, "https://media.example.test/clip.mp4", nil, client)
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	assertPlaybackProxyHeader(t, rec, "Content-Length", "100")
	if rec.Body.Len() != len(body) {
		t.Errorf("truncated partial body length = %d, want %d", rec.Body.Len(), len(body))
	}
}

func TestProbeSizeByRangeRejectsEncodedUpstream(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "partial gzip", status: http.StatusPartialContent},
		{name: "full gzip", status: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Range"); got != "bytes=0-0" {
					t.Errorf("probe Range = %q, want bytes=0-0", got)
				}
				if got := r.Header.Get("Accept-Encoding"); got != "identity" {
					t.Errorf("probe Accept-Encoding = %q, want identity", got)
				}
				w.Header().Set("Content-Encoding", "gzip")
				if tc.status == http.StatusPartialContent {
					w.Header().Set("Content-Range", "bytes 0-0/500")
					w.Header().Set("Content-Length", "1")
				} else {
					w.Header().Set("Content-Length", "500")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "x")
			}))
			t.Cleanup(upstream.Close)

			if total, _, ok := probeSizeByRange(
				context.Background(), upstream.URL, nil, upstream.Client()); ok {
				t.Fatalf("encoded probe accepted total=%d", total)
			}
		})
	}
}

func TestLegacyProxyStillForwardsSuffixRange(t *testing.T) {
	body := strings.Repeat("0123456789", 100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=-100" {
			t.Errorf("legacy upstream Range = %q, want bytes=-100", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 900-999/1000")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body[900:])
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=-100")
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, upstream.URL, nil)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body[900:] {
		t.Errorf("body length = %d, want legacy suffix response", len(got))
	}
}
