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

// A /…/ entry is a regular expression, used as written — including its case
// sensitivity, which the plain keywords do not have.
func TestRegexKeyword(t *testing.T) {
	re := excludeMatcher([]string{`/^\[PV\d+\]$/`})
	if re == nil {
		t.Fatal("matcher is nil")
	}
	if !re.MatchString("[PV12]") || !re.MatchString("[PV1]") {
		t.Error("regex should match [PV12] and [PV1]")
	}
	if re.MatchString("[PV]") {
		t.Error("\\d+ requires a digit")
	}

	// Case-sensitive unless the pattern says otherwise.
	lower := excludeMatcher([]string{`/^pv\d*$/`})
	if lower.MatchString("PV01") {
		t.Error("/^pv\\d*$/ must not match PV01")
	}
	insensitive := excludeMatcher([]string{`/(?i)^pv\d*$/`})
	if !insensitive.MatchString("PV01") {
		t.Error("/(?i)^pv\\d*$/ should match PV01")
	}
}

// A plain keyword keeps its old meaning next to a regex: latin words on word
// boundaries, everything else as a substring.
func TestRegexAndPlainKeywordsTogether(t *testing.T) {
	re := excludeMatcher([]string{`/^\d+\./`, "sample"})
	if !re.MatchString("01. 开场.mkv") {
		t.Error("leading number should match")
	}
	if !re.MatchString("Show - Sample.mkv") {
		t.Error("plain keyword should still work")
	}
	if re.MatchString("Spy x Family.mkv") {
		t.Error("plain keyword must not match inside a word")
	}
}

// A regex in the folder list matches a folder name at any depth.
func TestRegexFolderKeyword(t *testing.T) {
	files := []fileRec{
		{Path: "/lib/Show/CD1/E01.mkv", Dir: "/lib/Show/CD1", Root: "/lib"},
		{Path: "/lib/Show/E01.mkv", Dir: "/lib/Show", Root: "/lib"},
	}
	got := dropExcluded(files, nil, excludeMatcher([]string{`/^(CD|DVD)\d+$/`}))
	if len(got) != 1 || got[0].Path != "/lib/Show/E01.mkv" {
		t.Fatalf("got %v", paths(got))
	}
}

// A stored pattern that no longer compiles is skipped, so the rest of the list
// keeps working instead of the whole matcher collapsing to nil.
func TestBrokenRegexIsSkipped(t *testing.T) {
	re := excludeMatcher([]string{`/(unclosed/`, "sample"})
	if re == nil {
		t.Fatal("a broken entry must not disable the other keywords")
	}
	if !re.MatchString("Show - Sample.mkv") {
		t.Error("the good keyword should still match")
	}
}

// // is not an empty pattern (that would match everything): it stays a word.
func TestEmptyRegexIsLiteral(t *testing.T) {
	if _, ok := regexBody("//"); ok {
		t.Fatal("// must not be treated as a regex")
	}
	if re := excludeMatcher([]string{"//"}); re == nil || re.MatchString("anything") {
		t.Fatal("// must only match the literal text")
	}
}

func TestValidateExcludes(t *testing.T) {
	ok := []string{"sample", `/(?i)^pv\d+$/`, "特典", "//"}
	if err := ValidateExcludes(ok); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if err := ValidateExcludes([]string{"sample", `/(unclosed/`}); err == nil {
		t.Fatal("broken regex must be reported")
	}
}
