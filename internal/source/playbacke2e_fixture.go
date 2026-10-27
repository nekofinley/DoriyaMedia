//go:build playbacke2e

package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"doriyamedia/internal/models"
)

const (
	playbackE2ESourceType    = "playbacke2e-dash"
	playbackE2EModeFile      = "PLAYBACK_E2E_DASH_MODE_FILE"
	playbackE2EVideoURL      = "PLAYBACK_E2E_DASH_VIDEO_URL"
	playbackE2EAudioURL      = "PLAYBACK_E2E_DASH_AUDIO_URL"
	playbackE2ECookie        = "PLAYBACK_E2E_DASH_UNSAFE_COOKIE"
	playbackE2EAuthorization = "PLAYBACK_E2E_DASH_UNSAFE_AUTHORIZATION"
	playbackE2EPath          = "/episode.mp4"
)

var errPlaybackE2EFixture = errors.New("playbacke2e fixture: invalid configuration")

type playbackE2EDriver struct {
	partsCalls atomic.Int32
}

func newPlaybackE2EDriver(
	src *models.Source,
	_ models.SourceConfig,
) (Driver, bool, error) {
	if src == nil || !strings.EqualFold(src.Type, playbackE2ESourceType) {
		return nil, false, nil
	}
	return &playbackE2EDriver{}, true, nil
}

func playbackE2ESourceTypeSupported(sourceType string) bool {
	return strings.EqualFold(sourceType, playbackE2ESourceType)
}

func (*playbackE2EDriver) List(ctx context.Context, dir string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dir != "" && dir != "/" {
		return nil, nil
	}
	return []Entry{{
		Name:  "episode.mp4",
		Path:  playbackE2EPath,
		Size:  2 << 20,
		MTime: time.Now().UTC().Unix(),
	}}, nil
}

func (*playbackE2EDriver) DirectURL(context.Context, string) (string, map[string]string, error) {
	return "", nil, ErrNoDirect
}

func (d *playbackE2EDriver) Parts(ctx context.Context, path string) ([]Part, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path != playbackE2EPath {
		return nil, fmt.Errorf("%w: unknown path", errPlaybackE2EFixture)
	}
	d.partsCalls.Add(1)
	mode, err := playbackE2EMode()
	if err != nil {
		return nil, err
	}
	videoURL := strings.TrimSpace(os.Getenv(playbackE2EVideoURL))
	audioURL := strings.TrimSpace(os.Getenv(playbackE2EAudioURL))
	if videoURL == "" || audioURL == "" || videoURL == audioURL {
		return nil, fmt.Errorf("%w: video and audio URLs are required", errPlaybackE2EFixture)
	}

	parts := []Part{
		{
			URL: videoURL,
			Headers: map[string]string{
				"Referer":    biliReferer,
				"User-Agent": biliUA,
			},
			MIME: "video/mp4", Kind: "video", Codec: "h264",
			Tag: "avc1", Profile: "High",
			Width: 1920, Height: 1080, Duration: 120,
		},
		{
			URL: audioURL,
			Headers: map[string]string{
				"Referer":    biliReferer,
				"User-Agent": biliUA,
			},
			MIME: "audio/mp4", Kind: "audio", Codec: "aac",
			Tag: "mp4a", Channels: 2, Duration: 120,
		},
	}

	switch mode {
	case "valid":
		return parts, nil
	case "single":
		return parts[:1], nil
	case "bad-header":
		return playbackE2EBadHeaderParts(parts)
	case "valid-probe-and-select-then-bad-header":
		// Create performs one probe, two candidate selections, and then one
		// direct-dash issuance. Only the issuance call must see unsafe headers.
		if d.partsCalls.Load() <= 3 {
			return parts, nil
		}
		return playbackE2EBadHeaderParts(parts)
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", errPlaybackE2EFixture, mode)
	}
}

func playbackE2EBadHeaderParts(parts []Part) ([]Part, error) {
	cookie := strings.TrimSpace(os.Getenv(playbackE2ECookie))
	authorization := strings.TrimSpace(os.Getenv(playbackE2EAuthorization))
	if cookie == "" || authorization == "" {
		return nil, fmt.Errorf("%w: unsafe header canaries are required", errPlaybackE2EFixture)
	}
	parts[0].Headers["Cookie"] = cookie
	parts[1].Headers["Authorization"] = authorization
	return parts, nil
}

func playbackE2EMode() (string, error) {
	path := strings.TrimSpace(os.Getenv(playbackE2EModeFile))
	if path == "" {
		return "", fmt.Errorf("%w: mode file is required", errPlaybackE2EFixture)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: read mode file", errPlaybackE2EFixture)
	}
	mode := strings.ToLower(strings.TrimSpace(string(raw)))
	if mode == "" {
		return "", fmt.Errorf("%w: mode file is empty", errPlaybackE2EFixture)
	}
	return mode, nil
}

func (*playbackE2EDriver) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("playbacke2e fixture has no file bytes")
}

func (d *playbackE2EDriver) ProbeURL(ctx context.Context, path string) (string, map[string]string, error) {
	parts, err := d.Parts(ctx, path)
	if err != nil {
		return "", nil, err
	}
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("%w: source returned no streams", errPlaybackE2EFixture)
	}
	return parts[0].URL, parts[0].Headers, nil
}

func (*playbackE2EDriver) HasDirect() bool {
	return false
}

func (*playbackE2EDriver) BypassPartsCache() bool {
	return true
}
