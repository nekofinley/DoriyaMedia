package metadata

import (
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

// searchSpellings is the list of spellings worth asking a provider for, in
// order: the title as it stands, then its Simplified form when different.
func searchSpellings(s string) []string {
	out := []string{s}
	if simp := toSimplified(s); simp != s {
		out = append(out, simp)
	}
	return out
}
