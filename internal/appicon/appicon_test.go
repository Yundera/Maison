package appicon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The store ships the icon beside the app's compose file, so the copy is a local
// read of bytes already downloaded — no request to the CDN the URL points at.
func TestWriteCopiesFromTheStoreFolder(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(store, "icon.png"), "PNG-BYTES")

	url := "https://cdn.example/gh/Store@main/Apps/Demo/icon.png"
	if err := Write(context.Background(), appDir, store, url); err != nil {
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

// A store whose icon points at somebody else's server ships no file to copy, so
// the URL is fetched instead.
func TestWriteFallsBackToFetchingTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte("<svg/>"))
	}))
	defer srv.Close()

	appDir := t.TempDir()
	// The URL carries no extension: the response's Content-Type names it.
	if err := Write(context.Background(), appDir, t.TempDir(), srv.URL+"/logo"); err != nil {
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

	if err := Write(context.Background(), appDir, store, ""); err != nil {
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
	if err := Write(context.Background(), appDir, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("Path = %q, want none", p)
	}
}

// The names come from a store's compose file, so they are not to be trusted with
// a path: the read stays inside the store app's own folder.
func TestWriteDoesNotFollowAPathInTheIconURL(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(filepath.Dir(store), "secret.png"), "SECRET")

	err := Write(context.Background(), appDir, store, "file.png/../../secret.png")
	if err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("copied %q from outside the store folder", p)
	}
}

// A store cannot fill the data disk through the icon field.
func TestWriteRefusesAnOversizedIcon(t *testing.T) {
	store, appDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "icon.png"), make([]byte, maxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), appDir, store, ""); err != nil {
		t.Fatal(err)
	}
	if p := Path(appDir); p != "" {
		t.Fatalf("stored %q, want the oversized icon refused", p)
	}
}
