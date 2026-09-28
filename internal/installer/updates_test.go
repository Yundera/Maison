package installer

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/config"
)

// CheckAll is the Updates page's whole view of the box, so what matters is that
// every app lands in the right group — and that "couldn't check" never lands in
// "up to date".

// composeFor is the compose a store ships for app at tag — the same bytes an
// install would have copied in, so an app installed from it compares equal.
func composeFor(app, image string) string {
	return "name: " + app + "\n" +
		"services:\n" +
		"  app:\n" +
		"    image: " + image + "\n" +
		"x-casaos:\n" +
		"  main: app\n" +
		"  title:\n" +
		"    en_us: " + app + "\n"
}

// multiStoreZip builds a store archive holding every app given, by name → image.
func multiStoreZip(t *testing.T, apps map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, image := range apps {
		w, err := zw.Create("store-main/Apps/" + name + "/docker-compose.yml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(composeFor(name, image))); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// countingStore serves zipped and counts the requests it answers.
func countingStore(t *testing.T, zipped []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipped)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// checkBox is a data root with a store configured as a catalog source, and a way
// to drop apps into it.
type checkBox struct {
	t    *testing.T
	root string
	in   *Installer
}

func newCheckBox(t *testing.T, sources ...string) *checkBox {
	t.Helper()
	root := t.TempDir()
	store := appstore.New(sources, filepath.Join(root, "cache"))
	if len(sources) > 0 {
		if err := store.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	return &checkBox{t: t, root: root, in: &Installer{cfg: config.Config{DataRoot: root}, store: store}}
}

// app installs project with the compose given, following ref when it is not empty.
func (b *checkBox) app(project, compose, ref string) {
	b.t.Helper()
	dir := filepath.Join(b.root, "AppData", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		b.t.Fatal(err)
	}
	if ref != "" {
		if err := writeUpdateRef(dir, appstore.ParseRef(ref)); err != nil {
			b.t.Fatal(err)
		}
	}
}

func byID(rows []AppUpdate) map[string]AppUpdate {
	out := map[string]AppUpdate{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func TestCheckAllPutsEveryAppInItsGroup(t *testing.T) {
	srv, hits := countingStore(t, multiStoreZip(t, map[string]string{
		"jellyfin": "example/jellyfin:2",
		"dufs":     "example/dufs:1",
	}))
	store := srv.URL + "/store.zip"
	b := newCheckBox(t)

	b.app("jellyfin", composeFor("jellyfin", "example/jellyfin:1"), store+"/-/Apps/jellyfin")
	b.app("dufs", composeFor("dufs", "example/dufs:1"), store+"/-/Apps/dufs")
	b.app("legacy", composeFor("legacy", "example/legacy:1"), "")
	// A store nothing answers at: its app must read as "couldn't check".
	b.app("gone", composeFor("gone", "example/gone:1"), "http://127.0.0.1:1/store.zip/-/Apps/gone")

	rows := byID(b.in.CheckAll(context.Background(), []CheckTarget{
		{ID: "jellyfin", Managed: true},
		{ID: "dufs", Managed: true},
		{ID: "legacy", Managed: true},
		{ID: "gone", Managed: true},
		{ID: "portainer", Managed: false},
	}))

	for id, want := range map[string]string{
		"jellyfin":  StateAvailable,
		"dufs":      StateCurrent,
		"legacy":    StateUntracked,
		"gone":      StateError,
		"portainer": StateUnmanaged,
	} {
		if got := rows[id].State; got != want {
			t.Errorf("%s: state %q, want %q (%+v)", id, got, want, rows[id])
		}
	}
	if rows["gone"].Error == "" {
		t.Error("an unreachable store left no error to show")
	}

	img := rows["jellyfin"].Images
	if len(img) != 1 || img[0].Service != "app" || img[0].From != "example/jellyfin:1" || img[0].To != "example/jellyfin:2" {
		t.Errorf("jellyfin images = %+v, want app example/jellyfin:1 → :2", img)
	}

	// Two apps follow this store; it is downloaded once.
	if n := hits.Load(); n != 1 {
		t.Errorf("store answered %d requests for one check, want 1", n)
	}
}

// An update that changes something other than an image still is one — it just has
// no version line to show.
func TestImageChangesEmptyWhenOnlyTheDefinitionChanged(t *testing.T) {
	a := composeFor("x", "example/x:1")
	if got := imageChanges([]byte(a), []byte(a+"x-compose-app:\n  view: apps\n")); len(got) != 0 {
		t.Errorf("imageChanges = %+v, want none", got)
	}
}

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:1.27":                                "nginx",
		"docker.io/library/nginx:1.27":              "nginx",
		"ghcr.io/yundera/maison:v1.2@sha256:abcdef": "ghcr.io/yundera/maison",
		"registry:5000/team/app":                    "registry:5000/team/app",
		"Jellyfin/Jellyfin:10.10.1":                 "jellyfin/jellyfin",
	} {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggestionForAnUntrackedApp(t *testing.T) {
	srv, _ := countingStore(t, multiStoreZip(t, map[string]string{
		"jellyfin": "jellyfin/jellyfin:10.10.1",
		"dufs":     "sigoden/dufs:0.43",
	}))
	source := srv.URL + "/store.zip"
	b := newCheckBox(t, source)

	// Same project name, different image: a guess, and flagged as one.
	b.app("jellyfin", composeFor("jellyfin", "linuxserver/jellyfin:10.9"), "")
	// Different name, same image repository.
	b.app("files", composeFor("files", "sigoden/dufs:0.40"), "")
	// Neither.
	b.app("mystery", composeFor("mystery", "example/mystery:1"), "")

	rows := byID(b.in.CheckAll(context.Background(), []CheckTarget{
		{ID: "jellyfin", Managed: true},
		{ID: "files", Managed: true},
		{ID: "mystery", Managed: true},
	}))

	j := rows["jellyfin"].Suggestion
	if j == nil || j.Ref != appstore.NewRef(source, "Apps", "jellyfin").Path() || j.ImagesMatch {
		t.Errorf("jellyfin suggestion = %+v, want the store's jellyfin, images not matching", j)
	}
	f := rows["files"].Suggestion
	if f == nil || f.Ref != appstore.NewRef(source, "Apps", "dufs").Path() || !f.ImagesMatch {
		t.Errorf("files suggestion = %+v, want the store's dufs by image", f)
	}
	if m := rows["mystery"].Suggestion; m != nil {
		t.Errorf("mystery suggestion = %+v, want none", m)
	}

	// And the suggested ref is one SetUpdateRef accepts — it is what the page sends.
	if _, err := b.in.SetUpdateRef(context.Background(), "files", f.Ref); err != nil {
		t.Errorf("SetUpdateRef(%q): %v", f.Ref, err)
	}
}

// Two different store apps running the same image is a choice, not a match.
func TestSuggestionIsWithheldWhenAmbiguous(t *testing.T) {
	entry := func(id string, primary bool) catalogEntry {
		return catalogEntry{
			app:     &appstore.CatalogApp{ID: id, Name: id, StoreURL: "https://example.org/s.zip", Primary: primary},
			project: id,
			repo:    "sigoden/dufs",
		}
	}
	installed := []byte(composeFor("files", "sigoden/dufs:0.40"))

	if s := (catalogIndex{entry("dufs", true), entry("dufs-lite", true)}).suggest("files", installed); s != nil {
		t.Errorf("two distinct matches produced a suggestion: %+v", s)
	}
	// The same app in two stores is one app: the merge's winner is offered.
	if s := (catalogIndex{entry("dufs", true), entry("dufs", false)}).suggest("files", installed); s == nil {
		t.Error("one app shipped by two stores produced no suggestion")
	}
}

// The case the real store turned up: a stack named "files" running dufs, in a store
// that ships both an app called Files and Dufs. The image is the better evidence.
func TestImageMatchBeatsANameCoincidence(t *testing.T) {
	idx := catalogIndex{
		{app: &appstore.CatalogApp{ID: "Files", Name: "Files", Primary: true}, project: "files", repo: "filebrowser/filebrowser"},
		{app: &appstore.CatalogApp{ID: "Dufs", Name: "Dufs", Primary: true}, project: "dufs", repo: "sigoden/dufs"},
	}
	s := idx.suggest("files", []byte(composeFor("files", "sigoden/dufs:0.40")))
	if s == nil || s.Name != "Dufs" || !s.ImagesMatch {
		t.Errorf("suggestion = %+v, want Dufs by image", s)
	}
}

// Store apps share sidecars; only the main service identifies an app.
func TestMainRepoIgnoresSidecars(t *testing.T) {
	raw := []byte("services:\n  proxy:\n    image: ghcr.io/yundera/nginx-hash-lock:1.0.7\n" +
		"  jellyfin:\n    image: jellyfin/jellyfin:10.11\nx-casaos:\n  main: jellyfin\n")
	if got := mainRepo(raw); got != "jellyfin/jellyfin" {
		t.Errorf("mainRepo = %q, want jellyfin/jellyfin", got)
	}
	// Two services and no main: no basis for a match.
	raw = []byte("services:\n  a:\n    image: x/a:1\n  b:\n    image: x/b:1\n")
	if got := mainRepo(raw); got != "" {
		t.Errorf("mainRepo with no main = %q, want empty", got)
	}
}
