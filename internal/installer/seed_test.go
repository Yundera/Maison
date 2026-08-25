package installer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/stackup"
)

func TestCopyTreeKeepsExecBitAndRefusesSymlinks(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "db/init.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "db/schema.sql"), []byte("CREATE TABLE x();\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), stackup.SeedDir)
	if err := copyTree(src, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "db/init.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("init.sh mode = %v, want 0755", info.Mode().Perm())
	}

	// A link in an unpacked download can name anything on this filesystem.
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := copyTree(src, t.TempDir()); err == nil {
		t.Fatal("want an error for a symlink in a store tree")
	}
}

// The tree is replaced wholesale on update, so a file the store dropped stops
// being seeded. Nothing is lost by that: the app's own copies live outside
// SeedDir, and seeding never writes back into it.
func TestWriteSeedReplacesTheStoredTree(t *testing.T) {
	appDir := t.TempDir()
	stale := filepath.Join(appDir, stackup.SeedDir, "gone.conf")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A nil app is the "this install has no store app" case: clear, don't copy.
	if err := writeSeed(nil, appDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, stackup.SeedDir)); !os.IsNotExist(err) {
		t.Fatalf("stale seed tree survived: %v", err)
	}
}
