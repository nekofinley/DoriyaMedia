package scan

import (
	"encoding/json"
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
// The list is a setting, not a constant: every install starts from
// config.DefaultExcludeKeywords, and both the global setting and a single
// library's own list are editable.

// EffectiveExcludes returns the keywords in force for one library: its own list
// when it keeps one, the global list otherwise.
func EffectiveExcludes(lib *models.Library, cfg *config.Config) []string {
	if lib != nil && lib.ExcludeMode == "custom" {
		return parseKeywords(lib.Excludes)
	}
	if cfg != nil {
		return cfg.ExcludeKeywords
	}
	return config.DefaultExcludeKeywords()
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

// excludeMatcher compiles the keywords into one pattern.
//
// A keyword written in latin letters matches a whole word, allowing a trailing
// number: "pv" excludes "[PV1]" and "[PV]" but not "Spy×Family", and "cm" does
// not eat a show whose name happens to contain those letters. Anything else —
// Chinese, or a keyword with punctuation — matches as a plain substring, because
// those have no word boundaries to speak of.
func excludeMatcher(keywords []string) *regexp.Regexp {
	var parts []string
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		q := regexp.QuoteMeta(k)
		if isLatinWord(k) {
			parts = append(parts, `(?:^|[^a-z0-9])`+q+`s?\d*(?:$|[^a-z0-9])`)
		} else {
			parts = append(parts, q)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	re, err := regexp.Compile(`(?i)` + strings.Join(parts, "|"))
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

// dropExcluded removes the files this library does not accept. Cleanup then sees
// them as gone, which is how a newly added keyword takes an already imported file
// back out of the library.
func dropExcluded(files []fileRec, re *regexp.Regexp) []fileRec {
	if re == nil || len(files) == 0 {
		return files
	}
	out := make([]fileRec, 0, len(files))
	for _, fr := range files {
		if re.MatchString(baseName(fr.Path)) {
			continue
		}
		out = append(out, fr)
	}
	return out
}
