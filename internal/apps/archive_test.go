package apps

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// seedApp writes a minimal app folder: a compose file, an .env and one nested
// data file (the bit that must survive a backup round-trip).
func seedApp(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"docker-compose.yml": "services: {}\n",
		".env":               "PUID=1000\n",
		"db/data.sqlite":     "rows",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListBackupsParsesNamesAndIgnoresOthers(t *testing.T) {
	appsDir := t.TempDir()
	dirs := []string{
		"jellyfin.2026-07-10.archive",        // plain folder archive
		"jellyfin.2026-06-02.archive-153045", // same-day collision suffix
		"jellyfin",                           // the live app — not a backup
		"jellyfin-extras.2026-07-10.archive", // a different app
		"jellyfin.not-a-date.archive",        // malformed date
		"jellyfin.2026-07-10.archive.zip",    // a *directory* wearing a zip name
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(appsDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A real zip archive (a file, as it must be).
	if err := os.WriteFile(filepath.Join(appsDir, "jellyfin.2026-07-11.archive.zip"), []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ListBackups(appsDir, "jellyfin")
	var names []string
	for _, b := range got {
		names = append(names, b.Name)
	}
	// Newest first; the dir-named-like-a-zip and the other app's archive are dropped.
	want := []string{
		"jellyfin.2026-07-11.archive.zip",
		"jellyfin.2026-07-10.archive",
		"jellyfin.2026-06-02.archive-153045",
	}
	if len(names) != len(want) {
		t.Fatalf("backups = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("backup[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	if !got[0].Zip || got[0].Size != 2 {
		t.Errorf("zip backup = %+v, want Zip=true Size=2", got[0])
	}
	if got[1].Zip || got[1].Size != 0 {
		t.Errorf("folder backup = %+v, want Zip=false Size=0 (folders are not measured)", got[1])
	}
	if got[1].Date != "2026-07-10" {
		t.Errorf("date = %q, want 2026-07-10", got[1].Date)
	}
}

func TestRestoreBackupFolderIsARename(t *testing.T) {
	appsDir := t.TempDir()
	archive := filepath.Join(appsDir, "jellyfin.2026-07-10.archive")
	seedApp(t, archive)

	if err := RestoreBackup(appsDir, "jellyfin", "jellyfin.2026-07-10.archive"); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	// Data and .env come back...
	body, err := os.ReadFile(filepath.Join(appsDir, "jellyfin", "db", "data.sqlite"))
	if err != nil || string(body) != "rows" {
		t.Fatalf("restored data = %q, %v; want %q", body, err, "rows")
	}
	if _, err := os.Stat(filepath.Join(appsDir, "jellyfin", ".env")); err != nil {
		t.Errorf(".env not restored: %v", err)
	}
	// ...and the folder archive is consumed by the rename.
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("folder archive still present after restore")
	}
	if len(ListBackups(appsDir, "jellyfin")) != 0 {
		t.Errorf("restored folder archive still listed as a backup")
	}
}

func TestRestoreBackupZipRoundTripKeepsTheZip(t *testing.T) {
	appsDir := t.TempDir()
	src := filepath.Join(appsDir, "jellyfin")
	seedApp(t, src)

	// Archive exactly the way Uninstall(zip=true) does, then drop the folder.
	zipName := "jellyfin.2026-07-10.archive.zip"
	if err := archiveDir(src, filepath.Join(appsDir, zipName), nil); err != nil {
		t.Fatalf("archiveDir: %v", err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup(appsDir, "jellyfin", zipName); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(src, "db", "data.sqlite"))
	if err != nil || string(body) != "rows" {
		t.Fatalf("restored data = %q, %v; want %q", body, err, "rows")
	}
	// Restored files must be readable — archiveDir records no mode, so a naive
	// extract would create them 0000.
	fi, err := os.Stat(filepath.Join(src, ".env"))
	if err != nil {
		t.Fatalf("stat .env: %v", err)
	}
	if fi.Mode().Perm()&0o400 == 0 {
		t.Errorf(".env restored unreadable: mode %v", fi.Mode().Perm())
	}
	// A zip restore is a copy, so the backup survives and stays listed.
	if got := ListBackups(appsDir, "jellyfin"); len(got) != 1 || got[0].Name != zipName {
		t.Errorf("zip backup should survive a restore, got %+v", got)
	}
}

func TestRestoreBackupRefusesLiveAppAndForeignNames(t *testing.T) {
	appsDir := t.TempDir()
	seedApp(t, filepath.Join(appsDir, "jellyfin")) // already installed
	if err := os.MkdirAll(filepath.Join(appsDir, "jellyfin.2026-07-10.archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup(appsDir, "jellyfin", "jellyfin.2026-07-10.archive"); err == nil {
		t.Error("restoring over a live app should fail")
	}

	// Names that are not this app's archives are rejected before any filesystem work.
	for _, name := range []string{
		"sonarr.2026-07-10.archive",      // another app's backup
		"jellyfin",                       // the app folder itself
		"../etc.2026-07-10.archive",      // traversal
		"jellyfin.2026-07-10.archive/db", // not a base name
	} {
		if err := RestoreBackup(appsDir, "jellyfin", name); err == nil {
			t.Errorf("RestoreBackup(%q) should fail", name)
		}
	}
}

func TestExtractZipRejectsTraversalEntries(t *testing.T) {
	appsDir := t.TempDir()
	// Hand-craft a malicious zip: entries are stored under a leading segment that
	// extractZip strips, so the escape has to come from the remainder.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("jellyfin/../../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(appsDir, "jellyfin.2026-07-10.archive.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup(appsDir, "jellyfin", "jellyfin.2026-07-10.archive.zip"); err == nil {
		t.Fatal("extracting a traversal entry should fail")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(appsDir), "escaped.txt")); !os.IsNotExist(err) {
		t.Error("zip-slip wrote outside the app dir")
	}
}
