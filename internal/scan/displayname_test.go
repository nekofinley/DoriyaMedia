package scan

import "testing"

// A source may put an id in a directory name to make a virtual tree resolvable
// from a path alone. None of it belongs on a season card.
func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"游郭篇 [season 39433]":             "游郭篇",
		"鬼灭之刃 [series 26801]":            "鬼灭之刃",
		"第一季(中配) [season 43622]":         "第一季(中配)",
		"[VCB-S] Bakemonogatari [1080p]": "Bakemonogatari",
		"化物语":                            "化物语",
		"[1080p]":                        "[1080p]", // nothing would be left: keep it
	}
	for in, want := range cases {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
