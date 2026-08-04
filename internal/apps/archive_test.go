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

// seedBackupDirs creates directories under one app's backup folder.
func seedBackupDirs(t *testing.T, backupsDir, app string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(backupsDir, app, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListBackupsParsesStampsAndIgnoresOthers(t *testing.T) {
	backupsDir := t.TempDir()
	seedBackupDirs(t, backupsDir, "jellyfin",
		"2026-07-10_153045",          // a folder archive
		"2026-06-02_090000",          // an older one
		"2026-07-10_153045.zip",      // a *directory* wearing a zip name
		".staging-2026-07-12_101010", // an interrupted snapshot — inert, never offered
		"2026-13-99_999999",          // impossible date
		"notes",                      // something else entirely
	)
	seedBackupDirs(t, backupsDir, "sonarr", "2026-07-10_153045") // a different app
	// A real zip archive (a file, as it must be).
	if err := os.WriteFile(filepath.Join(backupsDir, "jellyfin", "2026-07-11_120000.zip"), []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ListBackups(backupsDir, "jellyfin")
	var names []string
	for _, b := range got {
		names = append(names, b.Name)
	}
	want := []string{ // newest first
		"2026-07-11_120000.zip",
		"2026-07-10_153045",
		"2026-06-02_090000",
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
	if got[1].Date != "2026-07-10" || got[1].App != "jellyfin" {
		t.Errorf("backup = %+v, want Date=2026-07-10 App=jellyfin", got[1])
	}
}

func TestMeasureSizesFolderArchivesAndLeavesZipsAlone(t *testing.T) {
	backupsDir := t.TempDir()
	seedApp(t, filepath.Join(backupsDir, "jellyfin", "2026-07-10_153045"))
	if err := os.WriteFile(filepath.Join(backupsDir, "jellyfin", "2026-07-11_120000.zip"), []byte("PK123"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The cheap list is the store's install-click path: it must stay a stat-only
	// read, so a folder archive comes back unmeasured.
	cheap := ListBackups(backupsDir, "jellyfin")
	if len(cheap) != 2 {
		t.Fatalf("ListBackups = %+v; want two", cheap)
	}
	for _, b := range cheap {
		if !b.Zip && b.Size != 0 {
			t.Errorf("ListBackups measured a folder (%+v); that walk belongs in Measure", b)
		}
	}

	sized := Measure(backupsDir, ListBackups(backupsDir, "jellyfin"))
	byName := map[string]Backup{}
	for _, b := range sized {
		byName[b.Name] = b
	}
	// seedApp writes "services: {}\n" (13) + "PUID=1000\n" (10) + "rows" (4),
	// nested one directory deep — so this also pins that the walk recurses.
	const want = 13 + 10 + 4
	if got := byName["2026-07-10_153045"].Size; got != want {
		t.Errorf("folder archive size = %d, want %d", got, want)
	}
	// A zip was already sized by the stat; Measure must not touch it.
	if got := byName["2026-07-11_120000.zip"].Size; got != 5 {
		t.Errorf("zip archive size = %d, want 5 (unchanged by Measure)", got)
	}
}

func TestListAllMeasuresFoldersAndMarksOrphans(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	seedApp(t, filepath.Join(appsDir, "jellyfin")) // installed
	// sonarr has archives but no app folder: uninstalled, and reachable only here.
	seedApp(t, filepath.Join(backupsDir, "sonarr", "2026-07-10_153045"))
	seedApp(t, filepath.Join(backupsDir, "jellyfin", "2026-07-11_120000"))

	got := ListAll(backupsDir, appsDir)
	if len(got) != 2 {
		t.Fatalf("ListAll = %+v; want jellyfin and sonarr", got)
	}
	if got[0].App != "jellyfin" || got[0].Orphan {
		t.Errorf("jellyfin = %+v; want Orphan=false", got[0])
	}
	if got[1].App != "sonarr" || !got[1].Orphan {
		t.Errorf("sonarr = %+v; want Orphan=true", got[1])
	}
	// Unlike ListBackups, folder archives are measured here — the page exists to
	// show what is eating the disk.
	if got[0].Total == 0 || got[0].Backups[0].Size == 0 {
		t.Errorf("folder archive left unmeasured: %+v", got[0])
	}
}

func TestRestoreBackupFolderIsARename(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	archive := filepath.Join(backupsDir, "jellyfin", "2026-07-10_153045")
	seedApp(t, archive)

	if err := RestoreBackup(backupsDir, appsDir, "jellyfin", "2026-07-10_153045"); err != nil {
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
	if len(ListBackups(backupsDir, "jellyfin")) != 0 {
		t.Errorf("restored folder archive still listed as a backup")
	}
}

func TestRestoreBackupZipRoundTripKeepsTheZip(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	src := filepath.Join(appsDir, "jellyfin")
	seedApp(t, src)
	if err := os.MkdirAll(filepath.Join(backupsDir, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Archive exactly the way Uninstall(zip=true) does, then drop the folder.
	name := "2026-07-10_153045.zip"
	if err := archiveDir(src, filepath.Join(backupsDir, "jellyfin", name), nil); err != nil {
		t.Fatalf("archiveDir: %v", err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup(backupsDir, appsDir, "jellyfin", name); err != nil {
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
	if got := ListBackups(backupsDir, "jellyfin"); len(got) != 1 || got[0].Name != name {
		t.Errorf("zip backup should survive a restore, got %+v", got)
	}
}

func TestRestoreBackupRefusesLiveAppAndBadNames(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	seedApp(t, filepath.Join(appsDir, "jellyfin")) // already installed
	seedBackupDirs(t, backupsDir, "jellyfin", "2026-07-10_153045")

	if err := RestoreBackup(backupsDir, appsDir, "jellyfin", "2026-07-10_153045"); err == nil {
		t.Error("restoring over a live app should fail")
	}

	// Names that are not archives are rejected before any filesystem work, and no
	// app or archive name can reach outside the backups tree.
	for _, c := range []struct{ app, name string }{
		{"jellyfin", "notes"},                 // not a stamp
		{"jellyfin", "2026-07-10_153045/db"},  // not a base name
		{"jellyfin", "../../etc/passwd"},      // traversal via the name
		{"..", "2026-07-10_153045"},           // traversal via the app
		{".backups", "2026-07-10_153045"},     // the backups dir itself
		{"jellyfin", ".staging-2026-07-10_1"}, // an in-flight snapshot
	} {
		if err := RestoreBackup(backupsDir, appsDir, c.app, c.name); err == nil {
			t.Errorf("RestoreBackup(%q, %q) should fail", c.app, c.name)
		}
	}
}

func TestDeleteBackupOnlyReachesArchives(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	seedApp(t, filepath.Join(appsDir, "jellyfin"))
	seedBackupDirs(t, backupsDir, "jellyfin", "2026-07-10_153045")

	// The live app is not reachable through the delete path, however it is named.
	for _, c := range []struct{ app, name string }{
		{"jellyfin", "../../jellyfin"},
		{"jellyfin", "notes"},
		{"..", "2026-07-10_153045"},
	} {
		if err := DeleteBackup(backupsDir, c.app, c.name); err == nil {
			t.Errorf("DeleteBackup(%q, %q) should fail", c.app, c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(appsDir, "jellyfin", ".env")); err != nil {
		t.Fatalf("live app was damaged by a rejected delete: %v", err)
	}

	if err := DeleteBackup(backupsDir, "jellyfin", "2026-07-10_153045"); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if len(ListBackups(backupsDir, "jellyfin")) != 0 {
		t.Error("archive still listed after delete")
	}
}

func TestExtractZipRejectsTraversalEntries(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	backupsDir := filepath.Join(appsDir, ".backups")
	if err := os.MkdirAll(filepath.Join(backupsDir, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	name := "2026-07-10_153045.zip"
	if err := os.WriteFile(filepath.Join(backupsDir, "jellyfin", name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup(backupsDir, appsDir, "jellyfin", name); err == nil {
		t.Fatal("extracting a traversal entry should fail")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); !os.IsNotExist(err) {
		t.Error("zip-slip wrote outside the app dir")
	}
}
