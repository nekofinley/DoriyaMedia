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
	// Extra marks an entry the source itself says is not a numbered episode of
	// its season: a 特别篇 / PV / 预告 section entry, or a main-list entry bilibili
	// labels without a number ("OVA"). ExtraAt orders it inside its section and
	// ExtraName is the section's own name — the card it belongs on. It is the
	// source's statement, not a guess, and it outranks anything the title says.
	Extra     bool
	ExtraAt   int
	ExtraName string
}

// reExtraTag is the marker a source writes into a file name to state that the
// entry is a section item rather than an episode: [sp#<order>#<section>]. See
// the bilibili driver's extraTag.
var reExtraTag = regexp.MustCompile(`\[sp#(\d+)#([^\]]*)\]`)

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
	reCJKep         = regexp.MustCompile(`第\s*(\d{1,4})\s*[集话話期页頁回]`)
	reEP            = regexp.MustCompile(`(?:^|[\s\._\-\[])(?:EP|E)\.?\s*(\d{1,4})(?:[\s\.\_\-\]]|$)`)
	reBracketEp     = regexp.MustCompile(`[\[【]\s*(\d{1,4}(?:\.\d)?(?:v\d)?)\s*[\]】]`)
	reDashEp        = regexp.MustCompile(`(?:^|[\s_])-[\s_]*(\d{1,4})(?:[\s_\-\[]|$)`)
	rePureNum       = regexp.MustCompile(`^(\d{1,4})(?:v\d)?$`)
	reParenEp       = regexp.MustCompile(`^[\(（]\s*(\d{1,4})\s*[\)）]$`)
	reLeadEp        = regexp.MustCompile(`^(\d{1,4})\s*[-–_]\s*`)
	reBracketEpOnly = regexp.MustCompile(`^\d{1,4}(?:\.\d)?(?:v\d)?$`)
	reTrailingNum   = regexp.MustCompile(`(?:^|[\s\._\-])(\d{1,4})(?:v\d)?\s*$`)
	reBracketGroups = regexp.MustCompile(`[\[【]([^\]】]*)[\]】]`)
	reBoxGroup      = regexp.MustCompile(`\[([^\[\]]*)\]`)
	reEpRange       = regexp.MustCompile(`(?:^|[\s\._\-\|])(\d{1,4})\s*[-–~]\s*(\d{1,4})(?:[\s\._\-\|\[]|$)`)
)

// Blocks that are never part of a title: encoder tags, alias lists, search hints.
var (
	reParenBlock = regexp.MustCompile(`[\(（]([^()（）]*)[\)）]`)
	reBraceBlock = regexp.MustCompile(`[\{｛][^{}｛｝]*[\}｝]`)
	reTechParen  = regexp.MustCompile(`(?i)(\d{3,4}\s*[x×]\s*\d{3,4}` +
		`|\b(?:1080p?|720p?|2160p?|480p|576p|4k|uhd|avc|hevc|h\.?\s?26[45]|x26[45]|av1|vp9|aac|flac|opus|ac3|eac3|dts|truehd|mp3|mp4|mkv|avi|mov|webm|web-?dl|web-?rip|webrip|bdrip|bd-?rip|blu-?ray|bluray|remux|hdtv|tvrip|dvdrip|srt\w*|ass|pgs|subs?)\b` +
		`|\b(?:cr|abema|baha|b-?global|viutv|netflix|nf|amzn|disney|bilibili|atvp|dsnp|hulu)\b` +
		`|检索|字幕|内嵌|内封|外挂|双语|简繁)`)
)

var (
	reSeasonWordEn  = regexp.MustCompile(`(?i)\bseason\s*(\d{1,2})\b`)
	reSeasonWordOrd = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)\s+season\b`)
	reCJKSeason     = regexp.MustCompile(`第\s*(\d{1,2}|[一二三四五六七八九十]{1,3})\s*[季部]`)
	reSeasonBracket = regexp.MustCompile(`(?i)^(?:\d{1,2}\s*月新番|新番|season\s*\d{1,2}|\d{1,2}(?:st|nd|rd|th)\s+season|第\s*[0-9一二三四五六七八九十]{1,3}\s*[季部])$`)
)

var reTitleSep = regexp.MustCompile(`\s*[/|｜❘]\s*`)

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

	// A source that states what an entry is has to be read before the name is.
	// bilibili publishes 特别篇 / PV / 预告 in sections beside the episodes, and it
	// names the entries there the way a viewer would describe them — "第3话预告"
	// is the preview of episode 3, not episode 3 again. Taken as a name, that
	// title matches the episode patterns below and the preview becomes a second
	// version of episode 3: two files on one card, and the preview is what plays
	// when the episode is picked.
	if m := reExtraTag.FindStringSubmatch(base); m != nil {
		if at, err := strconv.Atoi(m[1]); err == nil && at > 0 {
			label := strings.TrimSpace(m[2])
			if label == "" {
				label = "特别篇"
			}
			return Parsed{
				Kind: "episode", Title: cleanTitle(reExtraTag.ReplaceAllString(base, " ")),
				Episode: at, IsEpisode: true, Extra: true, ExtraAt: at, ExtraName: label,
			}
		}
	}

	// anime style: [Group][Show][01][tags]
	if strings.HasPrefix(strings.TrimSpace(base), "[") || strings.HasPrefix(strings.TrimSpace(base), "【") {
		return parseBracketed(base, original)
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

	// "(10).mp4" — the show is the folder, the file carries only its number.
	if m := reParenEp.FindStringSubmatch(base); m != nil {
		applyNum(&p, m[1])
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

	// "01-13" / "025-25_mp4": a span, and its start is the episode.
	if n, ok := rangeStart(base); ok {
		p.Episode = n
		p.IsEpisode = true
		p.Kind = "episode"
		loc := reEpRange.FindStringIndex(base)
		p.Title = cleanTitle(base[:loc[0]])
		if p.Title == "" {
			p.Title = cleanTitle(original)
		}
		return p
	}

	// trailing bare number: "Black Clover 174"
	if n, before, ok := trailingEpisode(base); ok {
		p.Episode = n
		p.IsEpisode = true
		p.Kind = "episode"
		p.Title = cleanTitle(before)
		if p.Title == "" {
			p.Title = cleanTitle(original)
		}
		return p
	}

	// "005-HaruhiS2_05_2398psf_": the number leads, and the rest is the release's
	// own bookkeeping. Nothing else in the name claims a number, so the prefix is
	// the episode.
	//
	// The prefix is only an episode when real words follow it: "1080-v.mp4" is a
	// resolution with a variant suffix, not episode 1080.
	if m := reLeadEp.FindStringSubmatch(base); m != nil {
		rest := base[len(m[0]):]
		if n, err := strconv.Atoi(m[1]); err == nil && plausibleEpisode(n) &&
			!reResolutionNum[n] && len([]rune(rest)) >= 2 && hasLatin(rest) {
			p.Episode = n
			p.IsEpisode = true
			p.Kind = "episode"
			p.Title = cleanTitle(base[len(m[0]):])
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

// parseBracketed reads the anime release style:
//
//	[Group][Show][09][1080P]              episode in a bracket
//	[Group] Show - 09 [WebRip 1080p]      episode after the title
//	【Group】★10月新番★[Show][01]           full-width brackets
func parseBracketed(base, original string) Parsed {
	p := Parsed{}
	groups := reBracketGroups.FindAllStringSubmatch(base, -1)
	rest := strings.TrimSpace(reBracketGroups.ReplaceAllString(base, " "))

	// epIdx is the bracket the episode number came out of. The show name is the
	// bracket before it, which is what tells "[Nekomoe kissaten][Kimi ga Shinu
	// made Koi wo Shitai][12][1080p]" apart: the first bracket is the group.
	epIdx := -1
	for i, g := range groups {
		val := strings.TrimSpace(g[1])
		if reSeasonBracket.MatchString(val) {
			continue
		}
		if reBracketEpOnly.MatchString(val) {
			if n, err := strconv.Atoi(strings.SplitN(val, "v", 2)[0]); err == nil && plausibleEpisode(n) {
				p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
				epIdx = i
				break
			}
		}
		if n, ok := bracketEpisode(val); ok && plausibleEpisode(n) {
			p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
			epIdx = i
			break
		}
		if m := reCJKep.FindStringSubmatch(val); m != nil {
			applyNum(&p, m[1])
			epIdx = i
			break
		}
		if n, ok := bracketRangeStart(val); ok && plausibleEpisode(n) {
			p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
			epIdx = i
			break
		}
	}

	if !p.IsEpisode {
		switch {
		case reSE.MatchString(rest):
			applySE(&p, reSE.FindStringSubmatch(rest))
		case reCJKep.MatchString(rest):
			applyNum(&p, reCJKep.FindStringSubmatch(rest)[1])
		case reEP.MatchString(rest):
			applyNum(&p, reEP.FindStringSubmatch(rest)[1])
		case reDashEp.MatchString(rest):
			if n, err := strconv.Atoi(reDashEp.FindStringSubmatch(rest)[1]); err == nil && n <= 999 {
				p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
			}
		default:
			if n, ok := rangeStart(rest); ok {
				p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
			} else if n, _, ok := trailingEpisode(rest); ok {
				p.Episode, p.IsEpisode, p.Kind = n, true, "episode"
			}
		}
	}

	if season, cleaned := takeSeasonWord(rest); season > 0 {
		p.Season = season
		rest = cleaned
	}

	title, have := titleCandidates(rest)
	if have == 0 {
		title = bracketShowTitle(groups, epIdx)
	}
	if title == "" {
		if m := reBraceBlock.FindString(original); m != "" {
			inner := strings.TrimSpace(m[1 : len(m)-1])
			title, _ = titleCandidates(inner)
		}
		if title == "" {
			title, _ = titleCandidates(reBracketGroups.ReplaceAllString(original, " "))
		}
		if title == "" {
			title = cleanTitle(original)
		}
	}
	p.Title = stripEpisodeTail(title, p.Episode)
	if p.Title == "" {
		p.Title = title
	}
	if !p.IsEpisode {
		p.Kind = "movie"
		if _, y := extractYear(title); y > 0 {
			p.Year, p.HasYear = y, true
		}
	}
	return p
}

// rangeStart reads the first episode of a span: "01-13" → 1. A year range is
// refused.
func rangeStart(s string) (int, bool) {
	m := reEpRange.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	a, err1 := strconv.Atoi(m[1])
	b, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || a <= 0 || b < a || a > 9999 {
		return 0, false
	}
	if a >= 1900 && a <= 2100 && b <= 2100 {
		return 0, false
	}
	return a, true
}

// plausibleEpisode refuses a year, which "[2019]" in a bracket would otherwise
// make episode 2019.
func plausibleEpisode(n int) bool {
	if n <= 0 || n > 9999 {
		return false
	}
	return !(n >= 1900 && n <= 2100)
}

// bracketRangeStart is rangeStart for one bracket's content, which may also be a
// span carrying a note: "[39-下周停播]" starts at 39.
func bracketRangeStart(val string) (int, bool) {
	if n, ok := rangeStart(val); ok {
		return n, true
	}
	m := reTrailingRangeStart.FindStringSubmatch(val)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 || n > 9999 {
		return 0, false
	}
	return n, true
}

var reTrailingRangeStart = regexp.MustCompile(`^(\d{1,4})\s*[-–~]\s*\D`)

// bracketShowTitle answers with the bracket that reads as a show name rather
// than the group, a tag, a number or a cours marker.
//
// A release puts the show in front of the episode number — "[Group][Show][12]
// [1080p]" — so the brackets are read outwards from the one the number came
// from: the nearest readable bracket before it wins, and only then the ones
// after. Position, not shape, is what separates "[Nekomoe kissaten]" from
// "[Kimi ga Shinu made Koi wo Shitai]": both are pure ASCII, and only their
// place in the run tells the group from the show.
func bracketShowTitle(groups [][]string, epIdx int) string {
	readable := make([]string, len(groups))
	for i, g := range groups {
		if len(g) < 2 {
			continue
		}
		val := strings.TrimSpace(g[1])
		if val == "" || reBracketEpOnly.MatchString(val) || reSeasonBracket.MatchString(val) {
			continue
		}
		if _, ok := bracketEpisode(val); ok {
			continue
		}
		// A release group opens the run, so only the first bracket can be one. A
		// later bracket is a name: reading "[KTXP][Kono Subarashii Sekai ni
		// Shukufuku o！…]" as a group because the title contains "sub" is how a
		// show ended up with no title at all.
		if i == 0 && looksLikeGroup(val) {
			continue
		}
		if t, n := titleCandidates(val); t != "" && n > 0 && !techOnlyTitle(t) {
			readable[i] = t
		}
	}
	if epIdx >= 0 {
		for i := epIdx - 1; i >= 0; i-- {
			if readable[i] != "" {
				return readable[i]
			}
		}
		for i := epIdx + 1; i < len(readable); i++ {
			if readable[i] != "" {
				return readable[i]
			}
		}
		return ""
	}
	// No bracket carried the number: the longest readable bracket is the title,
	// which is what a "[Group][Show][1080p]" movie layout looks like.
	best := ""
	for _, t := range readable {
		if len([]rune(t)) > len([]rune(best)) {
			best = t
		}
	}
	return best
}

// looksLikeGroup reports whether a bracket is a release group rather than a
// show: 字幕组 / 汉化组 / 工作室 suffixes, and short latin handles.
func looksLikeGroup(s string) bool {
	low := strings.ToLower(s)
	for _, tok := range []string{"字幕", "汉化", "漢化", "工作室", "制作组", "製作組", "压制", "壓制", "搬运", "搬運",
		"屋", "社", "组", "組"} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	if reGroupWord.MatchString(low) {
		return true
	}
	if !strings.Contains(s, " ") && len([]rune(s)) <= 12 && hasLatin(s) {
		return true
	}
	return false
}

// reGroupWord matches the latin half of a release-group name as a whole word:
// "Sakurato.sub" and "[Raws]" are groups, while a show whose name merely happens
// to contain those letters is not — "Kono Subarashii Sekai ni Shukufuku o！"
// contains "sub" and is still a title.
var reGroupWord = regexp.MustCompile(`(?i)(?:^|[\s._\-\[\]()（）])(?:fansubs?|raws?|subs?|house)(?:$|[\s._\-\[\]()（）])`)

func hasLatin(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// techOnlyTitle reports whether a bracket holds release furniture — resolution,
// codec, source, subtitle language, checksum — instead of a name. "[1080p]" and
// "[JPSC]" are furniture, "[Kimi ga Shinu made Koi wo Shitai]" is a name, and
// only the words in it can say which: a character-class test ("pure ASCII") sees
// no difference, which is how an English show title became no title at all.
func techOnlyTitle(s string) bool {
	rest := cjkTokens.ReplaceAllString(qualityTokens.ReplaceAllString(s, " "), " ")
	for _, f := range strings.Fields(rest) {
		if isNameWord(f) {
			return false
		}
	}
	return true
}

// isNameWord reports whether one word of a bracket reads as part of a name.
// Furniture is a word carrying digits (a resolution, a bit depth, a checksum) or
// a short all-caps tag ("JPSC", "NCOP"); everything else is a name.
func isNameWord(f string) bool {
	f = strings.Trim(f, ".-_()[]{}!,'\"·:;")
	if f == "" || strings.ContainsAny(f, "0123456789") {
		return false
	}
	for _, r := range f {
		if unicode.IsLower(r) || unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	// All-caps latin is a name only when it is too long to be a tag.
	return len([]rune(f)) >= 6
}

func applySE(p *Parsed, m []string) {
	s, _ := strconv.Atoi(m[1])
	e, _ := strconv.Atoi(m[2])
	p.Season = s
	p.Episode = e
	p.IsEpisode = true
	p.Kind = "episode"
}

// takeSeasonWord pulls the cours marker out of a title: "黑色五叶草 Season 2" →
// (2, "黑色五叶草"). The marker is stored in Season and must not reach a search
// query, which "Black Clover Season 2" would ruin.
func takeSeasonWord(s string) (int, string) {
	season := 0
	for _, re := range []*regexp.Regexp{reSeasonWordEn, reSeasonWordOrd, reCJKSeason} {
		m := re.FindStringSubmatchIndex(s)
		if m == nil {
			continue
		}
		raw := ""
		if m[2] >= 0 {
			raw = s[m[2]:m[3]]
		}
		n := 0
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		} else if v, ok := cnNumeral(raw); ok {
			n = v
		}
		if n > 0 && n <= 99 && season == 0 {
			season = n
		}
		s = strings.TrimSpace(s[:m[0]] + " " + s[m[1]:])
	}
	return season, s
}

// splitTitles cuts a name at the separators between the several spellings of one
// show ("冰之城墙 / Koori no Jouheki").
func splitTitles(s string) []string {
	parts := reTitleSep.Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// titleCandidates returns the first usable reading of one name part and how
// many there were; zero means nothing was left and the caller should fall back.
func titleCandidates(part string) (string, int) {
	return titleCandidatesExcept(part, 0)
}

// titleCandidatesExcept skips skip usable readings, and always skips a segment
// that is nothing but a number left between two separators.
func titleCandidatesExcept(part string, skip int) (string, int) {
	part = reSearchHint.ReplaceAllString(part, " ")
	segs := splitTitles(part)
	cleaned := make([]string, 0, len(segs))
	for _, seg := range segs {
		if c := cleanTitle(seg); c != "" {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) == 0 {
		return "", 0
	}
	usable := 0
	for _, c := range cleaned {
		if !isBareNumber(c) {
			usable++
		}
	}
	for _, c := range cleaned {
		if isBareNumber(c) || skip > 0 {
			if !isBareNumber(c) {
				skip--
			}
			continue
		}
		return c, usable
	}
	return cleaned[0], usable
}

// isBareNumber reports whether a segment is nothing but an episode number left
// between two title separators.
func isBareNumber(s string) bool {
	return rePureNum.MatchString(strings.TrimSpace(s))
}

// cleanTitle strips the tags a release name carries, leaving the show title.
func cleanTitle(s string) string {
	// These blocks are dropped whole, before token replacement can leave their
	// punctuation behind as separate words.
	s = reBraceBlock.ReplaceAllString(s, " ")
	s = reParenBlock.ReplaceAllStringFunc(s, func(block string) string {
		inner := block[1 : len(block)-1]
		if reTechParen.MatchString(inner) {
			return " "
		}
		return block
	})
	s = punctReplacer.Replace(s)
	s = seasonWordNoise.ReplaceAllString(s, " ")
	// Stars framing a cours marker go with it; a star inside a name does not.
	s = strings.Trim(strings.TrimSpace(s), "★☆ ")
	s = qualityTokens.ReplaceAllString(s, " ")
	s = cjkTokens.ReplaceAllString(s, " ")
	// A CJK bracket is emphasis inside a title, so only its brackets go; the
	// ASCII form is a tag and is dropped whole.
	s = reBoxGroup.ReplaceAllString(s, " ")
	s = strings.NewReplacer("【", "", "】", "", "「", "", "」", "").Replace(s)
	s = reCJKep.ReplaceAllString(s, " ")
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '(' || r == ')' || r == '[' || r == ']' || r == '~' || r == '!' || r == ',' || r == '、' || r == '；' || r == ';' || r == '：' || r == ':' || r == '|' || r == '/'
	}), " ")
	// "魔法★探险家" keeps its ★: it separates words inside the name.
	s = strings.ReplaceAll(s, " ★ ", "★")
	return strings.TrimSpace(s)
}

// reSearchHint matches the "(检索：…)" block some groups append for finders.
var reSearchHint = regexp.MustCompile(`[\(（]\s*检索[^\)）]*[\)）]`)

// stripEpisodeTail removes the episode already recorded from the title it was
// taken out of. Only that value is removed, so a title legitimately ending in a
// number ("赛博朋克 边缘行者 2") keeps it.
func stripEpisodeTail(title string, ep int) string {
	if ep <= 0 || title == "" {
		return title
	}
	num := strconv.Itoa(ep)
	padded := fmt.Sprintf("%02d", ep)
	out := title
	for _, pattern := range []string{
		`(?i)\bEP?\.?\s*0*` + num + `\b`,
		`第\s*` + num + `\s*[集话話期页頁回回]`,
		`(?i)\b(?:0?` + num + `|` + padded + `)v\d+\b`,
		`(?:^|[\s\._\-])0*` + num + `\s*$`,
	} {
		out = regexp.MustCompile(pattern).ReplaceAllString(out, " ")
	}
	out = reSubLanguageFrag.ReplaceAllString(out, "")
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}

// reSubLanguageFrag drops a lone subtitle-language letter left at the end, the
// way "[简／繁] (1080p H.264 AAC SRTx2)" leaves "H".
var reSubLanguageFrag = regexp.MustCompile(`(?i)\s+\b[achstk]\b$`)

// seasonWordNoise removes what survives of a cours marker once its number was
// taken out.
var seasonWordNoise = regexp.MustCompile(`(?i)(?:\b(?:season|series|新番)\b|\d+(?:st|nd|rd|th)\b|\d+\s*月新番)`)

// reTrailingTag matches a short language/codec/version tag at the very end of a
// name ("gb", "CHS", "v2", "x265"), the way "[KissSub]Magia Record - Mahou Shoujo
// 04 gb" ends in one.
var reTrailingTag = regexp.MustCompile(`(?i)\s+(?:[a-z]{1,4}|v\d{1,2}|x26[45]|h\.?26[45]|hevc|avc|aac|flac|opus|mp4|mkv)$`)

// trailingEpisode reads a name that ends in its episode number ("Black Clover
// 174", "魔法★探险家 05 (CR 1920x1080)"), plus the text before it.
func trailingEpisode(s string) (int, string, bool) {
	s = strings.TrimRight(strings.TrimSpace(s), " \t")
	for {
		trimmed := strings.TrimSpace(s)
		changed := false
		for _, re := range []*regexp.Regexp{reBracketGroups, reParenBlock, reBraceBlock} {
			if m := re.FindStringIndex(trimmed); m != nil && m[1] == len(trimmed) {
				trimmed = strings.TrimSpace(trimmed[:m[0]])
				changed = true
			}
		}
		// A language or codec tag written after the number ("… 04 gb") is not
		// part of the name, and must not hide the number it follows.
		if m := reTrailingTag.FindStringIndex(trimmed); m != nil && m[1] == len(trimmed) {
			trimmed = strings.TrimSpace(trimmed[:m[0]])
			changed = true
		}
		if !changed {
			s = trimmed
			break
		}
		s = trimmed
	}
	if s == "" {
		return 0, "", false
	}
	loc := reTrailingNum.FindStringSubmatchIndex(s)
	if loc == nil {
		return 0, "", false
	}
	raw := s[loc[2]:loc[3]]
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > 9999 {
		return 0, "", false
	}
	before := strings.TrimSpace(s[:loc[2]])
	if before == "" {
		return 0, "", false
	}
	if reResolution.MatchString(s) {
		return 0, "", false
	}
	return n, before, true
}

var reResolution = regexp.MustCompile(`(?i)\d{3,4}\s*[x×]\s*\d{3,4}|\b(?:1080|720|2160|480|576)[pi]\b`)

// reResolutionNum are the numbers that are a picture size, never an episode.
var reResolutionNum = map[int]bool{480: true, 576: true, 720: true, 1080: true, 1440: true, 2160: true, 4320: true}

func applyNum(p *Parsed, s string) {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(s, "v2"), "v3"), 64)
	if err == nil {
		p.Episode = int(f)
	}
	p.IsEpisode = true
	p.Kind = "episode"
	// Season stays 0: a bare episode number says nothing about which cours it is,
	// and the folder decides.
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

// reShowDirSeason matches the season a collection writes in a bracket of the
// show folder itself: the "[S2]" of "[CLANNAD][S2]", plus the forms that carry a
// note — "[S3@60FPS]", "[S2 S]", "[S1-4]", "[S2.5]".
var reShowDirSeason = regexp.MustCompile(`(?i)^s(\d{1,2})(?:\.\d+)?(?:[\s@\-–~·_].*)?$`)

// ParseShowDirName reads the folder convention that keeps one show's seasons in
// sibling directories named after it: "[show][S2]". The show is the first
// bracket, a later bracket carries the season, and whatever follows that bracket
// names the season — "[中二病也要谈恋爱!][S2][恋!]" is show 中二病也要谈恋爱!,
// season 2, named 恋!.
//
// "[S0]" is the show's theatrical film, not a zeroth season: the folder holds the
// movie, and the caller files it under 剧场版.
//
// Returns zero values for any folder that is not this shape, so a release folder
// ("[Group] Show [1080p]") is left to the normal name parser.
func ParseShowDirName(name string) (title string, season int, seasonName string, movie bool) {
	n := strings.TrimSpace(name)
	if !strings.HasPrefix(n, "[") && !strings.HasPrefix(n, "【") {
		return "", 0, "", false
	}
	// Text outside the brackets means this is a release folder, not a season
	// marker: only the all-brackets shape belongs to this convention.
	if strings.TrimSpace(reBracketGroups.ReplaceAllString(n, " ")) != "" {
		return "", 0, "", false
	}
	groups := reBracketGroups.FindAllStringSubmatch(n, -1)
	if len(groups) < 2 {
		return "", 0, "", false
	}
	title = cleanTitle(groups[0][1])
	if title == "" || reBracketEpOnly.MatchString(title) {
		return "", 0, "", false
	}

	names := make([]string, 0, len(groups))
	found := false
	for _, g := range groups[1:] {
		val := strings.TrimSpace(g[1])
		if val == "" {
			continue
		}
		if !found {
			if m := reShowDirSeason.FindStringSubmatch(val); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					season, found = v, true
				}
				continue
			}
			// A special collection ("[OVA]", "[剧场版]", "[SP]") is its own slot.
			if disp, num, ok := SpecialSeasonDir(val); ok {
				season, seasonName, found = num, disp, true
				continue
			}
			// Nothing in front of the marker can name the season.
			continue
		}
		// What follows the marker names the season ("恋!", "解").
		names = append(names, strings.TrimSpace(strings.Trim(val, "-–—~· ")))
	}
	if !found {
		return "", 0, "", false
	}
	if season == 0 {
		// "[S0]" holds the film.
		return title, 0, "", true
	}
	if seasonName == "" {
		seasonName = strings.Join(nonEmpty(names), " · ")
	}
	return title, season, seasonName, false
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// seasonMarkerDir reports the season number a bracket-only folder name marks, so
// a bare "[S2]" directory is a season folder like any other.
func seasonMarkerDir(name string) (int, bool) {
	v := strings.TrimSpace(name)
	if !strings.HasPrefix(v, "[") && !strings.HasPrefix(v, "【") {
		return 0, false
	}
	if strings.TrimSpace(reBracketGroups.ReplaceAllString(v, " ")) != "" {
		return 0, false
	}
	groups := reBracketGroups.FindAllStringSubmatch(v, -1)
	if len(groups) != 1 {
		return 0, false
	}
	if m := reShowDirSeason.FindStringSubmatch(strings.TrimSpace(groups[0][1])); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
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
