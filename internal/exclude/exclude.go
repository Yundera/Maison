// Package exclude models the directories an app declares as derived — regenerable
// data that Maison may leave out of its backups — and answers the two different
// questions the backup engines ask about them.
//
// It is one package because the engines must agree. The local engine walks the app
// folder itself and needs to ask "does this path match"; kopia never walks anything
// and needs "what are the ignore rules" to push into the repository as policy. If
// each derived its own answer from the raw patterns, the same app would back up
// different contents depending on which engine was selected — and the difference
// would only ever be discovered during a restore.
//
// The accepted syntax is deliberately far smaller than gitignore's: an app-root
// relative directory, or a directory name at any depth. Two forms are what the real
// store apps need, and two forms are what two independent engines can be trusted to
// implement identically. See docs/x-compose-app.md.
package exclude

import (
	"fmt"
	"path"
	"strings"
)

// rule is one parsed exclusion. Exactly one of the fields is set: path for the
// anchored form (`cache/`, `data/transcodes/`), name for the any-depth form
// (`**/thumbs/`).
//
// Every rule names a DIRECTORY, and excluding a directory excludes its whole
// subtree. There is no file-level rule, which is why Match needs no notion of
// what kind of entry it is looking at.
type rule struct {
	path string // app-root-relative, slash-separated, no trailing slash
	name string // a single directory name, matched at any depth
}

// Set is a parsed exclusion list. A nil *Set excludes nothing, which is what every
// app that declares nothing gets — so callers never have to guard the pointer.
type Set struct {
	rules []rule
}

// Parse turns declared patterns into a Set, together with one error per pattern it
// refused.
//
// It never fails as a whole, and that is the design: a refused pattern means the
// backup carries MORE than the author asked it to, which is never data loss, while
// refusing the app's whole declaration over one typo would be. The errors are for
// showing — see the backup dialog, which lists them next to what was excluded, so a
// mistake in a store app is visible rather than silent.
//
// appDir is the app's own folder as seen inside this container; it is what an
// absolute pattern is checked against. Pass "" to refuse absolute patterns outright.
// Patterns must already be interpolated — a surviving `$` is refused, exactly as
// stackup.resolvePath refuses one in a folder path.
func Parse(patterns []string, appDir string) (*Set, []error) {
	var (
		s    Set
		errs []error
		seen = map[string]bool{}
	)
	for _, raw := range patterns {
		r, err := parseOne(raw, appDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if key := r.canonical(); !seen[key] {
			seen[key] = true
			s.rules = append(s.rules, r)
		}
	}
	if len(s.rules) == 0 {
		return nil, errs
	}
	return &s, errs
}

func parseOne(raw, appDir string) (rule, error) {
	p := strings.TrimSpace(raw)
	switch {
	case p == "":
		return rule{}, fmt.Errorf("empty exclusion")
	case strings.Contains(p, "$"):
		// Same rule, and the same reason, as an unresolved variable in a folder path:
		// what it would match is anybody's guess, and guessing here loses data.
		return rule{}, fmt.Errorf("%q: unresolved variable", raw)
	case strings.HasPrefix(p, "!"):
		return rule{}, fmt.Errorf("%q: negation is not supported", raw)
	}

	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return rule{}, fmt.Errorf("%q: the app folder itself cannot be excluded", raw)
	}

	if rest, ok := strings.CutPrefix(p, "**/"); ok {
		if rest == "" || rest == "." || rest == ".." || strings.ContainsAny(rest, "/*?[") {
			return rule{}, fmt.Errorf("%q: **/ takes one directory name, as in **/cache/", raw)
		}
		return rule{name: rest}, nil
	}
	if strings.ContainsAny(p, "*?[") {
		return rule{}, fmt.Errorf("%q: wildcards are supported only as **/<name>/", raw)
	}

	// The absolute form is accepted because it is the spelling an author already
	// types in `folders:`, and copying one across is the mistake to expect. It is
	// only ever accepted inside this app's own folder: an app has no business
	// declaring anything about another app's data, or about the data root.
	if strings.HasPrefix(p, "/") {
		root := path.Clean(appDir)
		if appDir == "" || root == "." || root == "/" {
			return rule{}, fmt.Errorf("%q: use a path relative to the app folder", raw)
		}
		clean := path.Clean(p)
		if clean == root {
			return rule{}, fmt.Errorf("%q: the app folder itself cannot be excluded", raw)
		}
		if !strings.HasPrefix(clean, root+"/") {
			return rule{}, fmt.Errorf("%q: outside the app folder (%s)", raw, root)
		}
		return rule{path: strings.TrimPrefix(clean, root+"/")}, nil
	}

	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return rule{}, fmt.Errorf("%q: outside the app folder", raw)
	}
	return rule{path: clean}, nil
}

// Empty reports whether this Set excludes nothing.
func (s *Set) Empty() bool { return s == nil || len(s.rules) == 0 }

// Match reports whether an app-root-relative path is excluded. rel is
// slash-separated and names either an excluded directory or anything beneath one —
// a walk that skips the directory never asks about its contents, but a size
// measurement that visits files does.
func (s *Set) Match(rel string) bool {
	if s.Empty() {
		return false
	}
	rel = strings.Trim(strings.TrimPrefix(rel, "./"), "/")
	if rel == "" || rel == "." {
		return false
	}
	for _, r := range s.rules {
		if r.name == "" {
			if rel == r.path || strings.HasPrefix(rel, r.path+"/") {
				return true
			}
			continue
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == r.name {
				return true
			}
		}
	}
	return false
}

// Patterns is the canonical spelling of what this Set excludes — what the UI shows.
// It is derived from the parsed rules rather than echoed from the declaration, so
// what a user is told can never drift from what is applied.
func (s *Set) Patterns() []string {
	if s.Empty() {
		return nil
	}
	out := make([]string, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, r.canonical())
	}
	return out
}

// Rules is the same list as kopia ignore rules: anchored at the source root for the
// relative form, and left unanchored for the any-depth one. Both keep the trailing
// slash, which is what makes kopia read them as directories.
func (s *Set) Rules() []string {
	if s.Empty() {
		return nil
	}
	out := make([]string, 0, len(s.rules))
	for _, r := range s.rules {
		if r.name != "" {
			out = append(out, "**/"+r.name+"/")
			continue
		}
		out = append(out, "/"+r.path+"/")
	}
	return out
}

func (r rule) canonical() string {
	if r.name != "" {
		return "**/" + r.name + "/"
	}
	return r.path + "/"
}
