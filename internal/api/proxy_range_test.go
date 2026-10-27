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
		name  string
		in    string
		start int64
		end   int64
		ok    bool
	}{
		{name: "closed", in: "bytes=100-199", start: 100, end: 199, ok: true},
		{name: "open ended", in: "bytes=100-", start: 100, end: -1, ok: true},
		{name: "zero", in: "bytes=0-0", start: 0, end: 0, ok: true},
		{name: "case insensitive unit", in: "Bytes=5-10", start: 5, end: 10, ok: true},
		{name: "leading zeros", in: "bytes=0005-0010", start: 5, end: 10, ok: true},
		{name: "outer OWS", in: " \tbytes=5-10\t ", start: 5, end: 10, ok: true},
		{name: "multipart", in: "bytes=100-199,300-399", ok: false},
		{name: "suffix", in: "bytes=-500", ok: false},
		{name: "empty spec", in: "bytes=", ok: false},
		{name: "wrong unit", in: "items=0-10", ok: false},
		{name: "nonnumeric", in: "bytes=abc-def", ok: false},
		{name: "descending", in: "bytes=200-100", ok: false},
		{name: "signed start", in: "bytes=+100-199", ok: false},
		{name: "signed end", in: "bytes=100-+199", ok: false},
		{name: "space after equals", in: "bytes= 100-199", ok: false},
		{name: "space before dash", in: "bytes=100 -199", ok: false},
		{name: "space after dash", in: "bytes=100- 199", ok: false},
		{name: "tab before dash", in: "bytes=100\t-199", ok: false},
		{name: "space around unit separator", in: "bytes =100-199", ok: false},
		{name: "unicode digits", in: "bytes=\u0661\u0662-1", ok: false},
		{name: "overflow", in: "bytes=9223372036854775808-9223372036854775809", ok: false},
		{name: "non OWS trailing control", in: "bytes=100-199\v", ok: false},
		{name: "empty", in: "", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseRequestRange(c.in)
			if ok != c.ok || (ok && (got.start != c.start || got.end != c.end)) {
				t.Errorf("parseRequestRange(%q) = (%+v,%v), want (%d,%d,%v)",
					c.in, got, ok, c.start, c.end, c.ok)
			}
		})
	}
}

func TestParseLegacyRequestRange(t *testing.T) {
	cases := []struct {
		name     string
		values   []string
		want     legacyRangeRequest
		hadRange bool
		ok       bool
	}{
		{name: "absent", hadRange: false, ok: true},
		{
			name:     "forward",
			values:   []string{"bytes=5-10"},
			want:     legacyRangeRequest{start: 5, end: 10},
			hadRange: true,
			ok:       true,
		},
		{
			name:     "suffix with outer OWS",
			values:   []string{" \tbytes=-0100\t "},
			want:     legacyRangeRequest{suffix: 100},
			hadRange: true,
			ok:       true,
		},
		{
			name:     "signed suffix",
			values:   []string{"bytes=-+100"},
			hadRange: true,
			ok:       false,
		},
		{
			name:     "suffix whitespace",
			values:   []string{"bytes=- 100"},
			hadRange: true,
			ok:       false,
		},
		{
			name:     "forward signed start",
			values:   []string{"bytes=+5-10"},
			hadRange: true,
			ok:       false,
		},
		{
			name:     "repeated",
			values:   []string{"bytes=5-10", "bytes=20-30"},
			hadRange: true,
			ok:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hadRange, ok := parseLegacyRequestRange(tc.values)
			if got != tc.want || hadRange != tc.hadRange || ok != tc.ok {
				t.Errorf("parseLegacyRequestRange(%q) = (%+v,%v,%v), want (%+v,%v,%v)",
					tc.values, got, hadRange, ok, tc.want, tc.hadRange, tc.ok)
			}
		})
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		start int64
		end   int64
		total int64
		ok    bool
	}{
		{
			name: "canonical", in: "bytes 0-99/4122738",
			start: 0, end: 99, total: 4122738, ok: true,
		},
		{
			name: "unknown total", in: "bytes 100-199/*",
			start: 100, end: 199, total: -1, ok: true,
		},
		{
			name: "case insensitive unit", in: "BYTES 0-99/100",
			start: 0, end: 99, total: 100, ok: true,
		},
		{
			name: "outer OWS", in: " \tbytes 0-99/100\t ",
			start: 0, end: 99, total: 100, ok: true,
		},
		{
			name: "leading zeros", in: "bytes 000-099/000100",
			start: 0, end: 99, total: 100, ok: true,
		},
		{name: "empty", in: "", ok: false},
		{name: "empty spec", in: "bytes ", ok: false},
		{name: "missing end", in: "bytes 1-", ok: false},
		{name: "nonnumeric", in: "bytes a-b/10", ok: false},
		{name: "descending", in: "bytes 5-4/10", ok: false},
		{name: "wrong unit", in: "0-99/4122738", ok: false},
		{name: "missing slash", in: "bytes 0-99", ok: false},
		{name: "signed start", in: "bytes +0-99/100", ok: false},
		{name: "signed end", in: "bytes 0-+99/100", ok: false},
		{name: "signed total", in: "bytes 0-99/+100", ok: false},
		{name: "space before dash", in: "bytes 0 -99/100", ok: false},
		{name: "space after dash", in: "bytes 0- 99/100", ok: false},
		{name: "space before slash", in: "bytes 0-99 /100", ok: false},
		{name: "space after slash", in: "bytes 0-99/ 100", ok: false},
		{name: "extra space after unit", in: "bytes  0-99/100", ok: false},
		{name: "tab after unit", in: "bytes\t0-99/100", ok: false},
		{name: "unicode digits", in: "bytes \u0660-\u0669/100", ok: false},
		{name: "overflow total", in: "bytes 0-99/9223372036854775808", ok: false},
		{name: "non OWS trailing control", in: "bytes 0-99/100\v", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, total, ok := parseContentRange(tc.in)
			if ok != tc.ok || (ok &&
				(start != tc.start || end != tc.end || total != tc.total)) {
				t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
					tc.in, start, end, total, ok, tc.start, tc.end, tc.total, tc.ok)
			}
		})
	}
}

func TestParseUnsatisfiedByteContentRange(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		total int64
		ok    bool
	}{
		{name: "canonical", in: "bytes */100", total: 100, ok: true},
		{name: "case insensitive unit", in: "BYTES */100", total: 100, ok: true},
		{name: "outer OWS", in: " \tbytes */100\t ", total: 100, ok: true},
		{name: "leading zeros", in: "bytes */000100", total: 100, ok: true},
		{name: "signed", in: "bytes */+100", ok: false},
		{name: "space after slash", in: "bytes */ 100", ok: false},
		{name: "extra space after unit", in: "bytes  */100", ok: false},
		{name: "tab after unit", in: "bytes\t*/100", ok: false},
		{name: "range response", in: "bytes 0-99/100", ok: false},
		{name: "unknown total", in: "bytes */*", ok: false},
		{name: "unicode digits", in: "bytes */\u0661\u0660\u0660", ok: false},
		{name: "overflow", in: "bytes */9223372036854775808", ok: false},
		{name: "empty", in: "", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, ok := parseUnsatisfiedByteContentRange(tc.in)
			if ok != tc.ok || (ok && total != tc.total) {
				t.Errorf("parseUnsatisfiedByteContentRange(%q) = (%d,%v), want (%d,%v)",
					tc.in, total, ok, tc.total, tc.ok)
			}
		})
	}
}

func TestProxyContentLengthRequiresASCIIDigits(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int64
		ok    bool
	}{
		{name: "decimal", value: "4", want: 4, ok: true},
		{name: "zero", value: "0", want: 0, ok: true},
		{name: "leading zeros", value: "0004", want: 4, ok: true},
		{name: "outer OWS", value: " \t4\t ", want: 4, ok: true},
		{name: "positive sign", value: "+4", want: -1},
		{name: "negative zero", value: "-0", want: -1},
		{name: "negative", value: "-4", want: -1},
		{name: "nonnumeric", value: "4x", want: -1},
		{name: "empty", value: "", want: -1},
		{name: "overflow", value: "9223372036854775808", want: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Header:        http.Header{"Content-Length": {tc.value}},
				ContentLength: -1,
			}
			legacyLength, legacyOK := legacyProxyContentLength(resp)
			if legacyLength != tc.want || legacyOK != tc.ok {
				t.Errorf("legacyProxyContentLength(%q) = (%d,%v), want (%d,%v)",
					tc.value, legacyLength, legacyOK, tc.want, tc.ok)
			}
			if v1Length := v1ProxyContentLength(resp); v1Length != tc.want {
				t.Errorf("v1ProxyContentLength(%q) = %d, want %d",
					tc.value, v1Length, tc.want)
			}
		})
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
