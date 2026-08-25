package installer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/stackup"
)

// StoreSeedDir is the folder a store app ships its initial data tree in. Its
// layout mirrors the app directory, so a path in the store is the path on disk
// and the store declares a file by putting it where it goes.
const StoreSeedDir = "seed"

// writeSeed copies a store app's seed tree into the app folder, as
// stackup.SeedDir.
//
// The copy is the point. Maison could re-read the store's cache on every up, but
// then an app folder would only work while the store it came from was still
// configured and still extracted — and `docker compose up -d` run by hand in the
// folder would behave differently from an install. The compose file is copied
// byte-for-byte for exactly this reason (docs/app-model.md); the seed tree gets
// the same treatment, which also means the backup archives it.
//
// The destination is replaced wholesale rather than merged, so an update that
// drops a file from the store's tree drops it here too. That is safe because
// nothing ever writes into SeedDir: it holds the store's bytes, and the app's own
// copies of those files live outside it, where stackup.EnsureSeed puts them.
func writeSeed(app *appstore.CatalogApp, appDir string) error {
	dest := filepath.Join(appDir, stackup.SeedDir)
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("clear %s: %w", stackup.SeedDir, err)
	}
	if app == nil {
		return nil
	}
	src := filepath.Join(app.Dir(), StoreSeedDir)
	if _, err := os.Stat(src); err != nil {
		return nil // the app ships no seed tree — the common case
	}
	if err := copyTree(src, dest); err != nil {
		return fmt.Errorf("copy %s: %w", StoreSeedDir, err)
	}
	return nil
}

// copyTree copies a directory recursively, carrying the exec bit and nothing
// else (appstore.extractZip has already clamped store modes to 0644/0755).
//
// Symlinks are refused rather than followed or recreated: the source is an
// unpacked download, and a link in it can name anything on this filesystem.
func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: symlinks are not allowed", rel)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info, err := d.Info(); err == nil && info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, b, mode); err != nil {
			return err
		}
		// WriteFile applies the umask; pin the mode so an executable seed script
		// arrives executable.
		return os.Chmod(target, mode)
	})
}
