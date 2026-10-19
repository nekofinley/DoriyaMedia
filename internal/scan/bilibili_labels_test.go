package scan

import "testing"

// bilibili numbers its seasons in the folder name ("第一季", "第二季") or names
// them outright ("游郭篇", "SP"), and its episode files are titled rather than
// numbered "第 N 季". Two things went wrong with that shape:
//
//   - a file whose name carries a bare episode number had season 1 filled in, so
//     every season of every bilibili show came out as season 1 — and a franchise
//     season was labelled "… · 第 1 季" on top of its own name;
//   - a season folder that says which season it is kept no number, so the seasons
//     were name-only slots that could sort arbitrarily (第三季 before 第一季).
func TestBilibiliSeasonFoldersKeepTheirNumberAndName(t *testing.T) {
	root := "/热门番剧"
	cases := []struct {
		name       string
		dir        string
		file       string
		childCount int
		wantShow   string
		wantSeason int
		wantLabel  string
	}{
		{
			name: "numbered season folder", childCount: 2,
			dir:      "/热门番剧/MEGALO BOX [series 10001]/第二季 [season B]",
			file:     "/热门番剧/MEGALO BOX [series 10001]/第二季 [season B]/MEGALO BOX 第二季 第1话 Buy or Die [ep13].mp4",
			wantShow: root + "/MEGALO BOX [series 10001]", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name: "the first season is number one", childCount: 2,
			dir:      "/热门番剧/MEGALO BOX [series 10001]/第一季 [season A]",
			file:     "/热门番剧/MEGALO BOX [series 10001]/第一季 [season A]/MEGALO BOX 第一季 第1话 [ep1].mp4",
			wantShow: root + "/MEGALO BOX [series 10001]", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name: "a later season is not season one", childCount: 4,
			dir:      "/热门番剧/约会大作战 [series 10002]/第三季 [season C]",
			file:     "/热门番剧/约会大作战 [series 10002]/第三季 [season C]/约会大作战 第三季 第1话 七罪审判 [ep25].mp4",
			wantShow: root + "/约会大作战 [series 10002]", wantSeason: 3, wantLabel: "第 3 季",
		},
		{
			name: "a named season stays name-only", childCount: 4,
			dir:      "/热门番剧/鬼灭之刃 [series 26801]/游郭篇 [season 39433]",
			file:     "/热门番剧/鬼灭之刃 [series 26801]/游郭篇 [season 39433]/培育师・鳞泷左近次 [ep1].mp4",
			wantShow: root + "/鬼灭之刃 [series 26801]", wantSeason: 0, wantLabel: "游郭篇",
		},
		{
			name: "a special keeps its own name", childCount: 4,
			dir:      "/热门番剧/鬼灭之刃 [series 26801]/SP [season F]",
			file:     "/热门番剧/鬼灭之刃 [series 26801]/SP [season F]/特别篇 [ep99].mp4",
			wantShow: root + "/鬼灭之刃 [series 26801]", wantSeason: 0, wantLabel: "SP",
		},
		{
			// A single-season 番剧 uses a folder literally named "TV". Treating
			// it as the show made every such series one "TV" mega-show.
			name: "bilibili's TV folder is a season, not a show", childCount: 1,
			dir:      "/热门番剧/冰菓 [series 3398]/TV [season 3398]",
			file:     "/热门番剧/冰菓 [series 3398]/TV [season 3398]/冰菓 第1话 古典部系列 [ep1].mp4",
			wantShow: root + "/冰菓 [series 3398]", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:     "an ordinary show is unaffected",
			dir:      "/Cartoon/咒术回战/第 2 季",
			file:     "/Cartoon/咒术回战/第 2 季/第 01 话.mkv",
			wantShow: "/Cartoon/咒术回战", wantSeason: 2, wantLabel: "第 2 季",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			child := map[string]int{}
			if c.childCount > 0 {
				child[parentDir(c.dir)] = c.childCount
			}
			// The same call the scanner makes: the show directory is resolved from
			// the file's directory first, then planned.
			showDir := showDirFor(root, c.dir)
			parsed := ParseName(baseName(c.file))
			show, season, label := planShow(child, root, showDir, c.file, parsed.Season)
			if show != c.wantShow || season != c.wantSeason || label != c.wantLabel {
				t.Fatalf("planShow = (%q, %d, %q), want (%q, %d, %q)",
					show, season, label, c.wantShow, c.wantSeason, c.wantLabel)
			}
		})
	}
}

// Only an explicit season marker counts as one. A bare episode number says nothing
// about the season — filling in 1 there is what made every numbered season of every
// bilibili show look like season 1 and pushed the folder's own number out.
func TestBareEpisodeNumberDoesNotInventASeason(t *testing.T) {
	for _, name := range []string{
		"第1话 Buy or Die [ep13].mp4",
		"01.mkv",
		"Show - 07 [1080p].mkv",
		"培育师・鳞泷左近次 [ep1].mp4",
		"约会大作战 第三季 第1话 七罪审判 [ep25].mp4",
	} {
		if got := ParseName(name).Season; got != 0 {
			t.Errorf("ParseName(%q).Season = %d, want 0 (the folder decides)", name, got)
		}
	}
	// An explicit "SxxExx" still counts.
	if got := ParseName("Show S03E01.mkv").Season; got != 3 {
		t.Errorf("S03E01 → season %d, want 3", got)
	}
}
