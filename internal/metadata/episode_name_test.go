package metadata

import (
	"testing"

	"doriyamedia/internal/models"
)

// A name the scan took from the source itself beats a provider's guess. The
// guess is by episode *number*, which is how a second season came to carry the
// first season's episode titles: bilibili states every episode's own title in
// its listing, and that one is right for the season it was listed in.
func TestPlaceholderEpisodeName(t *testing.T) {
	cases := []struct {
		name string
		ep   *models.Item
		want bool
	}{
		{"empty", &models.Item{Episode: 3}, true},
		{"the scan's placeholder", &models.Item{Episode: 3, Name: "第 3 集"}, true},
		{"the old English placeholder", &models.Item{Episode: 3, Name: "Episode 3"}, true},
		{"another episode's placeholder is a real name", &models.Item{Episode: 3, Name: "第 5 集"}, false},
		{"bilibili's own label", &models.Item{Episode: 3, Name: "第3话 新国王"}, false},
		{"a section entry", &models.Item{Episode: 11, Name: "PV&其他 第3话预告 新国王"}, false},
		{"a provider's title", &models.Item{Episode: 1, Name: "结婚"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPlaceholderEpisodeName(c.ep); got != c.want {
				t.Fatalf("isPlaceholderEpisodeName(%q) = %v, want %v", c.ep.Name, got, c.want)
			}
		})
	}
}
