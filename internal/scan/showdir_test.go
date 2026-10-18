package scan

import "testing"

// Real-world layouts taken from the user's OpenList collection (/Cartoon).
func TestPlanShowAndSeasonMeta(t *testing.T) {
	cases := []struct {
		name       string
		libPath    string
		dir        string
		file       string
		wantShow   string
		wantSeason int
		wantLabel  string
	}{
		{
			name:     "OAD 目录不能当剧名(史莱姆)",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/关于我转生变成史莱姆这档事/OAD/bdrip/[VCB-Studio] Tensei Shitara Slime Datta Ken OAD Series [Ma10p_1080p]",
			file:     "/Cartoon/关于我转生变成史莱姆这档事/OAD/bdrip/[VCB-Studio] Tensei Shitara Slime Datta Ken OAD Series [Ma10p_1080p]/[VCB-Studio] Tensei Shitara Slime Datta Ken OAD Series [01][Ma10p_1080p][x265_flac].mkv",
			wantShow: "/Cartoon/关于我转生变成史莱姆这档事", wantSeason: 91, wantLabel: "OAD",
		},
		{
			name:     "bdrip[--cp]/0 (命运石之门 0)",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/命运石之门/bdrip[--cp]/0/[VCB-Studio] Steins;Gate 0 [Ma10p_1080p]",
			file:     "/Cartoon/命运石之门/bdrip[--cp]/0/[VCB-Studio] Steins;Gate 0 [Ma10p_1080p]/[VCB-Studio] Steins;Gate 0 [01][Ma10p_1080p][x265_flac].mkv",
			wantShow: "/Cartoon/命运石之门", wantSeason: 0, wantLabel: "第 0 季",
		},
		{
			name:     "bdrip/Season1 (咒术回战)",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/咒术回战/bdrip/Season1/[BeanSub and VCB-Studio] Jujutsu Kaisen [Ma10p_1080p]",
			file:     "/Cartoon/咒术回战/bdrip/Season1/[BeanSub and VCB-Studio] Jujutsu Kaisen [Ma10p_1080p]/[BeanSub and VCB-Studio] Jujutsu Kaisen [01][Ma10p_1080p][x265_flac].mkv",
			wantShow: "/Cartoon/咒术回战", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:     "bdrip/Season2 同剧第二季",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/咒术回战/bdrip/Season2/[BeanSub and VCB-Studio] Jujutsu Kaisen S2 [Ma10p_1080p]",
			file:     "/Cartoon/咒术回战/bdrip/Season2/[BeanSub and VCB-Studio] Jujutsu Kaisen S2 [Ma10p_1080p]/[BeanSub and VCB-Studio] Jujutsu Kaisen [25][Ma10p_1080p][x265_flac].mkv",
			wantShow: "/Cartoon/咒术回战", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name:     "webrip/10bitHEVC 无季目录(孤独摇滚)",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/孤独摇滚/webrip/10bitHEVC/[Sakurato] Bocchi the Rock! [HEVC-10bit 1080p AAC][CHS]",
			file:     "/Cartoon/孤独摇滚/webrip/10bitHEVC/[Sakurato] Bocchi the Rock! [HEVC-10bit 1080p AAC][CHS]/[Sakurato] Bocchi the Rock! [01][HEVC-10bit 1080p AAC][CHS].mkv",
			wantShow: "/Cartoon/孤独摇滚", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:     "剧名/第 2 季/bdrip/[组]",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/第 2 季/bdrip/[Group] Show [1080p]",
			file:     "/Cartoon/某番/第 2 季/bdrip/[Group] Show [1080p]/[Group] Show - 01.mkv",
			wantShow: "/Cartoon/某番", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name:     "剧名/bdrip/第 2 季",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/bdrip/第 2 季",
			file:     "/Cartoon/某番/bdrip/第 2 季/[Group] Show - 01.mkv",
			wantShow: "/Cartoon/某番", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name:     "纯数字剧名 86 不能被吃成季",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/86",
			file:     "/Cartoon/86/[Group] 86 - 01.mkv",
			wantShow: "/Cartoon/86", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:     "每集一个目录 剧名/01/file.mkv",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/01",
			file:     "/Cartoon/某番/01/[Group] Show - 01.mkv",
			wantShow: "/Cartoon/某番", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:     "中文数字季 剧名/第二季/file.mkv",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/第二季",
			file:     "/Cartoon/某番/第二季/[Group] Show - 01.mkv",
			wantShow: "/Cartoon/某番", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name:     "中文数字季带空格 剧名/第 3 季/webrip",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/第 3 季/webrip",
			file:     "/Cartoon/某番/第 3 季/webrip/[Group] Show - 01.mkv",
			wantShow: "/Cartoon/某番", wantSeason: 3, wantLabel: "第 3 季",
		},
		{
			name:     "剧场版 独立成季",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/某番/剧场版",
			file:     "/Cartoon/某番/剧场版/[Group] Show Movie [BDRip 1080p].mkv",
			wantShow: "/Cartoon/某番", wantSeason: 93, wantLabel: "剧场版",
		},
		{
			name:     "SPs 也算特别篇(86 的真实目录)",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/86-不存在的战区/bdrip/[UHA-WINGS&VCB-Studio] EIGHTY SIX [Ma10p_1080p]/SPs",
			file:     "/Cartoon/86-不存在的战区/bdrip/[UHA-WINGS&VCB-Studio] EIGHTY SIX [Ma10p_1080p]/SPs/[Group] 86 SP01.mkv",
			wantShow: "/Cartoon/86-不存在的战区", wantSeason: 94, wantLabel: "特别篇",
		},
		{
			name:     "语言目录 简体 不能当剧名",
			libPath:  "/Cartoon",
			dir:      "/Cartoon/跃动青春/webrip/简体",
			file:     "/Cartoon/跃动青春/webrip/简体/[MingY] Skip to Loafer [01][1080p][CHS&JPN].mp4",
			wantShow: "/Cartoon/跃动青春", wantSeason: 1, wantLabel: "第 1 季",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// nil childCount → no franchise promotion, i.e. plain path layout
			show, season, label := planShow(nil, c.libPath, c.dir, c.file, 0)
			if show != c.wantShow {
				t.Fatalf("show = %q, want %q", show, c.wantShow)
			}
			if season != c.wantSeason || label != c.wantLabel {
				t.Fatalf("season = (%d,%q), want (%d,%q)", season, label, c.wantSeason, c.wantLabel)
			}
		})
	}
}

// A season number parsed from the FILE NAME must win, and season 0 coming from
// a "0" directory must not be folded back into season 1.
func TestPlanShowPrecedence(t *testing.T) {
	if _, n, l := planShow(nil, "/Cartoon", "/Cartoon/某番/bdrip", "/Cartoon/某番/bdrip/x.mkv", 3); n != 3 || l != "第 3 季" {
		t.Fatalf("file-name season should win: got (%d,%q)", n, l)
	}
	dir0 := "/Cartoon/命运石之门/bdrip[--cp]/0/[VCB-Studio] Steins;Gate 0 [Ma10p_1080p]"
	file0 := dir0 + "/[VCB-Studio] Steins;Gate 0 [01][Ma10p_1080p][x265_flac].mkv"
	if _, n, l := planShow(nil, "/Cartoon", dir0, file0, 0); n != 0 || l != "第 0 季" {
		t.Fatalf("explicit 0 dir must stay season 0: got (%d,%q)", n, l)
	}
	dir1 := "/Cartoon/命运石之门/bdrip[--cp]/2012.05.12.Steins;Gate.2011.Blu-ray.x264-10bit.1080P.FLAC.MySilu"
	file1 := dir1 + "/Steins;Gate 2011 EP01 [BluRay 1920x1080p].mkv"
	if _, n, l := planShow(nil, "/Cartoon", dir1, file1, 0); n != 1 || l != "第 1 季" {
		t.Fatalf("no season dir → season 1: got (%d,%q)", n, l)
	}
}

// The user's real duplicates: 物语系列/{化物语,伪物语} and
// 青之驱魔师/{Season1,岛根启明结社篇,京都不淨王篇} are ONE show with several
// seasons — the franchise folder is the show, each child is its own season.
func TestFranchisePromotion(t *testing.T) {
	cases := []struct {
		name       string
		libPath    string
		dir        string
		file       string
		parsed     int
		childCount map[string]int
		wantShow   string
		wantSeason int
		wantLabel  string
	}{
		{
			name:       "物语系列/化物语 → 剧=物语系列, 季=化物语",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/物语系列/化物语/bdrip[--cp]/[VCB-S]Bakemonogatari[1080p]",
			file:       "/public/Cartoon/物语系列/化物语/bdrip[--cp]/[VCB-S]Bakemonogatari[1080p]/[VCB-S]Bakemonogatari[01][Hi10p_1080p][BDRip][x264_2flac].mkv",
			childCount: map[string]int{"/public/Cartoon/物语系列": 2},
			wantShow:   "/public/Cartoon/物语系列", wantSeason: 0, wantLabel: "化物语",
		},
		{
			name:       "物语系列/伪物语 → 同一部剧的另一个季",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/物语系列/伪物语/bdrip[--cp]/[VCB-S]Nisemonogatari[1080p]",
			file:       "/public/Cartoon/物语系列/伪物语/bdrip[--cp]/[VCB-S]Nisemonogatari[1080p]/[VCB-S]Nisemonogatari[01][Hi10p_1080p][BDRip][x264_3flac].mkv",
			childCount: map[string]int{"/public/Cartoon/物语系列": 2},
			wantShow:   "/public/Cartoon/物语系列", wantSeason: 0, wantLabel: "伪物语",
		},
		{
			name:       "青之驱魔师/岛根启明结社篇 → 剧=青之驱魔师",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/青之驱魔师/岛根启明结社篇/webrip/[BeanSub and LoliHouse] Ao no Exorcist Shimane Illuminati-hen [WebRip 1080p HEVC-10bit AAC ASSx2]",
			file:       "/public/Cartoon/青之驱魔师/岛根启明结社篇/webrip/[BeanSub and LoliHouse] Ao no Exorcist Shimane Illuminati-hen [WebRip 1080p HEVC-10bit AAC ASSx2]/Ao no Exorcist Shimane Illuminati-hen - 01 [WebRip 1080p HEVC-10bit AAC ASSx2].mkv",
			childCount: map[string]int{"/public/Cartoon/青之驱魔师": 3},
			wantShow:   "/public/Cartoon/青之驱魔师", wantSeason: 0, wantLabel: "岛根启明结社篇",
		},
		{
			name:       "子系列里还有季目录 → 标签合并",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/Fate/Zero/第 2 季/[Group]",
			file:       "/public/Cartoon/Fate/Zero/第 2 季/[Group]/Fate Zero - 01.mkv",
			childCount: map[string]int{"/public/Cartoon/Fate": 2},
			wantShow:   "/public/Cartoon/Fate", wantSeason: 0, wantLabel: "Zero · 第 2 季",
		},
		{
			name:       "子系列里文件名带季 → 标签合并",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/物语系列/化物语/bdrip",
			file:       "/public/Cartoon/物语系列/化物语/bdrip/[Group] Bakemonogatari S03E01.mkv",
			parsed:     3,
			childCount: map[string]int{"/public/Cartoon/物语系列": 2},
			wantShow:   "/public/Cartoon/物语系列", wantSeason: 0, wantLabel: "化物语 · 第 3 季",
		},
		{
			name:       "只有 1 个像剧的子目录 → 不提升",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/某番/外传",
			file:       "/public/Cartoon/某番/外传/Show - 01.mkv",
			childCount: map[string]int{"/public/Cartoon/某番": 1},
			wantShow:   "/public/Cartoon/某番/外传", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:       "junk 目录 Extra 依旧跳过",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/某番/Extra",
			file:       "/public/Cartoon/某番/Extra/Show - 01.mkv",
			childCount: map[string]int{"/public/Cartoon/某番": 1},
			wantShow:   "/public/Cartoon/某番", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:       "分类目录 Movie 不能当季节标记,也不能当剧名",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/Movie/你的名字",
			file:       "/public/Cartoon/Movie/你的名字/Your Name [BDRip].mkv",
			childCount: map[string]int{"/public/Cartoon/Movie": 9},
			wantShow:   "/public/Cartoon/Movie/你的名字", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:       "文件直接在 Movie 下 → 没有剧名(交给文件名)",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/Movie",
			file:       "/public/Cartoon/Movie/Some Movie [BDRip].mkv",
			childCount: map[string]int{"/public/Cartoon/Movie": 9},
			wantShow:   "", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:       "库根目录不能当剧名",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/某番/bdrip",
			file:       "/public/Cartoon/某番/bdrip/Show - 01.mkv",
			childCount: map[string]int{"/public/Cartoon": 60},
			wantShow:   "/public/Cartoon/某番", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			name:       "带季目录的子目录本来就走季逻辑(青之驱魔师/Season1)",
			libPath:    "/public/Cartoon",
			dir:        "/public/Cartoon/青之驱魔师/Season1/bdrip/[FLsnow]",
			file:       "/public/Cartoon/青之驱魔师/Season1/bdrip/[FLsnow]/[FLsnow][Ao_no_Exorcist][01][BDRIP][1080p][AVC_AAC].mkv",
			childCount: map[string]int{"/public/Cartoon/青之驱魔师": 3},
			wantShow:   "/public/Cartoon/青之驱魔师", wantSeason: 1, wantLabel: "第 1 季",
		},
		{
			// B 站的番剧树:系列目录里挂着这部剧的每一个季度。方括号里是路径用
			// 的 id,既要能反查出下一层,又不能出现在界面上。
			name:       "bilibili 系列/季度:季度名不带方括号标记",
			libPath:    "/热门番剧",
			dir:        "/热门番剧/鬼灭之刃 [series 26801]/游郭篇 [season 39433]",
			file:       "/热门番剧/鬼灭之刃 [series 26801]/游郭篇 [season 39433]/鬼灭之刃 第1话 残酷 [ep267851].mp4",
			childCount: map[string]int{"/热门番剧/鬼灭之刃 [series 26801]": 15},
			wantShow:   "/热门番剧/鬼灭之刃 [series 26801]", wantSeason: 0, wantLabel: "游郭篇",
		},
		{
			name:       "bilibili 季度自己就叫第一季/第二季",
			libPath:    "/热门番剧",
			dir:        "/热门番剧/间谍过家家 [series 41410]/第二季 [season 46085]",
			file:       "/热门番剧/间谍过家家 [series 41410]/第二季 [season 46085]/间谍过家家 第1话 [ep1].mp4",
			childCount: map[string]int{"/热门番剧/间谍过家家 [series 41410]": 7},
			wantShow:   "/热门番剧/间谍过家家 [series 41410]", wantSeason: 2, wantLabel: "第 2 季",
		},
		{
			name:       "bilibili 只有一季的番:季度目录自己当剧",
			libPath:    "/热门番剧",
			dir:        "/热门番剧/冰菓 [series 3398]/冰菓 [season 3398]",
			file:       "/热门番剧/冰菓 [series 3398]/冰菓 [season 3398]/冰菓 第1话 古典部系列 [ep1].mp4",
			childCount: map[string]int{"/热门番剧/冰菓 [series 3398]": 1},
			wantShow:   "/热门番剧/冰菓 [series 3398]/冰菓 [season 3398]", wantSeason: 1, wantLabel: "第 1 季",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			show, season, label := planShow(c.childCount, c.libPath, c.dir, c.file, c.parsed)
			if show != c.wantShow {
				t.Fatalf("show = %q, want %q", show, c.wantShow)
			}
			if season != c.wantSeason || label != c.wantLabel {
				t.Fatalf("season = (%d,%q), want (%d,%q)", season, label, c.wantSeason, c.wantLabel)
			}
		})
	}
}

func TestIsCategoryDir(t *testing.T) {
	for _, s := range []string{"Movie", "movies", "Anime", "合集", "剧场版", "未分类", "其他", "TV"} {
		if !IsCategoryDir(s) {
			t.Fatalf("IsCategoryDir(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"物语系列", "青之驱魔师", "孤独摇滚", "86", "Fate", "你的名字"} {
		if IsCategoryDir(s) {
			t.Fatalf("IsCategoryDir(%q) = true, want false", s)
		}
	}
}

// dirChildCount must see sibling show-like folders under the same parent.
func TestDirChildCount(t *testing.T) {
	files := []fileRec{
		{Dir: "/public/Cartoon/物语系列/化物语/bdrip/[VCB-S]"},
		{Dir: "/public/Cartoon/物语系列/化物语/bdrip/[VCB-S]"},
		{Dir: "/public/Cartoon/物语系列/伪物语/bdrip/[VCB-S]"},
		{Dir: "/public/Cartoon/孤独摇滚/webrip/10bitHEVC"},
	}
	got := dirChildCount(files)
	if got["/public/Cartoon/物语系列"] != 2 {
		t.Fatalf("物语系列 children = %d, want 2", got["/public/Cartoon/物语系列"])
	}
	if got["/public/Cartoon"] != 2 {
		t.Fatalf("/public/Cartoon children = %d, want 2", got["/public/Cartoon"])
	}
	if got["/public/Cartoon/物语系列/化物语"] != 1 {
		t.Fatalf("化物语 children = %d, want 1", got["/public/Cartoon/物语系列/化物语"])
	}
}
