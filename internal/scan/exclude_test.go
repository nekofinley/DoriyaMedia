package scan

import (
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func paths(files []fileRec) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// A folder keyword drops everything below a matching folder, at any depth, and
// leaves the rest untouched.
func TestDropExcludedByFolderName(t *testing.T) {
	files := []fileRec{
		{Path: "/lib/Show/Extras/feature.mkv", Dir: "/lib/Show/Extras", Root: "/lib"},
		{Path: "/lib/Show/S1/E01.mkv", Dir: "/lib/Show/S1", Root: "/lib"},
		{Path: "/lib/Show/S1/Extras/E02.mkv", Dir: "/lib/Show/S1/Extras", Root: "/lib"},
		{Path: "/lib/Show/S2/E03.mkv", Dir: "/lib/Show/S2", Root: "/lib"},
	}
	got := dropExcluded(files, nil, excludeMatcher([]string{"extras"}))
	want := []string{"/lib/Show/S1/E01.mkv", "/lib/Show/S2/E03.mkv"}
	if !sameSet(paths(got), want) {
		t.Fatalf("got %v, want %v", paths(got), want)
	}
}

// The library root itself is never matched: a keyword that happens to be the
// folder the user pointed at must not empty the library.
func TestFolderExcludeIgnoresRoot(t *testing.T) {
	files := []fileRec{
		{Path: "/media/Anime/Show/E01.mkv", Dir: "/media/Anime/Show", Root: "/media/Anime"},
	}
	got := dropExcluded(files, nil, excludeMatcher([]string{"anime"}))
	if len(got) != 1 {
		t.Fatalf("root folder must not be excluded, got %v", paths(got))
	}
}

// Chinese keywords match as a substring of a folder name, which is what an
// anime collection's "特典" / "字幕" folders need.
func TestFolderExcludeChineseSubstring(t *testing.T) {
	files := []fileRec{
		{Path: "/lib/剧集/特典/PV01.mkv", Dir: "/lib/剧集/特典", Root: "/lib"},
		{Path: "/lib/剧集/正片/E01.mkv", Dir: "/lib/剧集/正片", Root: "/lib"},
	}
	got := dropExcluded(files, nil, excludeMatcher([]string{"特典"}))
	if len(got) != 1 || got[0].Path != "/lib/剧集/正片/E01.mkv" {
		t.Fatalf("got %v", paths(got))
	}
}

// File-name keywords are unaffected by the folder list, and both lists apply at
// once when both are set.
func TestExcludeBothLists(t *testing.T) {
	files := []fileRec{
		{Path: "/lib/Show/E01 - sample.mkv", Dir: "/lib/Show", Root: "/lib"},
		{Path: "/lib/Show/Extras/E02.mkv", Dir: "/lib/Show/Extras", Root: "/lib"},
		{Path: "/lib/Show/E03.mkv", Dir: "/lib/Show", Root: "/lib"},
	}
	got := dropExcluded(files, excludeMatcher([]string{"sample"}), excludeMatcher([]string{"extras"}))
	if len(got) != 1 || got[0].Path != "/lib/Show/E03.mkv" {
		t.Fatalf("got %v", paths(got))
	}
}

// With no keywords on either list, nothing is dropped.
func TestExcludeEmptyListsDropsNothing(t *testing.T) {
	files := []fileRec{{Path: "/lib/Show/E01.mkv", Dir: "/lib/Show", Root: "/lib"}}
	got := dropExcluded(files, excludeMatcher(nil), excludeMatcher(nil))
	if len(got) != 1 {
		t.Fatalf("got %v", paths(got))
	}
}

// A library that keeps its own folder list replaces the global one.
func TestEffectiveExcludeFolders(t *testing.T) {
	cfg := &config.Config{ExcludeFolders: []string{"sample"}}
	inherit := &models.Library{ExcludeFolderMode: "inherit"}
	if got := EffectiveExcludeFolders(inherit, cfg); len(got) != 1 || got[0] != "sample" {
		t.Fatalf("inherit got %v", got)
	}
	custom := &models.Library{ExcludeFolderMode: "custom", ExcludeFolders: `["特典"]`}
	got := EffectiveExcludeFolders(custom, cfg)
	if len(got) != 1 || got[0] != "特典" {
		t.Fatalf("custom got %v", got)
	}
	// An empty custom list is an opt-out, not "fall back to global".
	off := &models.Library{ExcludeFolderMode: "custom", ExcludeFolders: "[]"}
	if got := EffectiveExcludeFolders(off, cfg); len(got) != 0 {
		t.Fatalf("opt-out got %v", got)
	}
}
