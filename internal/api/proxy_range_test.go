package api

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type legacyProxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f legacyProxyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestParseRequestRange(t *testing.T) {
	cases := []struct {
		in    string
		start int64
		end   int64
		ok    bool
	}{
		{"bytes=100-199", 100, 199, true},
		{"bytes=100-", 100, -1, true},
		{"bytes=0-0", 0, 0, true},
		{"Bytes=5-10", 5, 10, true},
		{"bytes=100-199,300-399", 0, 0, false}, // multipart: not worth handling
		{"bytes=-500", 0, 0, false},            // suffix range
		{"bytes=", 0, 0, false},
		{"items=0-10", 0, 0, false},
		{"bytes=abc-def", 0, 0, false},
		{"bytes=200-100", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		got, ok := parseRequestRange(c.in)
		if ok != c.ok || (ok && (got.start != c.start || got.end != c.end)) {
			t.Errorf("parseRequestRange(%q) = (%+v,%v), want (%d,%d,%v)", c.in, got, ok, c.start, c.end, c.ok)
		}
	}
}

func TestParseContentRange(t *testing.T) {
	start, end, total, ok := parseContentRange("bytes 0-99/4122738")
	if !ok || start != 0 || end != 99 || total != 4122738 {
		t.Fatalf("got (%d,%d,%d,%v)", start, end, total, ok)
	}
	if _, _, total, ok := parseContentRange("bytes 100-199/*"); !ok || total != -1 {
		t.Errorf("an unknown total must parse as -1, got %d ok=%v", total, ok)
	}
	for _, bad := range []string{"", "bytes ", "bytes 1-", "bytes a-b/10", "bytes 5-4/10", "0-99/4122738", "bytes 0-99"} {
		if _, _, _, ok := parseContentRange(bad); ok {
			t.Errorf("parseContentRange(%q) should have failed", bad)
		}
	}
}

// upstream builds a server that serves a known body, with control over how it
// treats ranges — the whole point is that not all of them behave.
func upstream(t *testing.T, body string, mode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ignore-range":
			// The classic offender: a full 200 no matter what was asked for.
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		case "bad-content-range":
			w.Header().Set("Content-Range", "bytes nonsense")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "junk")
		case "no-head":
			if r.Method == http.MethodHead {
				http.Error(w, "no HEAD here", http.StatusMethodNotAllowed)
				return
			}
			if rng := r.Header.Get("Range"); rng == "bytes=0-0" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
				w.Header().Set("Content-Length", "1")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, body[:1])
				return
			}
			io.WriteString(w, body)
		default: // honours ranges, like any sane server
			http.ServeContent(w, r, "v.mp4", time.Time{}, strings.NewReader(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProxyPassesThroughHonestRange(t *testing.T) {
	body := strings.Repeat("0123456789", 100) // 1000 bytes
	srv := upstream(t, body, "honest")

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=100-199")
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, srv.URL, nil)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != body[100:200] {
		t.Fatalf("body = %q, want the requested 100 bytes", got)
	}
	if cr := rec.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 100-199/") {
		t.Errorf("Content-Range = %q", cr)
	}
}

// A source that ignores Range used to turn every seek into a full-file 200.
// The proxy has to answer the range itself.
func TestProxySynthesisesRangeWhenUpstreamIgnoresIt(t *testing.T) {
	body := strings.Repeat("abcdefghij", 100) // 1000 bytes
	srv := upstream(t, body, "ignore-range")

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=250-349")
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, srv.URL, nil)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (the client asked for a range)", rec.Code)
	}
	if got := rec.Body.String(); got != body[250:350] {
		t.Fatalf("body = %q, want bytes 250-349 of the file", got)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 250-349/1000" {
		t.Errorf("Content-Range = %q, want bytes 250-349/1000", cr)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "100" {
		t.Errorf("Content-Length = %q, want 100", cl)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
}

// An open-ended range has to be cut at the real end of the file.
func TestProxySynthesisesOpenEndedRange(t *testing.T) {
	body := strings.Repeat("x", 500)
	srv := upstream(t, body, "ignore-range")

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=400-")
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, srv.URL, nil)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := len(rec.Body.String()); got != 100 {
		t.Fatalf("body length = %d, want 100 (400..499)", got)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 400-499/500" {
		t.Errorf("Content-Range = %q", cr)
	}
}

// A 206 nobody can parse must not be forwarded: the bytes would belong to no
// declared range and the player would take them for whatever it asked for.
func TestProxyRejectsUnusablePartialContent(t *testing.T) {
	srv := upstream(t, "whatever", "bad-content-range")

	req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, srv.URL, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an unusable 206", rec.Code)
	}
}

// HEAD must still tell the player the size and that ranges work, even against an
// upstream that refuses HEAD.
func TestProxyHeadFallsBackToRangeProbe(t *testing.T) {
	body := strings.Repeat("y", 2048)
	srv := upstream(t, body, "no-head")

	req := httptest.NewRequest(http.MethodHead, "/api/stream/1?proxy=1", nil)
	rec := httptest.NewRecorder()
	proxyRemote(rec, req, srv.URL, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the fallback probe", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "2048" {
		t.Errorf("Content-Length = %q, want 2048", cl)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a HEAD must not carry a body, got %d bytes", rec.Body.Len())
	}
}

func TestLegacyProxyRequestFailuresDoNotLeakSensitiveURLs(t *testing.T) {
	const (
		queryCanary = "legacy-query-canary-7a31"
		authCanary  = "legacy-auth-canary-93c5"
	)

	t.Run("request construction", func(t *testing.T) {
		logs := captureLegacyProxyLogs(t)
		target := "http://media.example.test/%zz/private/clip.mp4?token=" + queryCanary
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		rec := httptest.NewRecorder()

		proxyRemoteVia(rec, req, target,
			map[string]string{"Authorization": "Bearer " + authCanary}, nil)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
		}
		assertLegacyProxyNoSensitiveLeak(t, rec, logs, target, queryCanary, authCanary)
		if got := logs.String(); !strings.Contains(got, "event=request_build_failed") {
			t.Errorf("construction failure log omits fixed event: %q", got)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		logs := captureLegacyProxyLogs(t)
		target := "https://media.example.test/private/clip.mp4?token=" + queryCanary
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		rec := httptest.NewRecorder()
		client := &http.Client{Transport: legacyProxyRoundTripFunc(func(upstream *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed for %s with %s",
				upstream.URL.String(), upstream.Header.Get("Authorization"))
		})}

		proxyRemoteVia(rec, req, target,
			map[string]string{"Authorization": "Bearer " + authCanary}, client)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
		}
		assertLegacyProxyNoSensitiveLeak(t, rec, logs, target, queryCanary, authCanary)
		gotLogs := logs.String()
		if !strings.Contains(gotLogs, `host="media.example.test"`) {
			t.Errorf("transport failure log omits sanitized host: %q", gotLogs)
		}
		if !strings.Contains(gotLogs, "event=request_failed") {
			t.Errorf("transport failure log omits fixed event: %q", gotLogs)
		}
	})
}

func TestLegacyProxyRangeLogsUseWhitelistedFields(t *testing.T) {
	const (
		queryCanary = "legacy-range-query-canary-4d82"
		authCanary  = "legacy-range-auth-canary-b6e9"
	)
	target := "https://media.example.test/private/clip.mp4?token=" + queryCanary
	headers := map[string]string{"Authorization": "Bearer " + authCanary}

	t.Run("ignored range without length", func(t *testing.T) {
		logs := captureLegacyProxyLogs(t)
		client := &http.Client{Transport: legacyProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": {"video/mp4"}},
				Body:       io.NopCloser(strings.NewReader("media")),
			}, nil
		})}
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		req.Header.Set("Range", "bytes=10-19")
		rec := httptest.NewRecorder()

		proxyRemoteVia(rec, req, target, headers, client)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
		}
		assertLegacyProxyNoSensitiveLeak(t, rec, logs, target, queryCanary, authCanary)
		assertLegacyProxyRangeLog(t, logs.String(), "ignored_range_no_length", http.StatusOK)
	})

	t.Run("unusable partial content", func(t *testing.T) {
		logs := captureLegacyProxyLogs(t)
		client := &http.Client{Transport: legacyProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Status:     "206 Partial Content",
				Header: http.Header{
					"Content-Range": {"bytes unusable"},
					"Content-Type":  {"video/mp4"},
				},
				Body: io.NopCloser(strings.NewReader("junk")),
			}, nil
		})}
		req := httptest.NewRequest(http.MethodGet, "/api/stream/1?proxy=1", nil)
		req.Header.Set("Range", "bytes=0-3")
		rec := httptest.NewRecorder()

		proxyRemoteVia(rec, req, target, headers, client)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
		}
		assertLegacyProxyNoSensitiveLeak(t, rec, logs, target, queryCanary, authCanary)
		assertLegacyProxyRangeLog(t, logs.String(), "unusable_content_range", http.StatusPartialContent)
	})
}

func captureLegacyProxyLogs(t *testing.T) *bytes.Buffer {
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

func assertLegacyProxyNoSensitiveLeak(t *testing.T, rec *httptest.ResponseRecorder, logs *bytes.Buffer, target, queryCanary, authCanary string) {
	t.Helper()
	for name, value := range map[string]string{
		"response body":    rec.Body.String(),
		"response headers": fmt.Sprint(rec.Header()),
		"logs":             logs.String(),
	} {
		for _, secret := range []string{
			target,
			queryCanary,
			authCanary,
			"/private/clip.mp4",
			"?token=",
			"Authorization",
		} {
			if strings.Contains(value, secret) {
				t.Errorf("%s leaked %q: %q", name, secret, value)
			}
		}
	}
}

func assertLegacyProxyRangeLog(t *testing.T, got, event string, status int) {
	t.Helper()
	for _, want := range []string{
		"event=" + event,
		"transport=legacy_proxy",
		`host="media.example.test"`,
		fmt.Sprintf("status=%d", status),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("range failure log omits %q: %q", want, got)
		}
	}
}
