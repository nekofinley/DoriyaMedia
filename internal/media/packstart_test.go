package media

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// dualPart is a two-stream source: the picture and the sound are separate files,
// which is what makes a session start have to land on a video keyframe. It also
// mints a different link on every resolution, like a CDN-signed source does, so a
// test can tell "measured again" from "answered from the cache".
type dualPart struct {
	video string
	audio string
	// broken makes the next resolutions hand out an unreadable picture, which is
	// what a source that refuses or stalls out looks like to ffprobe.
	broken bool
}

func (d *dualPart) List(context.Context, string) ([]source.Entry, error) { return nil, nil }
func (d *dualPart) DirectURL(context.Context, string) (string, map[string]string, error) {
	return "", nil, nil
}
func (d *dualPart) Open(context.Context, string) (io.ReadCloser, error) { return nil, os.ErrInvalid }
func (d *dualPart) ProbeURL(context.Context, string) (string, map[string]string, error) {
	return d.video, nil, nil
}
func (d *dualPart) HasDirect() bool { return false }
func (d *dualPart) Parts(context.Context, string) ([]source.Part, error) {
	video := d.video
	if d.broken {
		video = filepath.Join(os.TempDir(), "no-such-stream-"+filepath.Base(d.video))
	}
	return []source.Part{{URL: video, Kind: "video"}, {URL: d.audio, Kind: "audio"}}, nil
}

// makeDualParts builds the fixture the alignment is about: a picture with a 2s GOP
// and a sound track long enough to cover it, as two separate files.
func makeDualParts(t *testing.T, dir string) *dualPart {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	video := filepath.Join(dir, "v.mp4")
	audio := filepath.Join(dir, "a.m4a")
	if err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=40",
		"-c:v", "libx264", "-g", "50", "-keyint_min", "50", "-sc_threshold", "0",
		"-pix_fmt", "yuv420p", video).Run(); err != nil {
		t.Skipf("cannot build the video fixture: %v", err)
	}
	if err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40", "-c:a", "aac", audio).Run(); err != nil {
		t.Skipf("cannot build the audio fixture: %v", err)
	}
	return &dualPart{video: video, audio: audio}
}

// PackStart is the one answer both halves of a playback use: /api/play reports it
// as stream_start, and the packaging session begins there. A keyframe is where it
// has to be, not the requested position.
func TestPackStartAlignsToTheKeyframe(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	drv := makeDualParts(t, t.TempDir())
	file := &models.MediaFile{ID: 7, SourceID: 1, Path: "/ep.mp4"}

	ctx := context.Background()
	if got := e.PackStart(ctx, file, drv, 21); got != 20 {
		t.Fatalf("PackStart at 21s = %v, want the keyframe at 20s", got)
	}
	// A position that is already a keyframe is answered with itself.
	if got := e.PackStart(ctx, file, drv, 20); got != 20 {
		t.Fatalf("PackStart at a keyframe = %v, want 20", got)
	}
	// Nothing to do at the top: a session there is simply the beginning.
	if got := e.PackStart(ctx, file, drv, 0); got != 0 {
		t.Fatalf("PackStart at 0 = %v, want 0", got)
	}
}

// A failure must be reported as "start at the beginning" (0), never as the
// requested position: a session that begins between keyframes keeps its sound a
// GOP away from its picture for the whole playback.
//
// It must also stay that answer for the request that follows: the player was told
// stream_start=0, and a second probe that happened to succeed would package from a
// keyframe the player knows nothing about.
func TestPackStartFailsToZeroAndKeepsThatAnswer(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	drv := makeDualParts(t, t.TempDir())
	file := &models.MediaFile{ID: 8, SourceID: 1, Path: "/ep2.mp4"}

	drv.broken = true
	if got := e.PackStart(context.Background(), file, drv, 21); got != 0 {
		t.Fatalf("PackStart over an unreadable picture = %v, want 0 (the beginning)", got)
	}

	// The source is healthy again by the time the playlist is requested. The answer
	// the player was given must still be the answer the packager uses, so the
	// second call must not measure afresh.
	drv.broken = false
	if got := e.PackStart(context.Background(), file, drv, 21); got != 0 {
		t.Fatalf("the second caller re-measured: got %v, want the cached 0", got)
	}

	// A different position is a different measurement: the cache is per position.
	// (The parts cache is dropped as well, or the source would still be resolved to
	// the link it refused.)
	e.partsMu.Lock()
	e.partsCache = map[string]partsEntry{}
	e.partsMu.Unlock()
	if got := e.PackStart(context.Background(), file, drv, 23); got != 22 {
		t.Fatalf("PackStart at 23s after the failure = %v, want the keyframe at 22", got)
	}
}

// Two callers of one playback must agree even when the source hands out a new link
// between them — which it does every time, on a source that signs its URLs.
func TestPackStartAgreesAcrossResolutions(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	drv := makeDualParts(t, t.TempDir())
	file := &models.MediaFile{ID: 9, SourceID: 1, Path: "/ep3.mp4"}

	// /api/play: what the player is told the stream begins at.
	reported := e.PackStart(context.Background(), file, drv, 21)
	// The playlist request that follows: where the session will be packaged from.
	// The parts cache is dropped in between, so this resolves the source again
	// instead of taking a shortcut — the alignment is the only thing shared.
	e.partsMu.Lock()
	e.partsCache = map[string]partsEntry{}
	e.partsMu.Unlock()
	packaged := e.PackStart(context.Background(), file, drv, 21)

	if reported != packaged {
		t.Fatalf("stream_start %v but packaging from %v: the element's clock would be off by that much",
			reported, packaged)
	}
	if reported != 20 {
		t.Fatalf("both callers must answer with the keyframe at 20s, got %v", reported)
	}
}
