package api

import (
	"math"
	"net/url"
	"testing"

	"doriyamedia/internal/media"
)

func TestProbeSummaryIncludesSourceFrameRate(t *testing.T) {
	raw := `{
		"streams": [{
			"index": 0,
			"codec_type": "video",
			"codec_name": "h264",
			"width": 1920,
			"height": 1080,
			"avg_frame_rate": "24000/1001",
			"bit_rate": "4500000"
		}],
		"format": {
			"format_name": "mov,mp4,m4a,3gp,3g2,mj2",
			"duration": "1431.5",
			"bit_rate": "4800000"
		}
	}`

	got := probeSummary(raw)
	if got == nil || got.Video == nil {
		t.Fatal("probeSummary returned no video stream")
	}
	if math.Abs(got.Video.FrameRate-24000.0/1001.0) > 0.0001 {
		t.Fatalf("frame rate = %v, want %v", got.Video.FrameRate, 24000.0/1001.0)
	}
}

func TestResolveSessionOriginUsesPinnedKeyframeWithoutStart(t *testing.T) {
	aligned := false
	want := media.PackWindow{StartSeconds: 130.13, InputSeekSeconds: 125.11}
	startSeg, got := resolveSessionOrigin(0, true, want, true, 4, 1431,
		func() media.PackWindow {
			aligned = true
			return media.PackWindow{}
		})

	if startSeg != 0 || got != want {
		t.Fatalf("origin = (%d, %+v), want (0, %+v)", startSeg, got, want)
	}
	if aligned {
		t.Fatal("a pinned segment request must not re-measure the keyframe")
	}
}

func TestResolveSessionOriginAlignsInitialTwoStreamSeek(t *testing.T) {
	want := media.PackWindow{StartSeconds: 130.13, InputSeekSeconds: 125.11}
	startSeg, got := resolveSessionOrigin(131, true, media.PackWindow{}, false, 4, 1431,
		func() media.PackWindow { return want })

	if startSeg != 0 || got != want {
		t.Fatalf("origin = (%d, %+v), want (0, %+v)", startSeg, got, want)
	}
}

func TestResolveSessionOriginFallsBackToZeroWhenAlignmentFails(t *testing.T) {
	startSeg, got := resolveSessionOrigin(131, true, media.PackWindow{}, false, 4, 1431,
		func() media.PackWindow { return media.PackWindow{} })

	if startSeg != 0 || got != (media.PackWindow{}) {
		t.Fatalf("origin = (%d, %+v), want (0, zero)", startSeg, got)
	}
}

func TestResolveSessionOriginSnapsOrdinaryFileToSegment(t *testing.T) {
	startSeg, got := resolveSessionOrigin(131, false, media.PackWindow{}, false, 4, 1431, nil)
	want := media.PackWindow{StartSeconds: 128}

	if startSeg != 32 || got != want {
		t.Fatalf("origin = (%d, %+v), want (32, %+v)", startSeg, got, want)
	}
}

func TestResolveSessionOriginRejectsPinnedStartPastEnd(t *testing.T) {
	startSeg, got := resolveSessionOrigin(0, true,
		media.PackWindow{StartSeconds: 2000, InputSeekSeconds: 1990}, true, 4, 1431, nil)

	if startSeg != 0 || got != (media.PackWindow{}) {
		t.Fatalf("origin = (%d, %+v), want (0, zero)", startSeg, got)
	}
}

func TestResolveSessionOriginKeepsPinnedFailureAtZero(t *testing.T) {
	aligned := false
	startSeg, got := resolveSessionOrigin(131, true, media.PackWindow{}, true, 4, 1431,
		func() media.PackWindow {
			aligned = true
			return media.PackWindow{StartSeconds: 130.13, InputSeekSeconds: 125.11}
		})

	if startSeg != 0 || got != (media.PackWindow{}) {
		t.Fatalf("origin = (%d, %+v), want the pinned failure origin (0, zero)", startSeg, got)
	}
	if aligned {
		t.Fatal("a pinned origin must not be re-measured on a later playlist or segment request")
	}
}

func TestPinnedSessionOrigin(t *testing.T) {
	tests := []struct {
		name string
		q    url.Values
		want media.PackWindow
		ok   bool
	}{
		{
			name: "with coarse seek",
			q:    url.Values{"ss": {"130.130"}, "is": {"125.110"}},
			want: media.PackWindow{StartSeconds: 130.13, InputSeekSeconds: 125.11},
			ok:   true,
		},
		{name: "without coarse seek", q: url.Values{"ss": {"130.130"}}, want: media.PackWindow{StartSeconds: 130.13}, ok: true},
		{name: "zero failure", q: url.Values{"ss": {"0"}}, ok: true},
		{name: "missing", q: url.Values{}},
		{name: "negative", q: url.Values{"ss": {"-1"}}},
		{name: "malformed", q: url.Values{"ss": {"not-a-time"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pinnedSessionOrigin(tt.q)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("pinnedSessionOrigin = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSessionOriginQueryPinsZeroForTwoStream(t *testing.T) {
	if got := sessionOriginQuery(true, media.PackWindow{}); got != "&ss=0.000&is=0.000" {
		t.Fatalf("two-stream origin query = %q, want a pinned zero", got)
	}
	if got := sessionOriginQuery(false, media.PackWindow{StartSeconds: 130.13, InputSeekSeconds: 125.11}); got != "" {
		t.Fatalf("ordinary file origin query = %q, want empty", got)
	}
}
