package config

import "testing"

// A config file written by an older build still carries the stock keyword list.
// Loading it now clears that list, since the defaults are gone.
func TestLoadClearsLegacyExcludeKeywords(t *testing.T) {
	cfg := Defaults()
	cfg.ExcludeKeywords = append([]string{}, legacyExcludeKeywords...)
	cfg.normalize()
	if len(cfg.ExcludeKeywords) != 0 {
		t.Fatalf("legacy list must be cleared, got %v", cfg.ExcludeKeywords)
	}
}

// A list the user edited is theirs: only the exact stock list is cleared.
func TestLoadKeepsEditedExcludeKeywords(t *testing.T) {
	cfg := Defaults()
	cfg.ExcludeKeywords = append([]string{}, legacyExcludeKeywords...)
	cfg.ExcludeKeywords = cfg.ExcludeKeywords[:len(cfg.ExcludeKeywords)-1] // drop "sp"
	cfg.normalize()
	if len(cfg.ExcludeKeywords) != len(legacyExcludeKeywords)-1 {
		t.Fatalf("edited list must be kept, got %v", cfg.ExcludeKeywords)
	}
}

// Both lists start empty: the keywords that mean "not an episode" depend on the
// collection, and a wrong guess silently drops whole shows.
func TestDefaultsHaveNoExclusions(t *testing.T) {
	cfg := Defaults()
	if len(cfg.ExcludeKeywords) != 0 || len(cfg.ExcludeFolders) != 0 {
		t.Fatalf("defaults must be empty, got %v / %v", cfg.ExcludeKeywords, cfg.ExcludeFolders)
	}
}
