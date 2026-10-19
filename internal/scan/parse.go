package scan

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Parsed is the filename identification result.
type Parsed struct {
	Kind      string // movie | episode
	Title     string // cleaned movie title, or show title for episodes
	Year      int
	Season    int
	Episode   int
	HasYear   bool
	IsEpisode bool
}

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".ts": true, ".m2ts": true,
	".mts": true, ".avi": true, ".wmv": true, ".flv": true, ".webm": true, ".mpg": true,
	".mpeg": true, ".vob": true, ".rmvb": true, ".rm": true, ".strm": true, ".tp": true,
}

func IsVideo(name string) bool {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return false
	}
	return videoExts[strings.ToLower(name[i:])]
}

var skipWords = []string{"sample", "trailer", "recycle", "$eaDir", "@eaDir", "lost+found", "#recycle", ".!qB", "_ths"}

func SkipEntry(name string) bool {
	low := strings.ToLower(name)
	for _, w := range skipWords {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

var (
	reYear          = regexp.MustCompile(`(?:^|[\s\.\(_\-])((?:19|20)\d{2})(?:[\s\.\)\_\-]|$)`)
	reSE            = regexp.MustCompile(`(?i)\bs(\d{1,2})[\._ ]?e(\d{1,3})(?:[\._ -]?e(\d{1,3}))?`)
	reSeasonOnly    = regexp.MustCompile(`(?i)^(?:season|series)[\._ ]?(\d{1,2})|^s(\d{1,2})(?:[\s\._\-]|$)|^第\s*([0-9]{1,2}|[一二三四五六七八九十]{1,3})\s*[季部]`)
	reCJKep         = regexp.MustCompile(`第\s*(\d{1,4})\s*[集话話期]`)
	reEP            = regexp.MustCompile(`(?:^|[\s\._\-\[])(?:EP|E)\.?\s*(\d{1,4})(?:[\s\.\_\-\]]|$)`)
	reBracketEp     = regexp.MustCompile(`[\[【]\s*(\d{1,4}(?:\.\d)?(?:v\d)?)\s*[\]】]`)
	reDashEp        = regexp.MustCompile(`(?:^|[\s_])-[\s_]*(\d{1,4})(?:[\s_\-\[]|$)`)
	rePureNum       = regexp.MustCompile(`^(\d{1,4})(?:v\d)?$`)
	reBracketGroups = regexp.MustCompile(`[\[【]([^\]】]*)[\]】]`)
)

// The leading part of a bracket that carries a label. Written as two patterns
// rather than one so it is unambiguous which text is the number: either the
// number with its version glued on ("12v2"), or the plain number, leaving " v2"
// to be read as the version it is.
var (
	reBracketEpVersion = regexp.MustCompile(`(?i)^(\d{1,4}(?:\.\d)?)v\d+`)
	reBracketEpPlain   = regexp.MustCompile(`^(\d{1,4}(?:\.\d)?)`)
)

// bracketEpisode reads an episode number out of a bracket whose contents are not
// just the number, so that a release marking its finale or a version of an
// episode ("[13 END]", "[01 (No Credit Ver.)]") is still identifiable as that
// episode. Returns false when the bracket is something else entirely.
//
// What follows the number has to be a label a release actually writes. Anything
// else is only *starting* with digits — a resolution ("[1080p]"), a 10-bit
// profile ("[Hi10p]"), a checksum — and reading an episode out of those would
// attach subtitles to the wrong file, which is worse than attaching none.
func bracketEpisode(val string) (int, bool) {
	v := strings.TrimSpace(val)
	if v == "" || v[0] < '0' || v[0] > '9' {
		return 0, false
	}
	m := reBracketEpVersion.FindStringSubmatch(v)
	if m == nil {
		m = reBracketEpPlain.FindStringSubmatch(v)
	}
	if m == nil {
		return 0, false
	}
	label := strings.TrimSpace(v[len(m[0]):])
	// A "v2" written apart from the number is a version, not a label.
	if mm := reVersionOnly.FindStringSubmatch(label); mm != nil {
		label = strings.TrimSpace(label[len(mm[0]):])
	}
	if label != "" && !episodeLabel(label) {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n < 0 || n > 9999 {
		return 0, false
	}
	return int(n), true
}

var reVersionOnly = regexp.MustCompile(`(?i)^v\d+`)

// bracketEpLabels are the words a release puts beside an episode number to say
// which episode it is — the finale, an alternative cut, a special — including
// the words that appear inside a parenthesised note ("No Credit Ver.").
var bracketEpLabels = map[string]bool{
	// which episode
	"end": true, "final": true, "finale": true, "fin": true, "last": true,
	"complete": true, "v2": true, "v3": true,
	// alternative cuts and versions
	"nc": true, "nced": true, "ncop": true, "no": true, "credit": true,
	"credits": true, "ver": true, "version": true, "clean": true, "alt": true,
	"alternative": true, "uncensored": true, "uncut": true, "remastered": true,
	"repack": true, "proper": true, "extended": true, "director": true, "cut": true,
	// specials
	"sp": true, "special": true, "ova": true, "oad": true, "ona": true,
	"movie": true, "recap": true, "digest": true, "preview": true,
	"opening": true, "ending": true, "title": true, "textless": true,
}

// episodeLabel reports whether the text after a bracket's episode number reads
// as one of those labels. Two shapes are accepted: a known word on its own
// ("END"), or such a word followed by a parenthesised note ("No Credit Ver.").
func episodeLabel(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// Split off a trailing "v2".
	if m := reVersionOnly.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		s = strings.TrimSpace(s[len(m[0]):])
	}
	if s == "" {
		return true
	}
	low := strings.ToLower(s)
	if bracketEpLabels[low] {
		return true
	}
	// "(No Credit Ver.)" and friends: an opening bracket whose content is words.
	if !strings.HasPrefix(low, "(") && !strings.HasPrefix(low, "（") {
		return false
	}
	inner := strings.Trim(low, "()（） \t")
	if inner == "" || strings.ContainsAny(inner, "0123456789") {
		return false
	}
	for _, w := range strings.Fields(inner) {
		if !bracketEpLabels[strings.Trim(w, ".,")] {
			return false
		}
	}
	return true
}

var qualityTokens = regexp.MustCompile(`(?i)\b(2160p|1080p|1080i|720p|576p|480p|4k|uhd|hdr10?|hdr10\+|dv|dovi|web[\.\-_ ]?(dl|rip)|brrip|blu[\.\-_ ]?ray|bdrip|remux|hdtv|dvdrip|hddvd|hevc|h[\.\-_ ]?26[45]|x26[45]|avc|10bit|8bit|60fps|30fps|23\.976|25fps|atmos|ddp?[\.\-_ ]?5\.1|eac3|ac3|dts[\.\-_ ]?(hd|ma|x)?|truehd|flac|aac|mp3|opus|chs|cht|chscj|big5|gb|mandarin|cantonese|chinese|english|japanese|dual|multi|sub|chs&eng|hsbs|sbs|3d|proper|repack|extended|uncut|remastered|unrated|internal|limited|imax|complete|hybrid|yify|rarbg|xvid|diy|mytvsuper|tvb|now|hbo|disney\+|netflix|nf|amzn|atvp|dsnp|hulu|max|pcm|truestem|web|dl|rip|bluray|uhdbluray)\b`)

var cjkTokens = regexp.MustCompile(`(国语|普通话|粤语|中字|简体|繁体|简繁|内封|外挂|内嵌|双语|双音轨|无字幕|杜比视界|杜比全景声|全景声|高清|超清|蓝光|原盘|修复版|完整版|抢先版|枪版|官方中字|中英双字|去水印|高码|高码率|低码|片源|压制|收录|合集|无水印|4K|2160P|1080P|720P)`)

// ParseName identifies a media filename.
func ParseName(filename string) Parsed {
	base := filename
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[:i]
	}
	original := base

	p := Parsed{}

	// anime style: [Group][Show][01][tags]
	if strings.HasPrefix(strings.TrimSpace(base), "[") || strings.HasPrefix(strings.TrimSpace(base), "【") {
		groups := reBracketGroups.FindAllStringSubmatch(base, -1)
		rest := reBracketGroups.ReplaceAllString(base, " ")
		epFound := false
		for _, g := range groups {
			val := strings.TrimSpace(g[1])
			if epRe := regexp.MustCompile(`^\d{1,4}(?:\.\d)?(?:v\d)?$`); epRe.MatchString(val) {
				if n, err := strconv.ParseFloat(strings.TrimSuffix(val, "v2"), 64); err == nil && n <= 9999 {
					p.Episode = int(n)
					p.IsEpisode = true
					p.Kind = "episode"
					epFound = true
				}
			} else if n, ok := bracketEpisode(val); ok {
				// The episode number with a label after it — "[13 END]",
				// "[01 (No Credit Ver.)]" — which is how a release marks its
				// last episode, or a version of one. The number is what names
				// the file either way.
				p.Episode = n
				p.IsEpisode = true
				p.Kind = "episode"
				epFound = true
			}
		}
		if !epFound {
			if m := reSE.FindStringSubmatch(rest); m != nil {
				applySE(&p, m)
			} else if m := reCJKep.FindStringSubmatch(rest); m != nil {
				applyNum(&p, m[1])
			}
		}
		title := cleanTitle(rest)
		if p.IsEpisode {
			p.Title = title
		} else {
			title, year := extractYear(title)
			p.Title = title
			p.Year = year
			p.HasYear = year > 0
			p.Kind = "movie"
		}
		if p.Title == "" {
			p.Title = cleanTitle(original)
		}
		return p
	}

	// S01E02
	if m := reSE.FindStringSubmatch(base); m != nil {
		applySE(&p, m)
		loc := reSE.FindStringIndex(base)
		show := stripTitleYear(base[:loc[0]])
		if show == "" {
			show = stripTitleYear(base[loc[1]:])
		}
		p.Title = show
		return p
	}

	// 第xx集
	if m := reCJKep.FindStringSubmatch(base); m != nil {
		applyNum(&p, m[1])
		loc := reCJKep.FindStringIndex(base)
		left := stripTitleYear(base[:loc[0]])
		right := stripTitleYear(base[loc[1]:])
		p.Title = pickLonger(left, right)
		return p
	}

	// EP01 / E01
	if m := reEP.FindStringSubmatch(base); m != nil {
		applyNum(&p, m[1])
		loc := reEP.FindStringIndex(base)
		show := stripTitleYear(base[:loc[0]])
		if show == "" {
			show = stripTitleYear(base[loc[1]:])
		}
		p.Title = show
		return p
	}

	// trailing bracketed number [01]
	locs := reBracketEp.FindAllStringSubmatchIndex(base, -1)
	if len(locs) > 0 {
		m := locs[len(locs)-1]
		val := base[m[2]:m[3]]
		if n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(val, "v2"), "v3"), 64); err == nil && n <= 9999 {
			p.Episode = int(n)
			p.IsEpisode = true
			p.Kind = "episode"
			p.Title = cleanTitle(base[:m[0]])
			if p.Title == "" {
				p.Title = cleanTitle(original)
			}
			return p
		}
	}

	// "Show - 01" pattern
	if m := reDashEp.FindStringSubmatch(base); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n <= 999 {
			p.Episode = n
			p.IsEpisode = true
			p.Kind = "episode"
			loc := reDashEp.FindStringIndex(base)
			p.Title = cleanTitle(base[:loc[0]])
			if p.Title == "" {
				p.Title = cleanTitle(original)
			}
			return p
		}
	}

	// bare number: "01.mkv" / "12v2.mkv" (episode inside a show folder)
	if m := rePureNum.FindStringSubmatch(base); m != nil {
		applyNum(&p, m[1])
		return p // Title left empty — callers fall back to the directory name
	}

	// movie
	title, year := extractYear(original)
	if year > 0 {
		p.HasYear = true
		p.Year = year
	}
	p.Title = title
	if p.Title == "" {
		p.Title = cleanTitle(original)
	}
	p.Kind = "movie"
	return p
}

func applySE(p *Parsed, m []string) {
	s, _ := strconv.Atoi(m[1])
	e, _ := strconv.Atoi(m[2])
	p.Season = s
	p.Episode = e
	p.IsEpisode = true
	p.Kind = "episode"
}

func applyNum(p *Parsed, s string) {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(s, "v2"), "v3"), 64)
	if err == nil {
		p.Episode = int(f)
	}
	p.IsEpisode = true
	p.Kind = "episode"
	// A bare number is an episode number and says nothing about the season — the
	// folder is what decides that. Filling in season 1 here made every folder that
	// names its seasons look like season 1: bilibili numbers its seasons (第一季 /
	// 第二季 / 游郭篇), and each one came out labelled "… · 第 1 季" with the real
	// season number lost.
}

func pickLonger(a, b string) string {
	if len([]rune(a)) >= len([]rune(b)) {
		return a
	}
	return b
}

// reContainerDir matches folder names that group UNRELATED titles
// (Movie / Anime / 合集 / 未分类 …). A container is never a title and never a
// season marker — unlike 剧场版, which marks one show's movie.
var reContainerDir = regexp.MustCompile(`(?i)^(movies?|films?|anime|animations?|tv|series|shows?|cartoons?|music|musics|software|apps?|docs?|others?|misc|collections?|合集|动画|動畫|番剧|番劇|剧集|劇集|电影|電影|音乐|音樂|软件|軟體|其他|其它|未分类|未分類|新建文件夹|新建資料夾|我的|下载|下載)$`)

// IsContainerDir reports whether name is a grouping folder rather than a title.
func IsContainerDir(name string) bool {
	return reContainerDir.MatchString(strings.TrimSpace(name))
}

// IsCategoryDir is IsContainerDir plus theatrical-movie folders: a parent that
// groups whole titles must never be promoted to a show either.
func IsCategoryDir(name string) bool {
	n := strings.TrimSpace(name)
	return reContainerDir.MatchString(n) ||
		n == "剧场版" || n == "劇場版" || strings.EqualFold(n, "theatrical")
}

// ParseSeasonDir detects "Season 01" / "S01" / "第一季" / "第 2 季" folder names.
func ParseSeasonDir(name string) (int, bool) {
	m := reSeasonOnly.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return 0, false
	}
	for _, g := range m[1:] {
		if g == "" {
			continue
		}
		if n, err := strconv.Atoi(g); err == nil {
			return n, true
		}
		if n, ok := cnNumeral(g); ok {
			return n, true
		}
	}
	return 0, false
}

// cnNumeral converts 一..九十九 (第一季 / 第十二季 …) to a number.
func cnNumeral(s string) (int, bool) {
	digits := map[rune]int{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return 0, false
	}
	total := 0
	if r[0] == '十' {
		total = 10
	} else if d, ok := digits[r[0]]; ok {
		total = d
	} else {
		return 0, false
	}
	if len(r) > 1 {
		if r[1] != '十' {
			return 0, false
		}
		total *= 10
		if len(r) > 2 {
			d, ok := digits[r[2]]
			if !ok {
				return 0, false
			}
			total += d
		}
	}
	return total, true
}

// reSpecialSeason matches collection folders that are *season-level* but carry
// no season number: 剧名/OAD/…, 剧名/剧场版/…, 剧名/SP/… — the show title must
// come from their parent, never from the folder itself.
var reSpecialSeason = regexp.MustCompile(`(?i)^(oad|ova|sps?|specials?|movie|剧场版|劇場版|特别篇|特別篇|特典|番外篇?|总集篇|總集篇|recap)([\s\._\-]?\d{1,2})?$`)

// specialSeasonBase keeps every special collection of one show in its own
// season slot (regular seasons are 1..90), so "OAD" and "剧场版" become two
// separate cards instead of colliding into one.
var specialSeasonBase = map[string]int{
	"oad": 91, "ova": 92, "movie": 93, "剧场版": 93, "劇場版": 93,
	"sp": 94, "sps": 94, "special": 94, "specials": 94, "特别篇": 94, "特別篇": 94,
	"特典": 95, "番外": 96, "番外篇": 96, "总集篇": 97, "總集篇": 97, "recap": 97,
}

// SpecialSeasonDir reports whether a directory is a special collection and
// returns its display name plus the synthetic season number to store.
func SpecialSeasonDir(name string) (string, int, bool) {
	n := strings.TrimSpace(name)
	m := reSpecialSeason.FindStringSubmatch(n)
	if m == nil {
		return "", 0, false
	}
	key := strings.ToLower(m[1])
	base, ok := specialSeasonBase[key]
	if !ok {
		return "", 0, false
	}
	display := m[1]
	switch key {
	case "sp", "sps", "special", "specials":
		display = "特别篇"
	case "movie":
		display = "剧场版"
	case "oad":
		display = "OAD"
	case "ova":
		display = "OVA"
	}
	// optional trailing number: OAD2 / 剧场版2 → its own slot
	if m[2] != "" {
		if k, err := strconv.Atoi(strings.TrimLeft(m[2], " ._-")); err == nil && k > 0 {
			if k > 1 {
				display = fmt.Sprintf("%s %d", display, k)
			}
			base += k - 1
		}
	}
	return display, base, true
}

// extractYear finds the last standalone year in the string, returns cleaned title + year.
func extractYear(s string) (string, int) {
	locs := reYear.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return cleanTitle(s), 0
	}
	l := locs[len(locs)-1]
	year, _ := strconv.Atoi(s[l[2]:l[3]])
	return cleanTitle(s[:l[2]]), year
}

// stripTitleYear removes a trailing standalone year from a show title.
func stripTitleYear(s string) string {
	t, _ := extractYear(s)
	return t
}

var punctReplacer = strings.NewReplacer(".", " ", "_", " ", "-", " ")

func cleanTitle(s string) string {
	s = punctReplacer.Replace(s)
	s = qualityTokens.ReplaceAllString(s, " ")
	s = cjkTokens.ReplaceAllString(s, " ")
	s = reBracketGroups.ReplaceAllString(s, " ")
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '(' || r == ')' || r == '[' || r == ']' || r == '【' || r == '】' || r == '~' || r == '!' || r == ','
	}), " ")
	return strings.TrimSpace(s)
}

// DisplayName drops the bracketed suffixes a directory name may carry, the way
// ParseName drops them from a file name. A source is free to put an id, a year or
// a release group in brackets — that is what makes a virtual tree resolvable from
// a path alone — and none of it belongs on a season card, where "游郭篇
// [season 39433]" should read "游郭篇". Falls back to the original when stripping
// would leave nothing behind.
func DisplayName(s string) string {
	if out := strings.Join(strings.Fields(reBracketGroups.ReplaceAllString(s, " ")), " "); out != "" {
		return out
	}
	return strings.TrimSpace(s)
}

// NormalizeKey builds a match key for dedupe.
func NormalizeKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
