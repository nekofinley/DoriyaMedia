package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A copied video stream can start at the keyframe before the requested PTS even
// when the request names a keyframe exactly. The independent audio input does not
// rewind, so a one-stage seek splits the two tracks. A coarse seek to the preceding
// keyframe plus an output-stage seek for the DTS distance lands both on the target.
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

	// The alignment the packager relies on: target DTS 19.92, with the preceding
	// keyframe at DTS 17.92 providing the coarse input seek.
	prober := NewProber("ffprobe")
	found, err := prober.keyframeWindowAtOrBefore(context.Background(), video, nil, "", 21, 30)
	if err != nil {
		t.Fatalf("keyframeWindowAtOrBefore: %v", err)
	}
	if math.Abs(found.pts-20) > 1e-6 {
		t.Fatalf("target keyframe PTS = %v, want 20", found.pts)
	}
	if math.Abs(found.start.StartSeconds-19.92) > 1e-6 ||
		math.Abs(found.start.InputSeekSeconds-17.92) > 1e-6 {
		t.Fatalf("seek window = %+v, want coarse 17.92 -> start 19.92", found.start)
	}

	// The first copied packet must be the requested keyframe, not merely close in
	// timestamp. Timestamp-only assertions miss the video-at-previous-GOP bug.
	targetHash := keyframePacketHashAtOrBefore(t, video, 21)
	aligned := packTwoInputsWindow(t, dir, "aligned", video, audio, found.start)
	if gap := aligned.videoStart - aligned.audioStart; gap > 0.2 || gap < -0.2 {
		t.Fatalf("starting on the keyframe left a %.3fs gap between picture and sound (v=%v a=%v)",
			gap, aligned.videoStart, aligned.audioStart)
	}
	if aligned.videoHash != targetHash {
		t.Fatalf("first output video packet = %s, want target keyframe %s", aligned.videoHash, targetHash)
	}

	// A one-stage seek at 21s still starts the picture on the previous keyframe
	// while the sound starts at 21s. This is the regression being prevented.
	between := packTwoInputsAt(t, dir, "between", video, audio, 21)
	if gap := between.videoStart - between.audioStart; gap > -0.5 {
		t.Fatalf("starting at 21s was expected to leave the sound about a GOP late, got a %.3fs gap (v=%v a=%v)",
			gap, between.videoStart, between.audioStart)
	}
}

type streamStarts struct {
	videoStart, audioStart float64
	videoHash              string
}

// packTwoInputsAt runs the packager's command shape for one segment and reports
// where each stream begins in the output.
func packTwoInputsAt(t *testing.T, dir, name, video, audio string, at float64) streamStarts {
	t.Helper()
	return packTwoInputs(t, dir, name, video, audio, at, 0)
}

// packTwoInputsWindow runs the two-stage seek the packager now uses.
func packTwoInputsWindow(t *testing.T, dir, name, video, audio string, window PackWindow) streamStarts {
	t.Helper()
	return packTwoInputs(t, dir, name, video, audio, window.InputSeekSeconds,
		window.StartSeconds-window.InputSeekSeconds)
}

func packTwoInputs(t *testing.T, dir, name, video, audio string, inputSeek, outputSeek float64) streamStarts {
	t.Helper()
	out := filepath.Join(dir, name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ss := "-ss"
	inputArg := ""
	if inputSeek > 0 {
		inputArg = strconv.FormatFloat(inputSeek, 'f', 3, 64)
	}
	outputArg := ""
	if outputSeek > 0 {
		outputArg = strconv.FormatFloat(outputSeek, 'f', 3, 64)
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	if inputArg != "" {
		args = append(args, ss, inputArg)
	}
	args = append(args, "-i", video)
	if inputArg != "" {
		args = append(args, ss, inputArg)
	}
	args = append(args, "-i", audio, "-map", "0:v:0", "-map", "1:a:0")
	if outputArg != "" {
		args = append(args, ss, outputArg)
	}
	args = append(args, "-c:v", "copy", "-c:a", "copy",
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
		videoHash:  firstVideoPacketHash(t, cat),
	}
}

func keyframePacketHashAtOrBefore(t *testing.T, path string, pos float64) string {
	t.Helper()
	from := math.Max(0, pos-10)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_data_hash", "md5",
		"-select_streams", "v:0", "-show_entries", "packet=pts_time,dts_time,flags,data_hash",
		"-read_intervals", fmt.Sprintf("%.3f%%+20", from), "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe source keyframes: %v", err)
	}
	var best string
	var bestPTS = -1.0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 4 || !strings.Contains(fields[2], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err == nil && pts <= pos && pts > bestPTS {
			bestPTS, best = pts, fields[3]
		}
	}
	if best == "" {
		t.Fatalf("no source keyframe at or before %.3fs", pos)
	}
	return best
}

func firstVideoPacketHash(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_data_hash", "md5",
		"-select_streams", "v:0", "-show_entries", "packet=pts_time,dts_time,flags,data_hash",
		"-read_intervals", "%+#1", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe output packet: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(fields) < 4 {
		t.Fatalf("unexpected output packet row %q", out)
	}
	return fields[3]
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
