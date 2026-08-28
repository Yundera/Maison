package appicon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/asset"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The ordinary case: the app names its icon beside its compose, and the copy is a
// local read of a file already on this disk.
func TestWriteCopiesTheFileTheComposeNames(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(store, "icon.png"), "PNG-BYTES")

	if err := Write(context.Background(), appDir, store, "icon.png", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(appDir, FileBase+".png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PNG-BYTES" {
		t.Fatalf("icon = %q, want the store's bytes", got)
	}
	if p := Path(appDir); p != filepath.Join(appDir, FileBase+".png") {
		t.Fatalf("Path = %q, want the copied icon", p)
	}
}

// A store is free to keep its art in a subfolder — the name is a path inside the
// app's folder, not just a filename.
func TestWriteCopiesFromASubfolder(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(store, "assets", "logo.svg"), "<svg/>")

	if err := Write(context.Background(), appDir, store, "assets/logo.svg", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(appDir, FileBase+".svg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<svg/>" {
		t.Fatalf("icon = %q, want the subfolder's bytes", got)
	}
}

// Every CasaOS-layout store ships icon.<ext> at the root of the app's folder
// whether or not the compose says so, so a compose that names no icon still gets
// one.
func TestWriteFallsBackToTheConventionalName(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(store, "icon.png"), "PNG-BYTES")

	if err := Write(context.Background(), appDir, store, "", ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != filepath.Join(appDir, FileBase+".png") {
		t.Fatalf("Path = %q, want the conventional icon", p)
	}
}

// Disk wins over the network. An app that ships its icon and *also* names a URL
// is not a reason to make a request.
func TestWritePrefersTheFileOverTheURL(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("FETCHED"))
	}))
	defer srv.Close()

	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(store, "icon.png"), "LOCAL")

	if err := Write(context.Background(), appDir, store, "icon.png", srv.URL+"/icon.png"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(appDir, FileBase+".png"))
	if string(got) != "LOCAL" {
		t.Fatalf("icon = %q, want the local file", got)
	}
	if hits != 0 {
		t.Fatalf("fetched the URL %d times with the file on disk", hits)
	}
}

// A compose that names somebody else's server ships no file to copy, so the URL
// is fetched instead. Absolute URLs stay valid — they are just no longer the norm.
func TestWriteFallsBackToFetchingTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte("<svg/>"))
	}))
	defer srv.Close()

	appDir := t.TempDir()
	// The URL carries no extension: the response's Content-Type names it.
	if err := Write(context.Background(), appDir, t.TempDir(), "", srv.URL+"/logo"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(appDir, FileBase+".svg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<svg/>" {
		t.Fatalf("icon = %q, want the fetched bytes", got)
	}
}

// An update may change the icon's format. Two .icon.* files would make Path's
// answer depend on its own ordering rather than on what was last written.
func TestWriteReplacesAnIconOfAnotherFormat(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(appDir, FileBase+".png"), "old")
	write(t, filepath.Join(store, "icon.svg"), "<svg/>")

	if err := Write(context.Background(), appDir, store, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, FileBase+".png")); !os.IsNotExist(err) {
		t.Fatalf("the old icon is still there (err = %v)", err)
	}
	if p := Path(appDir); p != filepath.Join(appDir, FileBase+".svg") {
		t.Fatalf("Path = %q, want the new icon", p)
	}
}

// An app with no icon anywhere installs like any other: no copy, no error, and
// the tile falls back to whatever its compose declares.
func TestWriteWithoutASourceKeepsTheFolderAsItIs(t *testing.T) {
	appDir := t.TempDir()
	if err := Write(context.Background(), appDir, t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("Path = %q, want none", p)
	}
}

// The name comes from a store's compose file, so it is not to be trusted with a
// path: the read stays inside the app's own folder.
func TestWriteDoesNotFollowAPathOutOfTheFolder(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(filepath.Dir(store), "secret.png"), "SECRET")

	if err := Write(context.Background(), appDir, store, "file.png/../../secret.png", ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("copied %q from outside the app folder", p)
	}
}

// A store cannot fill the data disk through the icon field.
func TestWriteRefusesAnOversizedIcon(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "icon.png"), make([]byte, asset.MaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), appDir, store, "icon.png", ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("stored %q, want the oversized icon refused", p)
	}
}
