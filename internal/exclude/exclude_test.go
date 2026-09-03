package exclude

import (
	"strings"
	"testing"
)

const appDir = "/DATA/AppData/jellyfin"

// A refused pattern must cost the app nothing but the pattern. Parse reports it and
// returns a Set that still applies every pattern it did understand — the alternative,
// failing the whole declaration, turns one typo in a store app into a backup that
// carries data the author asked to skip, with no way to tell from the outside.
func TestParseRefusesBadPatternsWithoutLosingTheGoodOnes(t *testing.T) {
	bad := []string{
		"",                          // empty
		"/",                         // the app folder itself
		"cache/../..",               // escapes
		"../sibling",                // escapes
		"${DATA_ROOT}/x",            // never interpolated
		"!keep",                     // negation
		"*.log",                     // file glob
		"**/a/b",                    // **/ takes one name
		"/DATA/AppData/other/cache", // another app's folder
		"/DATA/Documents",           // outside every app
	}
	patterns := append([]string{"cache/"}, bad...)
	patterns = append(patterns, "**/thumbs/")

	set, errs := Parse(patterns, appDir)
	if len(errs) != len(bad) {
		t.Fatalf("Parse returned %d errors, want %d: %v", len(errs), len(bad), errs)
	}
	if got, want := strings.Join(set.Patterns(), " "), "cache/ **/thumbs/"; got != want {
		t.Fatalf("Patterns() = %q, want %q", got, want)
	}
}

// The two accepted forms, and the absolute spelling an author copies out of
// `folders:`, must all land on the same rule — otherwise the same declaration means
// different things depending on how it was written.
func TestParseNormalisesEveryAcceptedSpelling(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cache", "cache/"},
		{"cache/", "cache/"},
		{"  cache/  ", "cache/"},
		{"data/transcodes/", "data/transcodes/"},
		{"./data/transcodes", "data/transcodes/"},
		{"/DATA/AppData/jellyfin/cache", "cache/"},
		{"/DATA/AppData/jellyfin/data/transcodes/", "data/transcodes/"},
		{"**/thumbs/", "**/thumbs/"},
		{"**/thumbs", "**/thumbs/"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			set, errs := Parse([]string{c.in}, appDir)
			if len(errs) != 0 {
				t.Fatalf("Parse(%q) refused it: %v", c.in, errs)
			}
			if got := set.Patterns(); len(got) != 1 || got[0] != c.want {
				t.Fatalf("Parse(%q).Patterns() = %v, want [%q]", c.in, got, c.want)
			}
		})
	}
}

// Match is what the local engine walks with, so an anchored rule must not match the
// same name deeper in the tree and an any-depth rule must.
func TestMatch(t *testing.T) {
	set, errs := Parse([]string{"cache/", "data/transcodes/", "**/thumbs/"}, appDir)
	if len(errs) != 0 {
		t.Fatalf("Parse: %v", errs)
	}
	cases := map[string]bool{
		"cache":                    true,
		"cache/blob":               true,
		"cache/a/b/c.bin":          true,
		"data/transcodes":          true,
		"data/transcodes/x.mp4":    true,
		"thumbs":                   true,  // any-depth matches at the root too
		"media/library/thumbs/1.j": true,  // and at depth
		"db/cache.sqlite":          false, // a file that merely starts with the name
		"config/cache-policy.yml":  false,
		"data/library":             false,
		"data":                     false, // a parent of an excluded path is not excluded
		"":                         false,
		".":                        false,
	}
	for rel, want := range cases {
		if got := set.Match(rel); got != want {
			t.Errorf("Match(%q) = %v, want %v", rel, got, want)
		}
	}
}

// A nil Set is what an app declaring nothing gets, and every caller holds it without
// a guard — so it has to answer all four questions.
func TestNilSetExcludesNothing(t *testing.T) {
	var s *Set
	if !s.Empty() || s.Match("cache") || s.Patterns() != nil || s.Rules() != nil {
		t.Fatal("a nil Set must be empty, match nothing, and list nothing")
	}
}

// Kopia reads a leading slash as "anchored at the source root" and a trailing one as
// "a directory". Both matter: without the anchor `cache/` would also exclude a
// `cache` directory the app keeps somewhere else, which is precisely what Match does
// NOT do — and the two engines have to agree.
func TestRulesAreAnchoredForKopia(t *testing.T) {
	set, _ := Parse([]string{"cache/", "data/transcodes/", "**/thumbs/"}, appDir)
	got := strings.Join(set.Rules(), " ")
	want := "/cache/ /data/transcodes/ **/thumbs/"
	if got != want {
		t.Fatalf("Rules() = %q, want %q", got, want)
	}
}

// The same directory declared twice, in two spellings, is one rule — a duplicate
// would otherwise reach kopia's policy twice and read as a mistake in the UI.
func TestDuplicatesCollapse(t *testing.T) {
	set, errs := Parse([]string{"cache", "cache/", "/DATA/AppData/jellyfin/cache"}, appDir)
	if len(errs) != 0 {
		t.Fatalf("Parse: %v", errs)
	}
	if got := set.Patterns(); len(got) != 1 {
		t.Fatalf("Patterns() = %v, want one rule", got)
	}
}
