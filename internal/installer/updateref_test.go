package installer

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/config"
)

// Retargeting an app is the whole point of an editable reference, and the half
// that can only be checked end to end is that the reference the operator typed
// ends up being the one the *next check* resolves. So these drive SetUpdateRef
// against a real (local) store archive and then ask CheckUpdate what it sees.

// refStoreZip builds a one-app store archive, with the app's image tag chosen so
// two stores can ship the same app with different bytes.
func refStoreZip(t *testing.T, appName, tag string) []byte {
	t.Helper()
	compose := "name: " + appName + "\n" +
		"services:\n" +
		"  app:\n" +
		"    image: example/" + appName + ":" + tag + "\n" +
		"x-casaos:\n" +
		"  main: app\n" +
		"  title:\n" +
		"    en_us: " + appName + "\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("store-main/Apps/" + appName + "/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(compose)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func refStoreServer(t *testing.T, zipped []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipped)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// refInstaller is an Installer over a temp data root, with one app folder holding
// the compose bytes given (its "installed" version) and no override.
func refInstaller(t *testing.T, project, installed string) (*Installer, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "AppData", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataRoot: root}
	return &Installer{cfg: cfg, store: appstore.New(nil, filepath.Join(root, "cache"))}, dir
}

func TestSetUpdateRefPointsTheAppAtAnotherStore(t *testing.T) {
	srv := refStoreServer(t, refStoreZip(t, "jellyfin", "2"))
	in, dir := refInstaller(t, "jellyfin", "name: jellyfin\nservices:\n  app:\n    image: example/jellyfin:1\n")

	ref := srv.URL + "/store.zip/-/Apps/jellyfin"
	st, err := in.SetUpdateRef(context.Background(), "jellyfin", ref)
	if err != nil {
		t.Fatalf("SetUpdateRef: %v", err)
	}
	if !st.HasRef || st.Ref != ref {
		t.Errorf("status ref = %q (has_ref %v), want %q", st.Ref, st.HasRef, ref)
	}
	// The store this app now follows ships a different compose than the installed
	// one, so the retarget itself already answers the question the operator asked.
	if !st.Available {
		t.Error("a store shipping different bytes did not report an available update")
	}

	// And the next check resolves the same store, from the override alone.
	got, err := in.CheckUpdate(context.Background(), "jellyfin")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if got.Ref != ref || !got.Available {
		t.Errorf("check = %+v, want the new store with an update available", got)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "store-ref: "+ref) {
		t.Errorf("override does not record the new source:\n%s", raw)
	}
}

// A reference that names nothing must not be recorded: written first and resolved
// later, a typo would replace a working source with a broken one and only say so
// on the next check.
func TestSetUpdateRefKeepsTheOldSourceWhenTheNewOneDoesNotResolve(t *testing.T) {
	srv := refStoreServer(t, refStoreZip(t, "jellyfin", "2"))
	in, dir := refInstaller(t, "jellyfin", "name: jellyfin\n")

	good := srv.URL + "/store.zip/-/Apps/jellyfin"
	if _, err := in.SetUpdateRef(context.Background(), "jellyfin", good); err != nil {
		t.Fatalf("SetUpdateRef: %v", err)
	}
	if _, err := in.SetUpdateRef(context.Background(), "jellyfin", srv.URL+"/store.zip/-/Apps/Nope"); err == nil {
		t.Fatal("a reference naming an app the store does not ship was accepted")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if !strings.Contains(string(raw), "store-ref: "+good) {
		t.Errorf("a failed retarget disturbed the recorded source:\n%s", raw)
	}
}

// The strict parse belongs to the API, not just the browser: a store URL with no
// app must be refused server-side too, and refused before anything is written.
func TestSetUpdateRefRefusesALocatorWithNoApp(t *testing.T) {
	in, dir := refInstaller(t, "jellyfin", "name: jellyfin\n")
	if _, err := in.SetUpdateRef(context.Background(), "jellyfin", "github.com/Yundera/AppStore/archive/main.zip"); err == nil {
		t.Fatal("a locator naming no app was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.override.yml")); !os.IsNotExist(err) {
		t.Error("a refused reference still wrote an override")
	}
}

// An app Maison merely discovered has no reference at all. Giving it one is the
// other half of the feature: it turns an unmanaged-looking app into one that
// updates, without a reinstall.
func TestSetUpdateRefGivesAnAppWithoutASourceOne(t *testing.T) {
	srv := refStoreServer(t, refStoreZip(t, "jellyfin", "1"))
	installed := "name: jellyfin\nservices:\n  app:\n    image: example/jellyfin:1\n" +
		"x-casaos:\n  main: app\n  title:\n    en_us: jellyfin\n"
	in, _ := refInstaller(t, "jellyfin", installed)

	before, err := in.CheckUpdate(context.Background(), "jellyfin")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if before.HasRef {
		t.Fatalf("fixture already had a reference: %+v", before)
	}

	st, err := in.SetUpdateRef(context.Background(), "jellyfin", srv.URL+"/store.zip/-/Apps/jellyfin")
	if err != nil {
		t.Fatalf("SetUpdateRef: %v", err)
	}
	// The store ships exactly what is installed, so the app is now checkable and
	// up to date — the two things it could not say a moment ago.
	if !st.HasRef || st.Available {
		t.Errorf("status = %+v, want a reference with no pending update", st)
	}
}
