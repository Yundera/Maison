package apps

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Backup is one uninstall archive found next to the apps it was archived from —
// the read side of Uninstall (see docs/app-model.md). It is what lets the store
// offer "install from backup" instead of a fresh install.
type Backup struct {
	Name string `json:"name"` // on-disk base name, e.g. jellyfin.2026-07-10.archive.zip
	Date string `json:"date"` // YYYY-MM-DD, parsed out of the name
	Zip  bool   `json:"zip"`  // compressed archive rather than a plain renamed folder
	Size int64  `json:"size"` // bytes; only known for zips (0 for folders, see below)
}

// backupRe matches the names Uninstall produces: <project>.<date>.archive, with
// uniqueName's optional -HHMMSS collision suffix and an optional .zip.
var backupRe = regexp.MustCompile(`^(.+)\.(\d{4}-\d{2}-\d{2})\.archive(?:-\d{6})?(\.zip)?$`)

// parseBackup reads an archive's on-disk name back into a Backup. ok is false for
// any name that is not an archive of `project`.
func parseBackup(name, project string) (Backup, bool) {
	m := backupRe.FindStringSubmatch(name)
	if m == nil || m[1] != project {
		return Backup{}, false
	}
	if _, err := time.Parse("2006-01-02", m[2]); err != nil {
		return Backup{}, false
	}
	return Backup{Name: name, Date: m[2], Zip: m[3] != ""}, true
}

// ListBackups returns every archive of `project` under appsDir, newest first.
//
// Size is only filled in for zips, where it is a single cheap stat. Folder
// archives are left at 0 on purpose: measuring one means walking the whole tree,
// and an archived app's tree is exactly where the bulk user data lives (a media
// library can be terabytes) — far too expensive for a list the store panel hits
// on every app click.
func ListBackups(appsDir, project string) []Backup {
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil
	}
	var out []Backup
	for _, e := range entries {
		b, ok := parseBackup(e.Name(), project)
		if !ok {
			continue
		}
		// A folder archive must be a directory and a zip archive a file; anything
		// else wearing the name is not something we can restore.
		if b.Zip == e.IsDir() {
			continue
		}
		if b.Zip {
			if fi, err := e.Info(); err == nil {
				b.Size = fi.Size()
			}
		}
		out = append(out, b)
	}
	// Newest first. Same-day archives carry a -HHMMSS suffix, so the name itself
	// orders them within a day; a plain (suffix-less) name is the day's first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		return out[i].Name > out[j].Name
	})
	return out
}

// RestoreBackup puts a backup back as the app's live folder, so a normal install
// can then run over it (the installer never clobbers an existing .env, so the
// user's variables come back with their data — see installer.Install).
//
// A folder archive is renamed back, which consumes it: no copy, instant, and the
// bytes never move. A zip archive is extracted, which leaves the zip in place —
// so a zipped backup survives being restored and can be restored again.
//
// It refuses to overwrite a live app: the caller must uninstall first.
func RestoreBackup(appsDir, project, name string) error {
	b, ok := parseBackup(name, project)
	if !ok {
		return fmt.Errorf("not a backup of %s: %s", project, name)
	}
	src := filepath.Join(appsDir, name)
	dst := filepath.Join(appsDir, project)

	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s is already installed — uninstall it before restoring a backup", project)
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("backup not found: %s", name)
	}
	if b.Zip {
		return extractZip(src, dst)
	}
	return os.Rename(src, dst)
}

// extractZip unpacks an archive written by archiveDir into dst. archiveDir stores
// paths under the app's original leaf folder (jellyfin/db/x.db), so the first
// segment is stripped and everything lands directly in dst.
func extractZip(srcZip, dst string) error {
	zr, err := zip.OpenReader(srcZip)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		rel := strings.SplitN(filepath.ToSlash(f.Name), "/", 2)
		if len(rel) != 2 || rel[1] == "" {
			continue // the top-level folder entry itself
		}
		// Zip-slip: never let a crafted entry name escape dst.
		path := filepath.Join(dst, filepath.FromSlash(rel[1]))
		if !strings.HasPrefix(path, dst+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in backup: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := copyZipEntry(f, path); err != nil {
			return err
		}
	}
	return nil
}

func copyZipEntry(f *zip.File, path string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// archiveDir writes entries with zip.Create, which records no mode — so most
	// entries come back as 0. Fall back to a sane default rather than creating
	// unreadable files.
	mode := f.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

// archiveDir zips srcDir into dstZip, preserving the leaf directory name as the
// top-level folder inside the archive (e.g. filebrowser/db/database.db).
//
// onProgress, when non-nil, is called as bytes are read with (copied, total) —
// zipping an app's data folder is the one part of an uninstall that can run for
// minutes, so it drives the uninstall's Archive bar. The total is measured up
// front with a size-only walk (a stat per file, no reads).
func archiveDir(srcDir, dstZip string, onProgress func(copied, total int64)) error {
	if onProgress == nil {
		onProgress = func(int64, int64) {}
	}
	total := dirSize(srcDir)

	zf, err := os.Create(dstZip)
	if err != nil {
		return err
	}
	defer zf.Close()

	zw := zip.NewWriter(zf)
	defer zw.Close()

	var copied int64
	onProgress(0, total)

	base := filepath.Dir(srcDir)
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, &progressReader{r: f, onRead: func(n int64) {
			copied += n
			onProgress(copied, total)
		}})
		return err
	})
}

// progressReader reports every chunk read, so a copy can be metered without
// buffering it.
type progressReader struct {
	r      io.Reader
	onRead func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead(int64(n))
	}
	return n, err
}

// dirSize sums the size of every regular file under dir. It is a stat-only walk
// (no reads), and returns 0 for an unreadable tree — the caller then just shows
// an indeterminate archive step rather than failing.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort measurement
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
