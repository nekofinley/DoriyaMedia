package metadata

import (
	"strings"
	"sync"

	"github.com/longbridgeapp/opencc"
)

// zh.go: Traditional → Simplified folding for metadata matching.
//
// bilibili's catalogue is licensed per region, and the 港澳台 catalogue states
// titles in Traditional Chinese ("鬼滅之刃", "間諜家家酒", "葬送的芙莉蓮").
// TMDB's and Bangumi's Chinese names are Simplified. A search with the
// Traditional spelling can come back empty even though both providers know the
// show, and a result found that way compares unequal to the folder name — which
// is the "掃不到" case a Hong Kong proxy produces.
//
// The conversion is only used for searching and comparing; the title stored on
// an item is whatever the source or provider actually said.

// t2s is built once, lazily: constructing the dictionary reads about a
// megabyte of embedded tables, and a server whose metadata pass never runs
// should not pay for it at startup.
var t2s = sync.OnceValue(func() *opencc.OpenCC {
	cc, err := opencc.New("t2s")
	if err != nil {
		// Without the dictionary the fallback is the identity, which is the
		// behaviour before this existed: still correct for Simplified titles.
		return nil
	}
	return cc
})

// toSimplified folds a title to Simplified Chinese. Anything the converter
// does not know is left alone, so ASCII names and Japanese titles pass through
// unchanged.
func toSimplified(s string) string {
	if s == "" {
		return s
	}
	cc := t2s()
	if cc == nil {
		return s
	}
	out, err := cc.Convert(s)
	if err != nil || out == "" {
		return s
	}
	return out
}

// coreTitle removes a trailing regional qualifier such as
// "（僅限港澳台地區）". Bilibili's 港澳台 catalogue includes the qualifier in
// the series name, while TMDB and Bangumi index the title without it. The
// returned string is only used for searching and matching; the item's display
// name is left untouched.
func coreTitle(s string) string {
	s = strings.TrimSpace(s)
	for {
		core, ok := trimRegionQualifier(s)
		if !ok {
			return s
		}
		core = strings.TrimSpace(core)
		if core == "" {
			return s
		}
		s = core
	}
}

// trimRegionQualifier removes one bracketed region qualifier from the end of
// s. Both full-width and ASCII brackets are accepted, as are the traditional
// and simplified spellings of the qualifier.
func trimRegionQualifier(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return s, false
	}
	runes := []rune(s)
	closeRune := runes[len(runes)-1]
	var openRune rune
	switch closeRune {
	case ')':
		openRune = '('
	case '）':
		openRune = '（'
	case ']':
		openRune = '['
	case '】':
		openRune = '【'
	default:
		return s, false
	}

	depth := 0
	for i := len(runes) - 1; i >= 0; i-- {
		switch runes[i] {
		case closeRune:
			depth++
		case openRune:
			depth--
			if depth == 0 {
				if i <= 0 {
					return s, false
				}
				if !isRegionQualifier(string(runes[i+1 : len(runes)-1])) {
					return s, false
				}
				return string(runes[:i]), true
			}
		}
	}
	return s, false
}

func isRegionQualifier(s string) bool {
	s = strings.Join(strings.Fields(toSimplified(s)), "")
	s = strings.TrimPrefix(s, "仅")
	s = strings.TrimPrefix(s, "限")
	s = strings.TrimSuffix(s, "地区")
	switch s {
	case "港澳台", "港澳", "港台", "台湾", "香港", "澳门":
		return true
	default:
		return false
	}
}

// searchSpellings is the list of spellings worth asking a provider for, in
// order: the title as it stands, then its Simplified core title and Traditional
// core title when a regional qualifier was removed. Keeping both core forms
// covers providers that index only one of the two Chinese spellings.
func searchSpellings(s string) []string {
	s = strings.TrimSpace(s)
	out := make([]string, 0, 3)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}
	add(s)
	core := coreTitle(s)
	if core != s {
		add(toSimplified(core))
		add(core)
	} else {
		add(toSimplified(s))
	}
	return out
}
