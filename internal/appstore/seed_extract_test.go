package appstore

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// storeZipWithSeed ships an app carrying a seed tree, one file of which is
// executable — the shape Guacamole has.
func storeZipWithSeed(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	add := func(name, body string, exec bool) {
		t.Helper()
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	base := "AppStore-main/Apps/guacamole/"
	add(base+"docker-compose.yml", "name: guacamole\nservices:\n  app:\n    image: example/guacamole:1\nx-casaos:\n  main: app\n", false)
	add(base+"seed/db/002-create-admin-user.sh", "#!/bin/sh\necho hi\n", true)
	add(base+"seed/db/001-schema.sql", "CREATE TABLE x();\n", false)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The exec bit is the one mode that carries meaning in a store, and os.Create
// used to drop it — leaving an app's init script unrunnable with no declaration
// available to fix it.
func TestExtractKeepsTheExecBitAndNothingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(storeZipWithSeed(t))
	}))
	defer srv.Close()

	m := New([]string{srv.URL}, t.TempDir())
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, _, err := m.Get("guacamole")
	if err != nil {
		t.Fatal(err)
	}

	// Dir is what the installer copies the seed tree from.
	seed := filepath.Join(app.Dir(), "seed", "db")
	script, err := os.Stat(filepath.Join(seed, "002-create-admin-user.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if script.Mode().Perm() != 0o755 {
		t.Fatalf("script mode = %v, want 0755", script.Mode().Perm())
	}
	sql, err := os.Stat(filepath.Join(seed, "001-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if sql.Mode().Perm() != 0o644 {
		t.Fatalf("data file mode = %v, want 0644", sql.Mode().Perm())
	}
}

// A store is a downloaded archive, so its modes are clamped rather than honoured:
// setuid or world-writable in the zip must not survive into the cache.
func TestStoreFileModeClamps(t *testing.T) {
	cases := map[os.FileMode]os.FileMode{
		0o644:                 0o644,
		0o600:                 0o644,
		0o755:                 0o755,
		0o777:                 0o755,
		0o666:                 0o644,
		os.ModeSetuid | 0o755: 0o755,
	}
	for in, want := range cases {
		if got := storeFileMode(in); got.Perm() != want {
			t.Errorf("storeFileMode(%v) = %v, want %v", in, got, want)
		}
	}
}
