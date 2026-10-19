package media

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Segment generation progress is what the player renders as a progress bar, so
// the counters must reflect reality: successes add timeline seconds, failures do
// not, and the realtime factor is generated seconds per wall second.
func TestStreamStatusTracksGeneration(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	e.resetStreamProgress()
	// Spend a measurable amount of work per segment so the throughput figure has
	// real inputs; an instantaneous fake would divide by ~0.
	e.runStream = func(_ context.Context, _ []string, out io.Writer) error {
		time.Sleep(20 * time.Millisecond)
		_, err := out.Write([]byte("segment"))
		return err
	}

	file := testFile()
	if st := e.StreamStatus(file.ID); st != nil {
		t.Fatalf("no activity must report nil, got %+v", st)
	}

	if _, err := e.Segment(context.Background(), file, drv, 0, 4, "transcode", "aac", 0, 0, 1080, io.Discard); err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if _, err := e.Segment(context.Background(), file, drv, 4, 4, "transcode", "aac", 0, 0, 1080, io.Discard); err != nil {
		t.Fatalf("Segment: %v", err)
	}

	st := e.StreamStatus(file.ID)
	if st == nil {
		t.Fatal("progress must be reported after a segment")
	}
	if st.SegmentsRequested != 2 || st.SegmentsReady != 2 {
		t.Fatalf("requested=%d ready=%d, want 2/2", st.SegmentsRequested, st.SegmentsReady)
	}
	if st.GeneratedSeconds != 8 {
		t.Fatalf("generated=%.1fs, want 8s", st.GeneratedSeconds)
	}
	if st.LastSegmentBytes != int64(len("segment")) {
		t.Fatalf("last bytes=%d, want %d", st.LastSegmentBytes, len("segment"))
	}
	// 8s of timeline from ~40ms of work: far above realtime, and finite.
	if st.RealtimeFactor <= 1 {
		t.Fatalf("realtime factor=%.2f, want > 1 (work is much faster than playback)", st.RealtimeFactor)
	}
	if st.LastSegmentMs < 15 {
		t.Fatalf("last segment took %dms, expected the fake's ~20ms to be recorded", st.LastSegmentMs)
	}
}

func TestStreamStatusCountsFailuresWithoutGenerating(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	e.resetStreamProgress()
	e.runStream = func(context.Context, []string, io.Writer) error {
		return errors.New("403 Forbidden")
	}

	file := testFile()
	if _, err := e.Segment(context.Background(), file, drv, 0, 4, "transcode", "aac", 0, 0, 1080, io.Discard); err == nil {
		t.Fatal("want an error")
	}

	st := e.StreamStatus(file.ID)
	if st == nil {
		t.Fatal("a failed attempt must still be reported")
	}
	if st.SegmentsReady != 0 {
		t.Fatalf("ready=%d, want 0", st.SegmentsReady)
	}
	if st.GeneratedSeconds != 0 {
		t.Fatalf("generated=%.1fs, want 0 (a failed segment produces no timeline)", st.GeneratedSeconds)
	}
	if st.SegmentsFailed == 0 {
		t.Fatal("the failure must be counted")
	}
	if st.LastSegmentBytes != 0 {
		t.Fatalf("last bytes=%d, want 0", st.LastSegmentBytes)
	}
}

// A fresh play (or a seek) after a long gap starts a new burst, so the progress
// bar does not start out already full.
func TestStreamStatusResetsAfterIdleGap(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	e.resetStreamProgress()
	e.runStream = func(_ context.Context, _ []string, out io.Writer) error {
		_, err := out.Write([]byte("x"))
		return err
	}

	file := testFile()
	if _, err := e.Segment(context.Background(), file, drv, 0, 4, "transcode", "aac", 0, 0, 1080, io.Discard); err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if got := e.StreamStatus(file.ID).GeneratedSeconds; got != 4 {
		t.Fatalf("generated=%.1f, want 4", got)
	}

	// Age the burst past the idle threshold, as a new play would.
	e.streamMu.Lock()
	e.stream[file.ID].lastAt = time.Now().Add(-2 * burstIdle)
	e.streamMu.Unlock()

	if _, err := e.Segment(context.Background(), file, drv, 600, 4, "transcode", "aac", 0, 0, 1080, io.Discard); err != nil {
		t.Fatalf("Segment: %v", err)
	}
	st := e.StreamStatus(file.ID)
	if st.GeneratedSeconds != 4 {
		t.Fatalf("generated=%.1f after a new burst, want 4", st.GeneratedSeconds)
	}
	if st.SegmentsRequested != 1 {
		t.Fatalf("requested=%d after a new burst, want 1", st.SegmentsRequested)
	}
}

// A packaged playback has to be able to answer "is the server still working on
// it?". Its segments never pass through the per-request counters, so without this
// a player is told nothing, applies its own timeout, and gives up on a stream that
// is merely slow to start — the first seconds of a cold 4K file on a cloud drive.
func TestPackagerProgressReportsWhatASessionProduced(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	dir := filepath.Join(e.Cfg.Abs(), "cache", "hls", "7-remux-a0-v0-h1080-s0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(dir, packagerPlaylistName),
		"#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n"+
			"#EXTINF:4.000,\nseg00000.m4s\n#EXTINF:3.500,\nseg00001.m4s\n")

	key := SessionKey(7, "remux", 0, 0, 1080, 0)
	pack := &Packager{done: make(chan struct{})}
	s := &Session{key: key, dir: dir, pack: pack, lastUsed: time.Now(), startedAt: time.Now().Add(-4 * time.Second)}
	e.sessMu.Lock()
	e.sessions[key] = s
	e.sessMu.Unlock()

	st := e.PackagerProgress(7)
	if st == nil {
		t.Fatal("a file with a live session must report progress")
	}
	if st.SegmentsReady != 2 {
		t.Fatalf("segments_ready = %d, want 2 (the playlist's EXTINF entries)", st.SegmentsReady)
	}
	if st.GeneratedSeconds != 7.5 {
		t.Fatalf("generated_seconds = %v, want 7.5", st.GeneratedSeconds)
	}
	if !st.PackagerRunning {
		t.Fatal("packager_running must be true while the packager is alive")
	}
	if st.RealtimeFactor <= 0 {
		t.Fatalf("realtime_factor = %v, want the production rate against the time it has had", st.RealtimeFactor)
	}

	// Once the packager exits, nothing is being produced any more: the client must
	// stop waiting on it and play what is already there.
	close(pack.done)
	st = e.PackagerProgress(7)
	if st == nil {
		t.Fatal("a finished session still has output worth reporting")
	}
	if st.PackagerRunning {
		t.Fatal("packager_running must be false once the packager has exited")
	}
	if st.SegmentsReady != 2 {
		t.Fatalf("segments_ready = %d after exit, want the 2 it produced", st.SegmentsReady)
	}
}

// No session at all means no opinion: the caller falls back to the per-request
// counters rather than being told "nothing is happening".
func TestPackagerProgressIsSilentWithoutASession(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	if st := e.PackagerProgress(4242); st != nil {
		t.Fatalf("progress for a file with no session = %+v, want nil", st)
	}
}

// A packager that has just started has produced nothing yet. That must read as
// "working", not as "idle for a very long time" — which is what a zero timestamp
// would say.
func TestPackagerProgressOfAJustStartedSessionLooksBusy(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	dir := filepath.Join(e.Cfg.Abs(), "cache", "hls", "9-remux-a0-v0-h1080-s0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	key := SessionKey(9, "remux", 0, 0, 1080, 0)
	e.sessMu.Lock()
	e.sessions[key] = &Session{key: key, dir: dir, pack: &Packager{done: make(chan struct{})}, startedAt: time.Now()}
	e.sessMu.Unlock()

	st := e.PackagerProgress(9)
	if st == nil {
		t.Fatal("want progress for a session that has just started")
	}
	if st.SegmentsReady != 0 {
		t.Fatalf("segments_ready = %d, want 0", st.SegmentsReady)
	}
	if st.IdleSeconds > 5 {
		t.Fatalf("idle_seconds = %.1f: a session that has just been started is not idle", st.IdleSeconds)
	}
	if !st.PackagerRunning {
		t.Fatal("packager_running must be true for a working packager")
	}
}
