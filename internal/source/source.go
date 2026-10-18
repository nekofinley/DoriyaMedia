package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"doriyamedia/internal/models"
)

// Entry is one row of a directory listing, in driver path space.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// Driver abstracts a storage source.
type Driver interface {
	List(ctx context.Context, dir string) ([]Entry, error)
	// DirectURL returns a browsable URL for the file plus any headers that
	// MUST be sent when fetching it (nil if none). Only called when HasDirect().
	DirectURL(ctx context.Context, path string) (string, map[string]string, error)
	// Open returns a reader of the whole file (used for small files: .strm, subtitles).
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	// ProbeURL returns a URL plus headers that ffprobe can consume, or "" when
	// probing must happen by another means (local files pass a filesystem path).
	ProbeURL(ctx context.Context, path string) (string, map[string]string, error)
	HasDirect() bool
}

// Part is one upstream stream of a file that is really several of them. It is
// the shape a DASH source has: the picture and the sound sit at two different
// URLs, and each one needs its own request headers. A driver that can answer
// Parts states the codecs too, so its files need no ffprobe pass at all.
type Part struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"-"`
	Kind    string            `json:"kind"` // "video" | "audio"
	// Codec is the ffprobe codec name (h264, hevc, av1, aac…), Tag the
	// codec_tag_string (hvc1 / hev1 / avc1) and Profile the H.264 profile — the
	// three things the playback decision actually reads.
	Codec    string  `json:"codec"`
	Tag      string  `json:"tag,omitempty"`
	Profile  string  `json:"profile,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Channels int     `json:"channels,omitempty"`
	// Duration is the stream's length in seconds (0 when the source is silent
	// about it).
	Duration float64 `json:"duration,omitempty"`
}

// MultiPart is an optional Driver capability: the file is not one progressive
// stream but several that have to be muxed together before any client can play
// it. Such a driver also answers ProbeURL and DirectURL with the video part, so
// a caller that ignores this interface still gets a usable — if silent — target;
// HasDirect should be false, because a link a client cannot fetch is not a
// direct link.
type MultiPart interface {
	Parts(ctx context.Context, path string) ([]Part, error)
}

// Comment is one bullet comment (弹幕) published against a file.
type Comment struct {
	Time  float64 `json:"t"`     // seconds into the file
	Mode  int     `json:"mode"`  // 1-3 scroll, 4 bottom, 5 top, 6 reverse, 7+ advanced
	Size  int     `json:"size"`  // font size as published; 25 is the normal one
	Color int     `json:"color"` // 0xRRGGBB
	Text  string  `json:"text"`
}

// Danmaku is an optional Driver capability: the file has bullet comments that
// belong on top of it. The whole set is returned in one call — a player wants
// them before playback starts, not as a live feed.
type Danmaku interface {
	Comments(ctx context.Context, path string) ([]Comment, error)
}

// NewDriver builds a driver from a source row.
func NewDriver(src *models.Source) (Driver, error) {
	var cfg models.SourceConfig
	if src.Config != "" {
		if err := jsonUnmarshal(src.Config, &cfg); err != nil {
			return nil, fmt.Errorf("source %d config: %w", src.ID, err)
		}
	}
	switch src.Type {
	case "local":
		return NewLocal(cfg.Root), nil
	case "webdav":
		return NewWebDAV(cfg.WebDAVURL, cfg.WebDAVUser, cfg.WebDAVPass), nil
	case "openlist":
		return NewOpenList(cfg.BaseURL, cfg.Token)
	case "bilibili":
		return NewBilibili(cfg.BiliCookie, cfg.BiliQuality, cfg.BiliCodec)
	case "url":
		return &URLDriver{}, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", src.Type)
	}
}

// ---------- shared helpers ----------

var httpClient = &http.Client{
	Timeout: 0, // streaming; per-call contexts apply
	Transport: &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

// escapeURLPath escapes each path segment, preserving "/" separators.
func escapeURLPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func joinURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(escapeURLPath(path), "/")
}

func httpDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	req = req.WithContext(ctx)
	return httpClient.Do(req)
}

// VirtualJoin joins a virtual base dir and child name into a virtual path.
func VirtualJoin(dir, name string) string {
	return strings.TrimSuffix(dir, "/") + "/" + name
}
