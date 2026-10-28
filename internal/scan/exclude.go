package scan

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

// Exclusion is what a library refuses to import: release furniture (NCED,
// menus, previews…), alternate cuts, anything the user adds. A matched file is
// not organised into the library at all — it does not become an episode, a film
// or a card — and a rescan removes the rows it used to have. It stays visible in
// the file browser, so it can still be played directly.
//
// The list is a setting, not a constant: both the global setting and a single
// library's own list are editable, and a fresh install starts with neither of
// them filled in — the words that mean "not an episode" differ per collection,
// and a wrong guess quietly drops whole shows.
//
// There are two lists, matched at different levels: keywords against the file's
// name, folder names against any folder between the library root and the file.
// Excluding a folder skips everything inside it, which is how an Extras or
// Sample directory is kept out without naming every file in it.
//
// An entry wrapped in slashes — /^\[PV\d+\]$/ — is a regular expression and is
// used as written; every other entry is matched the plain way described below.

// EffectiveExcludes returns the keywords in force for one library: its own list
// when it keeps one, the global list otherwise.
func EffectiveExcludes(lib *models.Library, cfg *config.Config) []string {
	if lib != nil && lib.ExcludeMode == "custom" {
		return parseKeywords(lib.Excludes)
	}
	if cfg != nil {
		return cfg.ExcludeKeywords
	}
	return nil
}

// EffectiveExcludeFolders is the same question for the folder list: the library's
// own list when it keeps one, the global list otherwise.
func EffectiveExcludeFolders(lib *models.Library, cfg *config.Config) []string {
	if lib != nil && lib.ExcludeFolderMode == "custom" {
		return parseKeywords(lib.ExcludeFolders)
	}
	if cfg != nil {
		return cfg.ExcludeFolders
	}
	return nil
}

// parseKeywords reads a json array, tolerating newline-separated text so a
// hand-edited row cannot take a scan down.
func parseKeywords(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		list = strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' })
	}
	out := make([]string, 0, len(list))
	for _, k := range list {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// regexBody reports whether a keyword is written as a regular expression
// (/pattern/) and returns its body. An empty body ("//") is not a pattern: it
// would match everything, which nobody means by accident, so it stays an
// ordinary word.
func regexBody(keyword string) (string, bool) {
	if len(keyword) < 3 || keyword[0] != '/' || keyword[len(keyword)-1] != '/' {
		return "", false
	}
	body := keyword[1 : len(keyword)-1]
	if strings.TrimSpace(body) == "" {
		return "", false
	}
	return body, true
}

// ValidateExcludes checks every entry written as a regular expression and reports
// the first one that does not compile, so a save can be refused with a message
// naming the culprit instead of storing a keyword that silently never matches.
// Plain keywords are always fine.
func ValidateExcludes(keywords []string) error {
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		body, ok := regexBody(k)
		if !ok {
			continue
		}
		if _, err := regexp.Compile(body); err != nil {
			return fmt.Errorf("正则写错了:%s(%v)", k, err)
		}
	}
	return nil
}

// excludeMatcher compiles the keywords into one pattern.
//
// A keyword written in latin letters matches a whole word, allowing a trailing
// number: "pv" excludes "[PV1]" and "[PV]" but not "Spy×Family", and "cm" does
// not eat a show whose name happens to contain those letters. Anything else —
// Chinese, or a keyword with punctuation — matches as a plain substring, because
// those have no word boundaries to speak of.
//
// A keyword wrapped in slashes is a regular expression and is used as written,
// case sensitivity included: /^pv/ is case-sensitive, /(?i)^pv/ is not. Plain
// keywords stay case-insensitive. A stored pattern that no longer compiles (a
// hand-edited row) is skipped instead of taking every other keyword down with it;
// ValidateExcludes is what keeps such rows from being saved in the first place.
func excludeMatcher(keywords []string) *regexp.Regexp {
	parts := make([]string, 0, len(keywords))
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if body, ok := regexBody(k); ok {
			if _, err := regexp.Compile(body); err != nil {
				log.Printf("exclude: skipping keyword %q: %v", k, err)
				continue
			}
			parts = append(parts, "(?:"+body+")")
			continue
		}
		q := regexp.QuoteMeta(k)
		if isLatinWord(k) {
			parts = append(parts, `(?i:(?:^|[^a-z0-9])`+q+`s?\d*(?:$|[^a-z0-9]))`)
		} else {
			parts = append(parts, `(?i:`+q+`)`)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	re, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return nil
	}
	return re
}

// isLatinWord reports whether a keyword is plain latin letters, the only kind
// that can be matched on word boundaries.
func isLatinWord(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == ' ' || r == '-':
			// "nc-op" is still a word: the boundary test handles the dash.
		default:
			return false
		}
	}
	return s != ""
}

// dropExcluded removes the files this library does not accept: a file whose name
// matches the keyword list, or whose folder matches the folder list. Cleanup then
// sees them as gone, which is how a newly added keyword takes an already imported
// file back out of the library.
func dropExcluded(files []fileRec, re, dirRe *regexp.Regexp) []fileRec {
	if (re == nil && dirRe == nil) || len(files) == 0 {
		return files
	}
	out := make([]fileRec, 0, len(files))
	for _, fr := range files {
		if re != nil && re.MatchString(baseName(fr.Path)) {
			continue
		}
		if dirRe != nil && dirExcluded(fr, dirRe) {
			continue
		}
		out = append(out, fr)
	}
	return out
}

// dirExcluded reports whether any folder between the library root and the file
// matches the folder list.
//
// The walk starts below Root on purpose: Root is the folder the user pointed the
// library at, and a keyword that happens to be its name ("Anime") must not empty
// a library the user just created. Only what the scan descends into counts.
func dirExcluded(fr fileRec, re *regexp.Regexp) bool {
	rel := fr.Dir
	if root := strings.TrimSuffix(fr.Root, "/"); root != "" && strings.HasPrefix(rel, root) {
		rel = rel[len(root):]
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg != "" && re.MatchString(seg) {
			return true
		}
	}
	return false
}
