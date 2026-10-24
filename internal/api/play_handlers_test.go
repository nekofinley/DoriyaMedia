package api

import (
	"net/url"
	"testing"
)

func TestResolveSessionOriginUsesPinnedKeyframeWithoutStart(t *testing.T) {
	aligned := false
	startSeg, sessionStart := resolveSessionOrigin(0, true, 130.13, true, 4, 1431,
		func() float64 {
			aligned = true
			return 0
		})

	if startSeg != 0 || sessionStart != 130.13 {
		t.Fatalf("origin = (%d, %v), want (0, 130.13)", startSeg, sessionStart)
	}
	if aligned {
		t.Fatal("a pinned segment request must not re-measure the keyframe")
	}
}

func TestResolveSessionOriginAlignsInitialTwoStreamSeek(t *testing.T) {
	startSeg, sessionStart := resolveSessionOrigin(131, true, 0, false, 4, 1431,
		func() float64 { return 130.13 })

	if startSeg != 0 || sessionStart != 130.13 {
		t.Fatalf("origin = (%d, %v), want (0, 130.13)", startSeg, sessionStart)
	}
}

func TestResolveSessionOriginFallsBackToZeroWhenAlignmentFails(t *testing.T) {
	startSeg, sessionStart := resolveSessionOrigin(131, true, 0, false, 4, 1431,
		func() float64 { return 0 })

	if startSeg != 0 || sessionStart != 0 {
		t.Fatalf("origin = (%d, %v), want (0, 0)", startSeg, sessionStart)
	}
}

func TestResolveSessionOriginSnapsOrdinaryFileToSegment(t *testing.T) {
	startSeg, sessionStart := resolveSessionOrigin(131, false, 0, false, 4, 1431, nil)

	if startSeg != 32 || sessionStart != 128 {
		t.Fatalf("origin = (%d, %v), want (32, 128)", startSeg, sessionStart)
	}
}

func TestResolveSessionOriginRejectsPinnedStartPastEnd(t *testing.T) {
	startSeg, sessionStart := resolveSessionOrigin(0, true, 2000, true, 4, 1431, nil)

	if startSeg != 0 || sessionStart != 0 {
		t.Fatalf("origin = (%d, %v), want (0, 0)", startSeg, sessionStart)
	}
}

func TestResolveSessionOriginKeepsPinnedFailureAtZero(t *testing.T) {
	aligned := false
	startSeg, sessionStart := resolveSessionOrigin(131, true, 0, true, 4, 1431,
		func() float64 {
			aligned = true
			return 130.13
		})

	if startSeg != 0 || sessionStart != 0 {
		t.Fatalf("origin = (%d, %v), want the pinned failure origin (0, 0)", startSeg, sessionStart)
	}
	if aligned {
		t.Fatal("a pinned origin must not be re-measured on a later playlist or segment request")
	}
}

func TestPinnedSessionOrigin(t *testing.T) {
	tests := []struct {
		name string
		q    url.Values
		want float64
		ok   bool
	}{
		{name: "without start", q: url.Values{"ss": {"130.130"}}, want: 130.13, ok: true},
		{name: "zero failure", q: url.Values{"ss": {"0"}}, want: 0, ok: true},
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
	if got := sessionOriginQuery(true, 0); got != "&ss=0.000" {
		t.Fatalf("two-stream origin query = %q, want a pinned zero", got)
	}
	if got := sessionOriginQuery(false, 130.13); got != "" {
		t.Fatalf("ordinary file origin query = %q, want empty", got)
	}
}
