package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/source"
)

func newV1LocalMediaFixture(t *testing.T, body string) (string, time.Time) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write media fixture: %v", err)
	}
	modTime := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		t.Fatalf("set media fixture time: %v", err)
	}
	return target, modTime
}

func runV1LocalMedia(
	t *testing.T,
	method, target string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/playback/media-sessions/ms_test/stream", nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	serveV1LocalMedia(rec, req, target)
	return rec
}

func TestPlaybackLocalMediaRangeSemantics(t *testing.T) {
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	target, modTime := newV1LocalMediaFixture(t, body)
	full := runV1LocalMedia(t, http.MethodGet, target, nil)
	etag := full.Header().Get("ETag")

	t.Run("full GET and HEAD", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method, func(t *testing.T) {
				rec := runV1LocalMedia(t, method, target, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
				}
				assertPlaybackProxyHeader(t, rec, "Content-Length", strconv.Itoa(len(body)))
				assertPlaybackProxyHeader(t, rec, "Accept-Ranges", "bytes")
				assertPlaybackProxyHeader(t, rec, "Content-Type", "video/mp4")
				assertPlaybackProxyHeader(t, rec, "Last-Modified", modTime.Format(http.TimeFormat))
				assertPlaybackProxyHeader(t, rec, "ETag", etag)
				if method == http.MethodHead {
					if rec.Body.Len() != 0 {
						t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
					}
				} else if got := rec.Body.String(); got != body {
					t.Errorf("body = %q, want full representation", got)
				}
			})
		}
	})

	t.Run("first byte", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method, func(t *testing.T) {
				rec := runV1LocalMedia(t, method, target,
					map[string]string{"Range": "bytes=0-0"})
				if rec.Code != http.StatusPartialContent {
					t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
				}
				assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 0-0/36")
				assertPlaybackProxyHeader(t, rec, "Content-Length", "1")
				if method == http.MethodHead {
					if rec.Body.Len() != 0 {
						t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
					}
				} else if got := rec.Body.String(); got != "0" {
					t.Errorf("body = %q, want first byte", got)
				}
			})
		}
	})

	t.Run("closed and open ranges", func(t *testing.T) {
		for _, tc := range []struct {
			name, value, contentRange, want string
		}{
			{name: "closed", value: "bytes=2-5", contentRange: "bytes 2-5/36", want: "2345"},
			{name: "open", value: "bytes=30-", contentRange: "bytes 30-35/36", want: "uvwxyz"},
			{name: "clamped", value: "bytes=30-999", contentRange: "bytes 30-35/36", want: "uvwxyz"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := runV1LocalMedia(t, http.MethodGet, target,
					map[string]string{"Range": tc.value})
				if rec.Code != http.StatusPartialContent {
					t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
				}
				assertPlaybackProxyHeader(t, rec, "Content-Range", tc.contentRange)
				if got := rec.Body.String(); got != tc.want {
					t.Errorf("body = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("unsatisfiable", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method, func(t *testing.T) {
				rec := runV1LocalMedia(t, method, target,
					map[string]string{"Range": "bytes=36-"})
				if rec.Code != http.StatusRequestedRangeNotSatisfiable {
					t.Fatalf("status = %d, want 416; body=%q", rec.Code, rec.Body.String())
				}
				assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes */36")
				if method == http.MethodHead && rec.Body.Len() != 0 {
					t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
				}
			})
		}
	})
}

func TestPlaybackLocalMediaIfRange(t *testing.T) {
	const body = "0123456789"
	target, modTime := newV1LocalMediaFixture(t, body)
	full := runV1LocalMedia(t, http.MethodGet, target, nil)
	etag := full.Header().Get("ETag")
	if etag == "" {
		t.Fatal("full response did not set ETag")
	}

	t.Run("matching entity tag", func(t *testing.T) {
		rec := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
			"Range": "bytes=2-4", "If-Range": etag,
		})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != "234" {
			t.Errorf("body = %q, want matching range", got)
		}
	})

	t.Run("modification date is not a strong validator", func(t *testing.T) {
		rec := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
			"Range": "bytes=6-8", "If-Range": modTime.Format(http.TimeFormat),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != body {
			t.Errorf("body = %q, want full representation", got)
		}
	})

	t.Run("mismatching validator serves full representation", func(t *testing.T) {
		rec := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
			"Range": "bytes=6-8", "If-Range": `"different"`,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != body {
			t.Errorf("body = %q, want full representation", got)
		}
	})

	for _, tc := range []struct {
		name    string
		ifRange string
	}{
		{
			name:    "future modification date",
			ifRange: modTime.Add(time.Second).Format(http.TimeFormat),
		},
		{
			name:    "stale modification date",
			ifRange: modTime.Add(-time.Second).Format(http.TimeFormat),
		},
		{
			name:    "invalid modification date",
			ifRange: "not-a-validator",
		},
		{
			name:    "weak entity tag",
			ifRange: `W/` + etag,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
				"Range": "bytes=6-8", "If-Range": tc.ifRange,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != body {
				t.Errorf("body = %q, want full representation", got)
			}
		})
	}
}

func TestPlaybackLocalMediaETagChangesAfterSameSizeRewrite(t *testing.T) {
	const (
		original    = "0123456789"
		replacement = "abcdefghij"
	)
	target, modTime := newV1LocalMediaFixture(t, original)
	before := runV1LocalMedia(t, http.MethodGet, target, nil)
	oldETag := before.Header().Get("ETag")
	if oldETag == "" {
		t.Skip("platform does not provide a reliable local file version")
	}

	time.Sleep(25 * time.Millisecond)
	if err := os.WriteFile(target, []byte(replacement), 0o600); err != nil {
		t.Fatalf("rewrite media fixture: %v", err)
	}
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		t.Fatalf("restore media fixture time: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat rewritten media fixture: %v", err)
	}
	if !info.ModTime().Equal(modTime) {
		t.Fatalf("rewritten media modification time = %s, want %s",
			info.ModTime(), modTime)
	}
	if info.Size() != int64(len(original)) {
		t.Fatalf("rewritten media size = %d, want %d",
			info.Size(), len(original))
	}

	after := runV1LocalMedia(t, http.MethodGet, target, nil)
	newETag := after.Header().Get("ETag")
	if newETag == "" {
		t.Fatal("rewritten media did not retain a strong ETag")
	}
	if newETag == oldETag {
		t.Fatalf("ETag = %q after same-size rewrite with restored mtime", newETag)
	}

	stale := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
		"Range": "bytes=2-4", "If-Range": oldETag,
	})
	if stale.Code != http.StatusOK {
		t.Fatalf("stale If-Range status = %d, want 200; body=%q",
			stale.Code, stale.Body.String())
	}
	assertPlaybackProxyHeader(t, stale, "Content-Length", strconv.Itoa(len(replacement)))
	if got := stale.Header().Get("Content-Range"); got != "" {
		t.Errorf("stale If-Range Content-Range = %q, want empty", got)
	}
	if got := stale.Body.String(); got != replacement {
		t.Errorf("stale If-Range body = %q, want complete replacement", got)
	}

	current := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
		"Range": "bytes=2-4", "If-Range": newETag,
	})
	if current.Code != http.StatusPartialContent {
		t.Fatalf("current If-Range status = %d, want 206; body=%q",
			current.Code, current.Body.String())
	}
	if got := current.Body.String(); got != "cde" {
		t.Errorf("current If-Range body = %q, want cde", got)
	}
}

func TestPlaybackLocalMediaWithoutReliableVersionDisablesIfRange(t *testing.T) {
	const body = "0123456789"
	target, modTime := newV1LocalMediaFixture(t, body)
	f, err := os.Open(target)
	if err != nil {
		t.Fatalf("open media fixture: %v", err)
	}
	_, supported, _ := localFileVersionForFile(f)
	_ = f.Close()
	if supported {
		t.Skip("platform provides a reliable local file version")
	}

	full := runV1LocalMedia(t, http.MethodGet, target, nil)
	if got := full.Header().Get("ETag"); got != "" {
		t.Fatalf("ETag = %q without a reliable file version", got)
	}
	for _, validator := range []string{
		`"old-file-version"`,
		modTime.Format(http.TimeFormat),
	} {
		rec := runV1LocalMedia(t, http.MethodGet, target, map[string]string{
			"Range": "bytes=2-4", "If-Range": validator,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("If-Range %q status = %d, want 200; body=%q",
				validator, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Range"); got != "" {
			t.Errorf("If-Range %q Content-Range = %q, want empty", validator, got)
		}
		if got := rec.Body.String(); got != body {
			t.Errorf("If-Range %q body = %q, want complete file", validator, got)
		}
	}
}

func TestPlaybackLocalMediaRejectsUnsupportedRangeShapes(t *testing.T) {
	target, _ := newV1LocalMediaFixture(t, "0123456789")
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "multiple", value: "bytes=0-1,3-4"},
		{name: "suffix", value: "bytes=-2"},
		{name: "backward", value: "bytes=4-2"},
		{name: "empty", value: ""},
		{name: "wrong unit", value: "items=0-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runV1LocalMedia(t, http.MethodGet, target,
				map[string]string{"Range": tc.value})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
				t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
			}
		})
	}

	t.Run("repeated header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/playback/media-sessions/ms_test/stream", nil)
		req.Header.Add("Range", "bytes=0-1")
		req.Header.Add("Range", "bytes=3-4")
		rec := httptest.NewRecorder()
		serveV1LocalMedia(rec, req, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("repeated if-range", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/playback/media-sessions/ms_test/stream", nil)
		req.Header.Set("Range", "bytes=0-1")
		req.Header.Add("If-Range", `"one"`)
		req.Header.Add("If-Range", `"two"`)
		rec := httptest.NewRecorder()
		serveV1LocalMedia(rec, req, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
		if got := playbackProxyErrorCode(t, rec); got != V1ErrorInvalidRequest {
			t.Errorf("error code = %q, want %q", got, V1ErrorInvalidRequest)
		}
	})
}

func TestPlaybackLocalMediaRejectsOtherMethods(t *testing.T) {
	target, _ := newV1LocalMediaFixture(t, "0123456789")
	rec := runV1LocalMedia(t, http.MethodPost, target, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
}

func TestPlaybackLocalMediaMissingFileDoesNotLeakPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing-private-name.mp4")
	rec := runV1LocalMedia(t, http.MethodGet, target, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	raw, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(raw), target) || strings.Contains(string(raw), "missing-private-name") {
		t.Fatalf("missing-file response leaked path: %q", raw)
	}
}

func TestPlaybackLocalMediaUsesOpenedHandleAfterPathReplacement(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original media fixture: %v", err)
	}
	opened, err := source.NewLocal(dir).Open(context.Background(), "/clip.mp4")
	if err != nil {
		t.Fatalf("open original media fixture: %v", err)
	}
	defer opened.Close()
	openedFile, ok := opened.(*os.File)
	if !ok {
		t.Fatalf("local driver returned %T, want *os.File", opened)
	}

	if err := os.Rename(target, target+".moved"); err != nil {
		t.Fatalf("move original media fixture: %v", err)
	}
	if err := os.WriteFile(target, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement media fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/playback/media-sessions/ms_test/stream", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	serveV1LocalMediaFile(rec, req, target, openedFile)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
	}
	assertPlaybackProxyHeader(t, rec, "Content-Range", "bytes 0-3/8")
	if got := rec.Body.String(); got != "orig" {
		t.Fatalf("body = %q, want bytes from the already-opened media fixture", got)
	}
}
