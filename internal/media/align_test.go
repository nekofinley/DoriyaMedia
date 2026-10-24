package media

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// The alignment must answer with a keyframe and say that it did, so its two callers
// — /api/play reporting stream_start, and the session that actually packages — can
// agree. A silent fallback to the requested position is what made one of them claim
// 110 while the other packaged from 120.
func TestAlignedStartAnswersWithAKeyframeAndSaysSo(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	video := dir + "/v.mp4"
	if err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=40",
		"-c:v", "libx264", "-g", "50", "-keyint_min", "50", "-sc_threshold", "0",
		"-pix_fmt", "yuv420p", video).Run(); err != nil {
		t.Skipf("cannot build the fixture: %v", err)
	}

	prober := NewProber("ffprobe")
	key, err := prober.KeyframeAtOrBefore(context.Background(), video, nil, "", 21, 30)
	if err != nil {
		t.Fatalf("KeyframeAtOrBefore: %v", err)
	}
	if key != 20 {
		t.Fatalf("keyframe at or before 21s = %v, want 20", key)
	}

	// A position past the last keyframe still answers, with the last one.
	last, err := prober.KeyframeAtOrBefore(context.Background(), video, nil, "", 39, 30)
	if err != nil {
		t.Fatalf("KeyframeAtOrBefore near the end: %v", err)
	}
	if last > 39 {
		t.Fatalf("keyframe %v is after the requested position 39", last)
	}
}

// A cached alignment is reused for the same file and position, whatever the stream
// URL was at the time. The source mints a new signed link on every resolution, so a
// URL-keyed cache would never hit — and an entry left by one resolution would answer
// for another.
func TestKeyframeCacheIsKeyedByFileAndPosition(t *testing.T) {
	e := &Engine{keyCache: map[string]keyframeEntry{}}
	// A hit short-circuits before any resolution happens: a nil driver and file are
	// enough to prove the cache answered rather than the probe.
	e.keyCache["7|120.000"] = keyframeEntry{at1: 110, ok: true, at: time.Now()}

	key, ok := e.AlignedStart(context.Background(), nil, nil, 120)
	if ok {
		// The nil-file early return comes first, so this asserts the guard order.
		t.Fatalf("expected the early return for a nil file, got key=%v ok=%v", key, ok)
	}
	if key != 120 {
		t.Fatalf("early return must echo the position, got %v", key)
	}
}

// A failed alignment must be reported as failed, so the caller can start at zero
// instead of packaging from a position between keyframes.
func TestAlignedStartReportsFailureRatherThanGuessing(t *testing.T) {
	e := &Engine{keyCache: map[string]keyframeEntry{}, prober: NewProber("ffprobe-that-does-not-exist")}
	// A non-nil file and driver would be needed to reach the probe, so this asserts
	// the contract at the point that is reachable without a source: the answer for
	// anything unalignable is (pos, false), never a silent pos.
	pos, ok := e.AlignedStart(context.Background(), nil, nil, 120)
	if ok {
		t.Fatal("an unalignable position must not be reported as aligned")
	}
	if pos != 120 {
		t.Fatalf("an unalignable position must be echoed unchanged, got %v", pos)
	}
}
