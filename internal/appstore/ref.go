package appstore

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// A store reference addresses one app in one store:
//
//	<locator>/-/<in-zip path>
//	git.example.org/appstore/archive/main.zip/-/Apps/FileBrowser
//
// The locator is a URL whose scheme may be omitted (https is implied); the in-zip
// path is the app's directory relative to the store root, so the apps folder is
// named by the reference rather than hardcoded. Both halves are needed and
// neither can stand in for the other: a box asked for a store it has never seen
// must be told *where* it lives, which no identifier can carry, while the folder
// inside the archive is the store's layout and not the box's business.
//
// The separator is GitLab's `/-/`, and it is split on the LAST occurrence, not
// the first. A GitLab-hosted store's own archive URL contains one:
//
//	git.example.org/group/project/-/archive/main/project-main.zip/-/Apps/FileBrowser
//
// splitting that on the first `/-/` fetches `git.example.org/group/project` and
// looks for an app under `archive/...`. Wrong URL, no error — which is why
// TestParseRefSplitsOnTheLastSeparator exists.
const refSep = "/-/"

// DefaultAppsPath is the apps folder assumed when a reference names none. It is
// the CasaOS layout every store in existence uses today, so an old link and a
// configured source keep resolving unchanged.
const DefaultAppsPath = "Apps"

// Ref is a parsed store reference.
//
// A zero URL means "the merged catalog answers" — the reference names an app
// without saying which store it comes from, which is what a plain /store/<id>
// link and every pre-existing caller do.
type Ref struct {
	// URL is the canonical (scheme-bearing) store locator, or "" for the merged
	// catalog.
	URL string
	// AppsPath is the apps folder inside the archive, relative to the store root.
	// Empty means DefaultAppsPath.
	AppsPath string
	// ID is the app's directory name inside AppsPath — the catalog id.
	ID string
}

// Apps is AppsPath with the default applied.
func (r Ref) Apps() string {
	if r.AppsPath == "" {
		return DefaultAppsPath
	}
	return r.AppsPath
}

// Merged reports whether this reference resolves against the merged catalog
// rather than one pinned store.
func (r Ref) Merged() bool { return strings.TrimSpace(r.URL) == "" }

// Path renders the reference as it appears in a deep link: scheme-less, because
// the scheme is implied and its eight characters are the ugliest part of an
// otherwise readable address. Round-trips through ParseRef.
//
// Without a locator the apps folder is left off. It would be an assertion about a
// store the reference does not name — and it would rewrite every plain
// /store/<id> link into a longer one that says no more than it did.
func (r Ref) Path() string {
	if r.Merged() {
		return path.Join(r.AppsPath, r.ID)
	}
	return bareURL(r.URL) + refSep + path.Join(r.Apps(), r.ID)
}

// bareURL drops an implied scheme for display. http is kept: it is not the
// default, and a store fetched over it is one anyone on the path can replace.
func bareURL(u string) string { return strings.TrimPrefix(u, "https://") }

var hasScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://`)

// CanonicalURL turns a store locator into the one spelling everything downstream
// uses. Two spellings of one store must not become two cache directories: the
// workdir is keyed on this string, and an unlisted store is fetched once and then
// only refreshed on demand, so a second key means a second copy that silently
// goes stale on its own.
//
// A scheme-less locator gets https. Plain http therefore has to be written out in
// full, which is the intended bias: a store fetched over http on the open
// internet is a store anyone on the path can replace.
func CanonicalURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(s, "//"):
		s = "https:" + s
	case !hasScheme.MatchString(s):
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// ParseRef reads a store reference. The input is the part of a deep link after
// /store/, or the equivalent string from anywhere else.
//
// Without a separator the whole input is an in-zip path against the merged
// catalog, so "FileBrowser" keeps meaning what it has always meant.
func ParseRef(s string) Ref {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return Ref{}
	}
	if i := strings.LastIndex(s, refSep); i >= 0 {
		r := splitInZip(s[i+len(refSep):])
		r.URL = CanonicalURL(s[:i])
		return r
	}
	return splitInZip(s)
}

// ParseUserRef reads a reference a person typed or pasted, and refuses the two
// spellings ParseRef reads as something other than what was meant.
//
// ParseRef is deliberately forgiving: anything without the separator is an in-zip
// path against the merged catalog, which is what keeps a plain /store/<id> link
// working. That is right for a URL the SPA generated, and wrong for a box a person
// types into — pasting a store's archive URL on its own,
//
//	github.com/Yundera/AppStore/archive/main.zip
//
// parses as the app "main.zip" in the folder "github.com/Yundera/AppStore/archive"
// of the merged catalog. No error, and a confusing "app not found" much later. So a
// multi-segment input without the separator is rejected here, and a single segment
// (a bare app id, the merged catalog) is kept.
func ParseUserRef(s string) (Ref, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return Ref{}, errors.New("a store reference is required")
	}
	if !strings.Contains(s, refSep) {
		if strings.Contains(s, "/") {
			return Ref{}, fmt.Errorf("%q names no app: a reference is <store>%s<folder>/<app id>, e.g. github.com/Yundera/AppStore/archive/main.zip%sApps/FileBrowser", s, refSep, refSep)
		}
		return Ref{ID: s}, nil // a bare app id: the merged catalog answers
	}
	r := ParseRef(s)
	if r.URL == "" {
		return Ref{}, fmt.Errorf("%q has no store before %s", s, refSep)
	}
	if r.ID == "" {
		return Ref{}, fmt.Errorf("%q has no app id after %s", s, refSep)
	}
	return r, nil
}

// NewRef builds a reference from the pieces a query string carries.
func NewRef(storeURL, appsPath, id string) Ref {
	return Ref{
		URL:      CanonicalURL(storeURL),
		AppsPath: strings.Trim(strings.TrimSpace(appsPath), "/"),
		ID:       strings.TrimSpace(id),
	}
}

// splitInZip divides an in-zip path into its apps folder and the app directory:
// the last segment is the app, everything before it is the folder.
func splitInZip(p string) Ref {
	p = strings.Trim(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return Ref{AppsPath: p[:i], ID: p[i+1:]}
	}
	return Ref{ID: p}
}
