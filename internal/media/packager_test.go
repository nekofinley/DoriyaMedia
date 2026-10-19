package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMKV = "../../testdata/movies/MKV EAC3 Movie 2022/MKV EAC3 Movie 2022 (2022).mkv"

// tfdtValues extracts every baseMediaDecodeTime from a fragmented MP4 segment.
// HLS fMP4 requires these to advance with the media timeline; ffmpeg rebases
// them to zero for each independently started process, which is exactly why
// packaging needs one long-lived process.
func tfdtValues(data []byte) []uint64 {
	var out []uint64
	for i := 4; i+8 < len(data); i++ {
		if data[i] != 't' || data[i+1] != 'f' || data[i+2] != 'd' || data[i+3] != 't' {
			continue
		}
		if data[i+4] == 1 {
			if i+16 > len(data) {
				break
			}
			var v uint64
			for k := 0; k < 8; k++ {
				v = v<<8 | uint64(data[i+8+k])
			}
			out = append(out, v)
			i += 12
			continue
		}
		if i+12 > len(data) {
			break
		}
		var v uint64
		for k := 0; k < 4; k++ {
			v = v<<8 | uint64(data[i+8+k])
		}
		out = append(out, v)
		i += 8
	}
	return out
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := os.Stat(testMKV); err != nil {
		t.Skipf("test source missing: %v", err)
	}
}

// The whole point of the packager: a correct fMP4 HLS init segment Apple will
// accept, plus a continuous media timeline — neither of which the per-segment
// on-demand path can produce.
func TestPackagerProducesAppleValidFMP4(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := StartPackager(ctx, PackagerOptions{
		Target:       testMKV,
		VideoIndex:   0,
		AudioOrdinal: 0,
		VideoMode:    "copy",
		AudioMode:    "aac",
		VideoCodec:   "hevc",
		Dir:          dir,
	})
	if err != nil {
		t.Fatalf("StartPackager: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("packaging failed: %v", err)
	}

	pl, err := p.Playlist()
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	m3u8 := string(pl)
	for _, want := range []string{"#EXT-X-VERSION:7", `#EXT-X-MAP:URI="init.mp4"`, "#EXT-X-ENDLIST"} {
		if !strings.Contains(m3u8, want) {
			t.Fatalf("playlist missing %q:\n%s", want, m3u8)
		}
	}

	initBytes, err := os.ReadFile(p.InitPath())
	if err != nil {
		t.Fatalf("read init segment: %v", err)
	}
	// ffmpeg writes `hev1` by default and Apple refuses to play that.
	if !strings.Contains(string(initBytes), "hvc1") {
		t.Error("init segment does not carry the hvc1 sample entry Apple requires")
	}
	if strings.Contains(string(initBytes), "hev1") {
		t.Error("init segment still carries hev1")
	}

	// Segments must advance on one continuous timeline.
	segs, err := filepath.Glob(filepath.Join(dir, "seg*.m4s"))
	if err != nil || len(segs) < 2 {
		t.Fatalf("expected at least 2 segments, got %v (err=%v)", segs, err)
	}
	first := readFileT(t, segs[0])
	last := readFileT(t, segs[len(segs)-1])
	firstTfdt := tfdtValues(first)
	lastTfdt := tfdtValues(last)
	if len(firstTfdt) == 0 || len(lastTfdt) == 0 {
		t.Fatal("segments carry no tfdt box")
	}
	if firstTfdt[0] != 0 {
		t.Errorf("first segment tfdt = %d, want 0", firstTfdt[0])
	}
	if lastTfdt[0] <= firstTfdt[0] {
		t.Errorf("tfdt did not advance: first=%v last=%v", firstTfdt, lastTfdt)
	}

	// Init + a later segment must actually decode.
	comb := filepath.Join(dir, "combined.mp4")
	if err := os.WriteFile(comb, append(append([]byte(nil), initBytes...), last...), 0o644); err != nil {
		t.Fatalf("write combined: %v", err)
	}
	decodeCtx, decodeCancel := context.WithTimeout(context.Background(), time.Minute)
	defer decodeCancel()
	cmd := exec.CommandContext(decodeCtx, "ffmpeg", "-v", "error", "-i", comb, "-frames:v", "1", "-f", "null", "-")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init+last segment does not decode: %v: %s", err, out)
	}
}

func readFileT(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// The output directory must exist, otherwise ffmpeg would fail with a confusing
// error deep inside the process.
func TestPackagerRejectsMissingOutputDir(t *testing.T) {
	_, err := StartPackager(context.Background(), PackagerOptions{
		Target: testMKV, VideoMode: "copy", Dir: filepath.Join(t.TempDir(), "nope"),
	})
	if err == nil {
		t.Fatal("want an error for a missing output directory")
	}
	if !strings.Contains(err.Error(), "output directory") {
		t.Fatalf("error should name the problem, got: %v", err)
	}
}

// The hvc1 tag must be applied to HEVC copies (Apple rejects hev1) and must not
// be applied to anything else.
func TestPackagerArgsCodecTag(t *testing.T) {
	base := PackagerOptions{Target: "/tmp/x.mkv", VideoIndex: 0, AudioOrdinal: 0, VideoMode: "copy", Dir: "/tmp/out"}

	hevc := base
	hevc.VideoCodec = "hevc"
	args, err := packagerArgs(hevc)
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	if !containsPair(args, "-tag:v", "hvc1") {
		t.Fatalf("HEVC copy must set -tag:v hvc1, got %v", args)
	}

	h264 := base
	h264.VideoCodec = "h264"
	args, err = packagerArgs(h264)
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	if containsPair(args, "-tag:v", "hvc1") {
		t.Fatalf("H.264 copy must not set the HEVC tag, got %v", args)
	}

	// A transcode produces H.264, so the source codec must not leak a tag.
	tc := base
	tc.VideoMode, tc.VideoCodec = "transcode", "hevc"
	args, err = packagerArgs(tc)
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	if containsPair(args, "-tag:v", "hvc1") {
		t.Fatalf("transcode must not set the HEVC tag, got %v", args)
	}
	if !containsPair(args, "-c:v", "libx264") {
		t.Fatalf("transcode must encode H.264, got %v", args)
	}
}

// Segments must be renamed into place, not written where they are served from.
// Measured without this flag: a segment was observable at 0 bytes growing to
// 14 MB, and a player handed that reports a media error instead of retrying.
func TestPackagerArgsWriteSegmentsAtomically(t *testing.T) {
	args, err := packagerArgs(PackagerOptions{
		Target: "/tmp/x.mkv", VideoIndex: 0, AudioOrdinal: 0,
		VideoMode: "copy", Dir: "/tmp/out",
	})
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	flags := ""
	for i, a := range args {
		if a == "-hls_flags" && i+1 < len(args) {
			flags = args[i+1]
		}
	}
	if !strings.Contains(flags, "temp_file") {
		t.Fatalf("-hls_flags must include temp_file, got %q (args: %v)", flags, args)
	}
	// A playlist that grows as it is packaged is what lets playback start early;
	// dropping it to get atomic writes would trade one bug for a worse one.
	if !containsPair(args, "-hls_playlist_type", "event") {
		t.Fatalf("the playlist must stay an event playlist, got %v", args)
	}
}

func containsPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}
