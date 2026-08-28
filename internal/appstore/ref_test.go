package appstore

import "testing"

// A GitLab-hosted store's own archive URL contains the separator:
//
//	git.example.org/group/project/-/archive/main/project-main.zip
//
// Splitting a reference on the FIRST separator therefore fetches
// `git.example.org/group/project` and looks for an app under `archive/...` —
// a different URL, with no error to say so. This is the case the last-match rule
// exists for, and it is exactly the third-party-forge case the grammar is meant
// to serve, so it is not hypothetical.
func TestParseRefSplitsOnTheLastSeparator(t *testing.T) {
	r := ParseRef("git.example.org/group/project/-/archive/main/project-main.zip/-/Apps/FileBrowser")

	if want := "https://git.example.org/group/project/-/archive/main/project-main.zip"; r.URL != want {
		t.Errorf("URL = %q, want %q", r.URL, want)
	}
	if r.Apps() != "Apps" {
		t.Errorf("Apps = %q, want Apps", r.Apps())
	}
	if r.ID != "FileBrowser" {
		t.Errorf("ID = %q, want FileBrowser", r.ID)
	}
}

// Without a separator the reference names no store, so the merged catalog
// answers — which is what every /store/<id> link written before this grammar
// existed means, and must keep meaning.
func TestParseRefWithoutLocatorUsesTheMergedCatalog(t *testing.T) {
	r := ParseRef("FileBrowser")

	if !r.Merged() {
		t.Errorf("Merged() = false, want true (URL %q)", r.URL)
	}
	if r.ID != "FileBrowser" {
		t.Errorf("ID = %q, want FileBrowser", r.ID)
	}
	if r.Apps() != DefaultAppsPath {
		t.Errorf("Apps = %q, want %q", r.Apps(), DefaultAppsPath)
	}
}

// The apps folder comes from the reference, so a store that keeps its apps
// somewhere other than Apps/ is addressable without Maison being taught about it.
func TestParseRefReadsANestedAppsFolder(t *testing.T) {
	r := ParseRef("apps.example.org/store.zip/-/catalog/apps/FileBrowser")

	if r.AppsPath != "catalog/apps" {
		t.Errorf("AppsPath = %q, want catalog/apps", r.AppsPath)
	}
	if r.ID != "FileBrowser" {
		t.Errorf("ID = %q, want FileBrowser", r.ID)
	}
}

// Two spellings of one store must not become two cache directories: the workdir
// is keyed on the canonical URL, and an unlisted store is fetched once and then
// refreshed only on demand, so a second key is a second copy that goes stale on
// its own.
func TestCanonicalURLGivesOneSpellingPerStore(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git.example.org/s.zip", "https://git.example.org/s.zip"},
		{"//git.example.org/s.zip", "https://git.example.org/s.zip"},
		{"https://git.example.org/s.zip", "https://git.example.org/s.zip"},
		{"  https://GIT.example.org/s.zip  ", "https://git.example.org/s.zip"},
		// Plain http has to be asked for explicitly, and is then honoured.
		{"http://git.example.org/s.zip", "http://git.example.org/s.zip"},
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalURL(c.in); got != c.want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Path is what a deep link carries, so it has to survive the round trip — the SPA
// normalises the URL it loaded on, and a reference that changed shape on the way
// through would rewrite the address bar to something that resolves elsewhere.
func TestRefPathRoundTrips(t *testing.T) {
	for _, in := range []string{
		"git.example.org/appstore/archive/main.zip/-/Apps/FileBrowser",
		"git.example.org/group/project/-/archive/main/project-main.zip/-/Apps/FileBrowser",
		"apps.example.org/store.zip/-/catalog/apps/FileBrowser",
		"http://lan.example.org/store.zip/-/Apps/FileBrowser",
		"Apps/FileBrowser",
		// No locator, no folder: a plain catalog link must not grow one.
		"FileBrowser",
	} {
		if got := ParseRef(in).Path(); got != in {
			t.Errorf("ParseRef(%q).Path() = %q", in, got)
		}
	}
}

// NewRef is the query-string door into the same grammar: the API takes the
// locator and the folder as separate parameters, and must read a scheme-less
// locator exactly as the path form does.
func TestNewRefCanonicalisesTheLocator(t *testing.T) {
	r := NewRef("git.example.org/s.zip", "/catalog/apps/", " FileBrowser ")

	if r.URL != "https://git.example.org/s.zip" {
		t.Errorf("URL = %q", r.URL)
	}
	if r.AppsPath != "catalog/apps" {
		t.Errorf("AppsPath = %q", r.AppsPath)
	}
	if r.ID != "FileBrowser" {
		t.Errorf("ID = %q", r.ID)
	}
}

// ParseUserRef is the parse for a box a person types into, where ParseRef's
// forgiving reading of an input without the separator is a trap rather than a
// convenience.

func TestParseUserRefReadsAFullLocator(t *testing.T) {
	r, err := ParseUserRef(" github.com/Yundera/AppStore/archive/main.zip/-/Apps/FileBrowser ")
	if err != nil {
		t.Fatalf("ParseUserRef: %v", err)
	}
	if r.URL != "https://github.com/Yundera/AppStore/archive/main.zip" {
		t.Errorf("URL = %q, want the canonicalised locator", r.URL)
	}
	if r.Apps() != "Apps" || r.ID != "FileBrowser" {
		t.Errorf("apps/id = %q/%q, want Apps/FileBrowser", r.Apps(), r.ID)
	}
}

// The whole point of the strict parse: a store's archive URL on its own names no
// app, and ParseRef reads it as the app "main.zip" in a folder named after the
// forge. Failing at the box beats "app not found" minutes later.
func TestParseUserRefRejectsAStoreURLThatNamesNoApp(t *testing.T) {
	if _, err := ParseUserRef("github.com/Yundera/AppStore/archive/main.zip"); err == nil {
		t.Fatal("a locator with no app was accepted")
	}
}

// A bare id still means the merged catalog, exactly as /store/<id> always has.
func TestParseUserRefKeepsABareAppID(t *testing.T) {
	r, err := ParseUserRef("FileBrowser")
	if err != nil {
		t.Fatalf("ParseUserRef: %v", err)
	}
	if !r.Merged() || r.ID != "FileBrowser" {
		t.Errorf("ref = %+v, want the merged catalog's FileBrowser", r)
	}
}

func TestParseUserRefRejectsHalfAReference(t *testing.T) {
	for _, in := range []string{"", "   ", "/-/Apps/FileBrowser", "github.com/store.zip/-/"} {
		if _, err := ParseUserRef(in); err == nil {
			t.Errorf("ParseUserRef(%q) was accepted", in)
		}
	}
}
