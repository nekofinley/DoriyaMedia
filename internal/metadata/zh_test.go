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

func TestSearchSpellingsStripsRegionQualifier(t *testing.T) {
	got := searchSpellings("86－不存在的戰區－（僅限港澳台地區）")
	want := []string{
		"86－不存在的戰區－（僅限港澳台地區）",
		"86－不存在的战区－",
		"86－不存在的戰區－",
	}
	if len(got) != len(want) {
		t.Fatalf("searchSpellings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("searchSpellings[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := searchSpellings("間諜家家酒（僅限港澳台）"); len(got) != 3 ||
		got[1] != "间谍家家酒" || got[2] != "間諜家家酒" {
		t.Fatalf("searchSpellings with short qualifier = %v", got)
	}
	if got := searchSpellings("86（限港澳台地区）"); len(got) != 2 || got[1] != "86" {
		t.Fatalf("searchSpellings with simplified qualifier = %v", got)
	}
}

func TestCoreTitleOnlyStripsRegionQualifier(t *testing.T) {
	cases := map[string]string{
		"86－不存在的戰區－（僅限港澳台地區）":        "86－不存在的戰區－",
		"86－不存在的戰區－（僅限港澳台地區） (2021)": "86－不存在的戰區－（僅限港澳台地區） (2021)",
		"鬼滅之刃（第一季）":                  "鬼滅之刃（第一季）",
		"鬼滅之刃（劇場版）":                  "鬼滅之刃（劇場版）",
		"（僅限港澳台地區）":                  "（僅限港澳台地區）",
	}
	for input, want := range cases {
		if got := coreTitle(input); got != want {
			t.Errorf("coreTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormTitleFoldsTraditional(t *testing.T) {
	pairs := [][2]string{
		{"鬼滅之刃", "鬼灭之刃"},
		{"葬送的芙莉蓮！", "葬送的芙莉莲"},
		{"孤獨搖滾", "孤独摇滚"},
		{"86－不存在的戰區－", "86-不存在的战区-"},
		{"86—不存在的戰區—", "86–不存在的战区–"},
	}
	for _, p := range pairs {
		if a, b := normTitle(p[0]), normTitle(p[1]); a != b {
			t.Errorf("normTitle(%q) = %q, normTitle(%q) = %q; want equal", p[0], a, p[1], b)
		}
	}
	if got := normTitle("86－不存在的戰區－（僅限港澳台地區）"); got != "86不存在的战区" {
		t.Fatalf("normTitle with region qualifier = %q, want 86不存在的战区", got)
	}
}
