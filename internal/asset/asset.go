// Package asset resolves an app's declared assets — its icon, thumbnail and
// screenshots — against the folder its compose file sits in.
//
// The rule is one line: a value that is not an absolute URL names a file beside
// the compose. It reads the same on both sides of an install, because on both
// sides the compose is on disk — a store app's compose sits in the extracted
// store tree next to the icon the store ships, and an installed app's compose
// sits in the app folder next to the copy Maison took. So `icon: icon.png`
// resolves without either side knowing which one it is.
//
// This replaces addressing those files by URL. A store that ships its own art —
// which is every store, the CDN links in them point back at the store's own
// repository — was paying a round trip to a third party to read bytes it had
// already downloaded and extracted, and taking on that third party's cache, its
// outages and its link rot to do it. A box behind a filtering proxy, or with no
// egress at all, showed an app grid of blank tiles.
//
// Absolute URLs stay valid. Every store in existence declares them today, and a
// hand-written compose pointing at somebody's public logo is a reasonable thing
// to write; they are simply no longer the normal way to say where an icon is.
package asset

import (
	"net/url"
	"path"
	"strings"
)

// MaxBytes caps any single asset Maison reads into memory or serves from a store
// tree. Icons run to tens of KB and screenshots to a few hundred; this is loose
// enough for a detailed PNG and tight enough that a store cannot fill the data
// disk, or a request pin the process, through an asset field.
const MaxBytes = 4 << 20

// Exts are the image extensions Maison will store and serve, in the order a
// by-convention lookup tries them. The extension is what tells the browser how to
// render the file — it is handed to net/http, which types the response from it —
// so a name carrying anything else is not an asset as far as this package is
// concerned.
var Exts = []string{".png", ".svg", ".jpg", ".jpeg", ".webp", ".gif", ".ico", ".avif"}

// Rel reports whether raw names a file beside the compose, and returns that name
// cleaned and slash-separated.
//
// It is deliberately strict, and a value it rejects is *not* passed along to be
// tried some other way. A store that ships a broken relative path should show a
// missing image — deterministic, greppable, fixable — rather than fall through to
// a network fetch of a string that was never a URL, which is how the previous
// scheme turned a typo into a silent 404 on somebody else's server.
//
// Rejected: anything with a scheme (that is a URL, see IsURL); an absolute path,
// which addresses the box rather than the app; any path escaping the compose's
// folder; and any name whose extension is not in Exts. The escape check is the
// load-bearing one — the value comes from a store, and "../../../etc/shadow" is
// a perfectly ordinary-looking string until it is joined onto a directory.
func Rel(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsURL(raw) {
		return "", false
	}
	// A backslash is a separator on the platform half of this codebase's users and
	// not on the other; rather than guess, refuse it. Store paths are slash-separated.
	if strings.ContainsAny(raw, `\`) {
		return "", false
	}
	if strings.HasPrefix(raw, "/") {
		return "", false
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	if Ext(clean) == "" {
		return "", false
	}
	return clean, true
}

// IsURL reports whether raw is an absolute URL — anything with a scheme, not just
// http(s), because "ftp://…" is equally not a file beside the compose. Fetching is
// a separate question and stays where it was: only http(s) is ever downloaded.
func IsURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != ""
}

// Ext returns name's extension when it is one of Exts, lowercased, else "".
func Ext(name string) string {
	ext := strings.ToLower(path.Ext(name))
	for _, e := range Exts {
		if ext == e {
			return ext
		}
	}
	return ""
}
