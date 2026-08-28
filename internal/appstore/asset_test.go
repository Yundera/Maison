package appstore

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// assetStoreZip builds a store whose one app declares assets exactly as written
// in `declared` (a YAML fragment for the x-casaos block) and ships the files in
// `files`, keyed by their path inside the app's folder.
func assetStoreZip(t *testing.T, appName, declared string, files map[string]string) []byte {
	t.Helper()
	compose := "name: " + appName + "\n" +
		"services:\n" +
		"  app:\n" +
		"    image: example/" + appName + ":1\n" +
		"x-casaos:\n" +
		"  main: app\n" +
		"  title:\n" +
		"    en_us: " + appName + "\n" +
		declared

	base := "AppStore-main/" + DefaultAppsPath + "/" + appName + "/"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(base+"docker-compose.yml", compose)
	add("AppStore-main/store.json", `{"name": "Test Store"}`)
	for name, body := range files {
		add(base+name, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// refreshed builds a Manager over one store and syncs it.
func refreshed(t *testing.T, zipBody []byte) (*Manager, string) {
	t.Helper()
	srv := newStoreServer(t, `"v1"`, zipBody)
	m := New([]string{srv.URL}, t.TempDir())
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, CanonicalURL(srv.URL)
}

// The point of the feature: a store that ships its art beside the compose says so
// with a filename, and the catalog hands the dashboard a URL that reads it off
// this box — no third party in the path of a tile.
func TestCatalogResolvesRelativeAssetsToLocalURLs(t *testing.T) {
	declared := "  icon: icon.png\n" +
		"  thumbnail: thumbnail.png\n" +
		"  screenshot_link:\n" +
		"    - screenshot-1.png\n" +
		"    - assets/screenshot-2.png\n"
	m, storeURL := refreshed(t, assetStoreZip(t, "demo", declared, map[string]string{
		"icon.png":                "ICON",
		"thumbnail.png":           "THUMB",
		"screenshot-1.png":        "SHOT1",
		"assets/screenshot-2.png": "SHOT2",
	}))

	app, _, err := m.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	q := "?store=" + url.QueryEscape(storeURL)
	for _, c := range []struct{ got, want string }{
		{app.Icon, "/api/store/demo/asset/icon.png" + q},
		{app.Thumbnail, "/api/store/demo/asset/thumbnail.png" + q},
		{app.Screenshots[0], "/api/store/demo/asset/screenshot-1.png" + q},
		{app.Screenshots[1], "/api/store/demo/asset/assets/screenshot-2.png" + q},
	} {
		if c.got != c.want {
			t.Errorf("asset URL = %q, want %q", c.got, c.want)
		}
	}
	// The installer copies the file, so the name it was declared under has to
	// survive the rewrite.
	if app.IconRel() != "icon.png" {
		t.Errorf("IconRel = %q, want the declared filename", app.IconRel())
	}
}

// Absolute URLs stay exactly as written. Every store declares them today, and a
// compose pointing at somebody's public logo is still a reasonable thing to write.
func TestCatalogLeavesAbsoluteURLsAlone(t *testing.T) {
	const iconURL = "https://cdn.example/gh/Store@main/Apps/demo/icon.png"
	m, _ := refreshed(t, assetStoreZip(t, "demo", "  icon: "+iconURL+"\n", nil))

	app, _, err := m.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if app.Icon != iconURL {
		t.Errorf("Icon = %q, want the URL untouched", app.Icon)
	}
	if app.IconRel() != "" {
		t.Errorf("IconRel = %q, want none — there is no file to copy", app.IconRel())
	}
}

// A value that is neither a URL nor a name inside the app's folder is dropped, not
// passed on: emitted into an <img src> it would be a request to the dashboard for
// a path that means nothing there.
func TestCatalogDropsAnUnresolvableAsset(t *testing.T) {
	declared := "  icon: ../../secret.png\n" +
		"  screenshot_link:\n" +
		"    - good.png\n" +
		"    - /etc/hosts.png\n"
	m, _ := refreshed(t, assetStoreZip(t, "demo", declared, map[string]string{"good.png": "OK"}))

	app, _, err := m.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if app.Icon != "" {
		t.Errorf("Icon = %q, want it dropped", app.Icon)
	}
	if len(app.Screenshots) != 1 || !strings.HasSuffix(strings.SplitN(app.Screenshots[0], "?", 2)[0], "/good.png") {
		t.Errorf("Screenshots = %v, want only the resolvable one", app.Screenshots)
	}
}

// The read half: the file behind a rewritten URL is served out of the extracted
// store tree.
func TestOpenAssetReadsTheFileBesideTheCompose(t *testing.T) {
	m, storeURL := refreshed(t, assetStoreZip(t, "demo", "  icon: icon.png\n",
		map[string]string{"icon.png": "ICON", "assets/shot.png": "SHOT"}))

	for _, c := range []struct{ rel, want string }{
		{"icon.png", "ICON"},
		{"assets/shot.png", "SHOT"},
	} {
		f, st, err := m.OpenAsset(NewRef(storeURL, "", "demo"), c.rel)
		if err != nil {
			t.Fatalf("OpenAsset(%q): %v", c.rel, err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != c.want {
			t.Errorf("OpenAsset(%q) = %q, want %q", c.rel, b, c.want)
		}
		if st.Size() != int64(len(c.want)) {
			t.Errorf("OpenAsset(%q): size = %d, want %d", c.rel, st.Size(), len(c.want))
		}
	}

	// The merged catalog answers too, for a reference that names no store.
	f, _, err := m.OpenAsset(NewRef("", "", "demo"), "icon.png")
	if err != nil {
		t.Fatalf("merged OpenAsset: %v", err)
	}
	f.Close()
}

// Both halves of the request are attacker-controlled once a deep link is a URL
// anyone can type: the asset name and the app id are each joined onto a directory.
func TestOpenAssetStaysInsideTheAppFolder(t *testing.T) {
	m, storeURL := refreshed(t, assetStoreZip(t, "demo", "", map[string]string{"icon.png": "ICON"}))

	for _, c := range []struct{ id, rel string }{
		{"demo", "../../store.json"},
		{"demo", "/etc/hosts.png"},
		{"demo", "icon.png/../../../store.json"},
		{"../demo", "icon.png"},
		{"..", "icon.png"},
		{"", "icon.png"},
	} {
		f, _, err := m.OpenAsset(NewRef(storeURL, "", c.id), c.rel)
		if err == nil {
			f.Close()
			t.Errorf("OpenAsset(id=%q, rel=%q) succeeded; want it refused", c.id, c.rel)
		}
	}
}

// An asset request is an <img> the page emitted, not a link somebody followed: it
// must never trigger the tens-of-MB download that every other read of an unknown
// store is allowed to.
func TestOpenAssetNeverFetches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(nil, t.TempDir())
	if f, _, err := m.OpenAsset(NewRef(srv.URL, "", "demo"), "icon.png"); err == nil {
		f.Close()
		t.Fatal("OpenAsset succeeded for a store that was never extracted")
	}
	if hits != 0 {
		t.Fatalf("the store was fetched %d times to answer an asset request", hits)
	}
}

// The URL has to round-trip: what AssetURL writes is what storeRef + OpenAsset
// read back, including a store this box has to be told about.
func TestAssetURLCarriesTheStore(t *testing.T) {
	ref := NewRef("git.example.test/g/p/-/archive/main.zip", "custom/Apps", "demo")
	got := AssetURL(ref, "assets/shot 1.png")
	want := "/api/store/demo/asset/assets/shot%201.png" +
		"?apps_path=" + url.QueryEscape("custom/Apps") +
		"&store=" + url.QueryEscape("https://git.example.test/g/p/-/archive/main.zip")
	if got != want {
		t.Errorf("AssetURL = %q, want %q", got, want)
	}
	// The default apps folder is left out — it would say nothing.
	if got := AssetURL(NewRef("example.test/s.zip", DefaultAppsPath, "demo"), "icon.png"); strings.Contains(got, "apps_path") {
		t.Errorf("AssetURL = %q, want no apps_path for the default layout", got)
	}
}
