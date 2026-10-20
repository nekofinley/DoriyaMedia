package metadata

import "testing"

// A 港澳台 title arrives in Traditional Chinese. The providers index the
// Simplified spelling, so both have to be asked for — and asking for the
// original twice is just a wasted request.
func TestSearchSpellings(t *testing.T) {
	got := searchSpellings("鬼滅之刃")
	if len(got) != 2 || got[0] != "鬼滅之刃" || got[1] != "鬼灭之刃" {
		t.Fatalf("searchSpellings(鬼滅之刃) = %v, want [鬼滅之刃 鬼灭之刃]", got)
	}
	if got := searchSpellings("鬼灭之刃"); len(got) != 1 || got[0] != "鬼灭之刃" {
		t.Fatalf("searchSpellings(simplified) = %v, want the title alone", got)
	}
	if got := searchSpellings("SPY×FAMILY"); len(got) != 1 || got[0] != "SPY×FAMILY" {
		t.Fatalf("searchSpellings(ASCII) = %v, want the title alone", got)
	}
}

func TestNormTitleFoldsTraditional(t *testing.T) {
	pairs := [][2]string{
		{"鬼滅之刃", "鬼灭之刃"},
		{"葬送的芙莉蓮！", "葬送的芙莉莲"},
		{"孤獨搖滾", "孤独摇滚"},
	}
	for _, p := range pairs {
		if a, b := normTitle(p[0]), normTitle(p[1]); a != b {
			t.Errorf("normTitle(%q) = %q, normTitle(%q) = %q; want equal", p[0], a, p[1], b)
		}
	}
}
