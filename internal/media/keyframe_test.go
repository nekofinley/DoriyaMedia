package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A session that starts mid-file copies the video, so ffmpeg rewinds to the last
// keyframe — while the audio input starts exactly where it was told. Starting on a
// keyframe is what keeps the two together; starting between them leaves the sound a
// GOP ahead, permanently. This is the measurement that motivated AlignedStart.
func TestKeyframeAlignmentKeepsAudioWithVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "v.mp4")
	audio := filepath.Join(dir, "a.m4a")
	// 40s at 25fps with a 2s GOP: keyframes at 0, 2, 4 … so 21s is not one.
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

	// The alignment the packager relies on: the keyframe at or before 21s.
	prober := NewProber("ffprobe")
	key, err := prober.KeyframeAtOrBefore(context.Background(), video, nil, "", 21, 30)
	if err != nil {
		t.Fatalf("KeyframeAtOrBefore: %v", err)
	}
	if key != 20 {
		t.Fatalf("keyframe at or before 21s = %v, want 20", key)
	}

	// Starting there keeps the streams together.
	aligned := packTwoInputsAt(t, dir, "aligned", video, audio, key)
	if gap := aligned.videoStart - aligned.audioStart; gap > 0.2 || gap < -0.2 {
		t.Fatalf("starting on the keyframe left a %.3fs gap between picture and sound (v=%v a=%v)",
			gap, aligned.videoStart, aligned.audioStart)
	}

	// Starting between keyframes does not, which is why the alignment exists.
	between := packTwoInputsAt(t, dir, "between", video, audio, 21)
	if gap := between.videoStart - between.audioStart; gap > -0.5 {
		t.Fatalf("starting at 21s was expected to leave the sound about a GOP late, got a %.3fs gap (v=%v a=%v)",
			gap, between.videoStart, between.audioStart)
	}
}

type streamStarts struct {
	videoStart, audioStart float64
}

// packTwoInputsAt runs the packager's command shape for one segment and reports
// where each stream begins in the output.
func packTwoInputsAt(t *testing.T, dir, name, video, audio string, at float64) streamStarts {
	t.Helper()
	out := filepath.Join(dir, name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ss := "-ss"
	atArg := ""
	if at > 0 {
		atArg = strconv.FormatFloat(at, 'f', 3, 64)
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	if atArg != "" {
		args = append(args, ss, atArg)
	}
	args = append(args, "-i", video)
	if atArg != "" {
		args = append(args, ss, atArg)
	}
	args = append(args, "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "copy",
		"-f", "hls", "-hls_segment_type", "fmp4", "-hls_time", "4",
		"-hls_playlist_type", "event", "-hls_list_size", "0",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(out, "seg%05d.m4s"),
		filepath.Join(out, "index.m3u8"))
	cmd := exec.Command("ffmpeg", args...)
	// The init filename resolves against the process working directory, not the
	// playlist path (the same trap packager.go documents).
	cmd.Dir = out
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v (%s)", err, out)
	}

	// The first segment, prefixed with its init, is what a player reads first.
	first := filepath.Join(out, "seg00000.m4s")
	cat := filepath.Join(out, "probe.mp4")
	initBytes, err := os.ReadFile(filepath.Join(out, "init.mp4"))
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	segBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	if err := os.WriteFile(cat, append(initBytes, segBytes...), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	return streamStarts{
		videoStart: streamStartOf(t, cat, "v:0"),
		audioStart: streamStartOf(t, cat, "a:0"),
	}
}

func streamStartOf(t *testing.T, path, stream string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream,
		"-show_entries", "stream=start_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe start_time: %v", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe start_time %q: %v", out, err)
	}
	return v
}
