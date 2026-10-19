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

// PartialLister is an optional Driver capability: a directory listing is a
// sample, not an inventory. bilibili's root is the account's feed (follows +
// airing + hot), so a series can be absent from it without having been deleted;
// a scanner that reads that absence as deletion removes a library's shows
// whenever they leave the feed. Such a source may only be cleaned at the leaf:
// a directory that was actually read and no longer holds a file proves that
// file is gone, while a missing ancestor proves nothing.
type PartialLister interface {
	PartialListing() bool
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
	Codec    string `json:"codec"`
	Tag      string `json:"tag,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Channels int    `json:"channels,omitempty"`
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

// ProxiedDriver is an optional Driver capability: the source reaches its host
// through a proxy of its own (bilibili from a server abroad), and everything
// that fetches its bytes on the server's side has to go through that same proxy
// — including ffmpeg, which is handed a loopback bridge because it cannot dial
// SOCKS5 or Shadowsocks itself.
//
// The string is the setting as configured, in the forms netproxy.Parse accepts;
// an empty string means direct, which is what a driver without this interface
// already means.
type ProxiedDriver interface {
	ProxySpec() string
}

// CatalogSearcher is an optional Driver capability: the source can search its
// own upstream catalogue for titles a library has not scanned yet, and resolve
// the directory a catalogue result lives under. The scanner then imports just
// that series subtree — the whole catalogue is far too large to walk.
type CatalogSearcher interface {
	// SearchCatalog finds seasons by keyword, best match first.
	SearchCatalog(ctx context.Context, keyword string, limit int) ([]CatalogHit, error)
	// CatalogImportPath returns the virtual directory of the series containing
	// seasonID, plus the series title for display.
	CatalogImportPath(ctx context.Context, seasonID int64) (path, title string, err error)
}

// CatalogHit is one season found in a source's catalogue, before it is in the
// library. It carries just enough to draw a card and let the user click through.
type CatalogHit struct {
	SeasonID int64   `json:"season_id"`
	Title    string  `json:"title"`
	Original string  `json:"original_title,omitempty"`
	Overview string  `json:"overview,omitempty"`
	Cover    string  `json:"cover,omitempty"`
	Year     int     `json:"year,omitempty"`
	Episodes int     `json:"episodes,omitempty"`
	Score    float64 `json:"score,omitempty"`
	Index    string  `json:"index_show,omitempty"`
	Kind     string  `json:"kind,omitempty"`
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
		return NewBilibili(cfg.BiliCookie, cfg.BiliQuality, cfg.BiliCodec, cfg.BiliProxy)
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
