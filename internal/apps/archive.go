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

// StampLayout is the time format every archive is named with. Seconds are part of
// it so two archives of the same app on the same day cannot collide — which is
// what lets the name be the whole identity, with no collision suffix.
const StampLayout = "2006-01-02_150405"

// Backup is one archive of an app, living under <backupsDir>/<app>/. Archives are
// produced two ways — as the side effect of an uninstall, and on demand from the
// app's Backups tab — and the two are indistinguishable on disk by design: a
// backup is a backup, whatever created it.
type Backup struct {
	App   string `json:"app"`   // compose project the archive belongs to
	Name  string `json:"name"`  // on-disk base name: "2026-07-10_153045" or that + ".zip"
	Stamp string `json:"stamp"` // the name without its extension, YYYY-MM-DD_HHMMSS
	Date  string `json:"date"`  // YYYY-MM-DD, the stamp's day — for grouping and display
	Zip   bool   `json:"zip"`   // compressed archive rather than a plain folder
	Size  int64  `json:"size"`  // bytes; see ListBackups on when it is measured

	// Tier is where this backup actually is: "local" for an archive on disk under
	// .backups/, "remote" for one that exists only in a backup engine's repository,
	// "both" when the same (app, stamp) is in each.
	//
	// Restore and delete dispatch on this, never on which engine is currently
	// selected — otherwise switching engines would orphan everything the previous
	// one wrote. See docs/backup.md.
	Tier string `json:"tier"`

	// Engine that holds it ("local", "kopia", …). Permanent once shipped: it is how
	// an existing backup finds its way home after the user switches engines.
	Engine string `json:"engine,omitempty"`
}

// Backup tiers. A backup is identified by (App, Stamp) regardless of tier — the
// stamp is the identity, and an engine's own snapshot id is an internal detail.
const (
	TierLocal  = "local"
	TierRemote = "remote"
	TierBoth   = "both"
)

// EngineLocal is the built-in engine: archives on the data disk under .backups/.
// It needs no configuration and is always present, which is what makes it the
// default and the fallback.
const EngineLocal = "local"

// AppBackups is one app's archives, as the global Backups page groups them.
type AppBackups struct {
	App string `json:"app"`
	// Orphan marks an app that no longer exists at <appsDir>/<app> — uninstalled,
	// or never installed on this box. Its archives have no tile to hang off, so the
	// global page is the only place they can be reached from.
	Orphan  bool     `json:"orphan"`
	Backups []Backup `json:"backups"`
	Total   int64    `json:"total"` // sum of Size across Backups
}

// stampRe matches an archive's on-disk base name. Anything else in a backup
// directory (a half-written staging folder, a stray file) is not an archive and
// is ignored rather than offered for restore.
var stampRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{6})(\.zip)?$`)

// projectRe matches a compose project name we are willing to touch on disk. It is
// the traversal guard for every path built from a caller-supplied app name: no
// separators, no dots (which the app model reserves for archives and hidden
// dirs), so "..", "a/b" and ".backups" are all rejected.
var projectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// parseBackup reads an archive's on-disk name back into a Backup. ok is false for
// any name that is not an archive.
func parseBackup(app, name string) (Backup, bool) {
	m := stampRe.FindStringSubmatch(name)
	if m == nil {
		return Backup{}, false
	}
	t, err := time.Parse(StampLayout, m[1])
	if err != nil {
		return Backup{}, false
	}
	// Tier and Engine default to local because every caller here is reading an
	// on-disk archive. Remote listing reuses this function purely for its *name
	// validation* — a snapshot whose recorded stamp does not parse is dropped rather
	// than surfaced — and overwrites both fields afterwards.
	return Backup{
		App:    app,
		Name:   name,
		Stamp:  m[1],
		Date:   t.Format("2006-01-02"),
		Zip:    m[2] != "",
		Tier:   TierLocal,
		Engine: EngineLocal,
	}, true
}

// ValidProjectName reports whether name is a compose project Maison is willing to
// touch on disk. It is exported so that callers enumerating app folders — the
// nightly run, for one — use the same guard the on-disk paths do, instead of a
// second filter that drifts from it.
func ValidProjectName(name string) bool { return projectRe.MatchString(name) }

// ParseBackupName validates a backup name and returns the Backup it describes, or
// ok == false if the name is not one.
//
// It exists for engines implemented outside this package. A remote engine records
// the stamp alongside its own snapshot, and that recorded value is untrusted on the
// way back in — so every engine must re-validate it through *this* regex before it
// becomes a Backup. That is what lets the traversal guard stay exactly as strict as
// it is today while backups live somewhere other than on disk: the check moves to
// the ingress of remote listing rather than being loosened to accommodate it.
//
// Callers outside this package must set Tier and Engine themselves; the defaults
// here describe a local archive.
func ParseBackupName(app, name string) (Backup, bool) { return parseBackup(app, name) }

// AppBackupDir is where one app's archives live. It is created on demand by the
// writers; readers tolerate it being absent.
func AppBackupDir(backupsDir, project string) string {
	return filepath.Join(backupsDir, project)
}

// ListBackups returns every archive of `project`, newest first.
//
// Size is only filled in for zips, where it is a single cheap stat. Folder
// archives are left at 0 here on purpose: measuring one means walking the whole
// tree, and an archived app's tree is exactly where the bulk user data lives (a
// media library can be terabytes) — far too expensive for the store's install
// click, which only needs to know which archives exist.
//
// Wrap the result in Measure to get folder sizes; that is what the pages which
// actually display sizes do.
func ListBackups(backupsDir, project string) []Backup {
	if !projectRe.MatchString(project) {
		return nil
	}
	entries, err := os.ReadDir(AppBackupDir(backupsDir, project))
	if err != nil {
		return nil
	}
	var out []Backup
	for _, e := range entries {
		b, ok := parseBackup(project, e.Name())
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
	sortBackups(out)
	return out
}

// Measure fills in Size for the folder archives in list — one full tree walk
// each, which is why it is a separate step rather than part of ListBackups.
//
// Call it where a size is worth waiting for (the pages that show archives and
// what they cost), not on the store's install-click path, where the only
// question is which archives exist.
func Measure(backupsDir string, list []Backup) []Backup {
	for i := range list {
		if list[i].Zip {
			continue // already a single cheap stat
		}
		// A backup that exists only in a remote engine has no folder here to walk, and
		// the engine already reported its size. Walking anyway would find nothing and
		// overwrite that size with 0 — reading, on the page, as a backup that contains
		// nothing.
		if list[i].Tier == TierRemote {
			continue
		}
		list[i].Size = dirSize(filepath.Join(backupsDir, list[i].App, list[i].Name))
	}
	return list
}

// sortBackups orders newest first. The stamp is fixed-width and lexically
// ordered, so comparing names is comparing times.
func sortBackups(b []Backup) {
	sort.Slice(b, func(i, j int) bool { return b[i].Stamp > b[j].Stamp })
}

// resolveBackup validates a (project, name) pair and returns the archive's path.
// Every path this package builds from caller input goes through here, so a
// crafted app or archive name cannot escape the backups directory.
func resolveBackup(backupsDir, project, name string) (Backup, string, error) {
	if !projectRe.MatchString(project) {
		return Backup{}, "", fmt.Errorf("invalid app name: %s", project)
	}
	b, ok := parseBackup(project, name)
	if !ok {
		return Backup{}, "", fmt.Errorf("not a backup name: %s", name)
	}
	path := filepath.Join(AppBackupDir(backupsDir, project), name)
	if _, err := os.Stat(path); err != nil {
		return Backup{}, "", fmt.Errorf("backup not found: %s", name)
	}
	return b, path, nil
}

// DeleteBackup removes one archive. This is the only path that destroys data in
// Maison, and it is deliberately narrow: the name must parse as an archive stamp,
// so nothing else under the backups directory — least of all a live app folder,
// which is not even in this tree — can be reached through it.
func DeleteBackup(backupsDir, project, name string) error {
	_, path, err := resolveBackup(backupsDir, project, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// RestoreBackup puts an archive back as the app's live folder, so a normal install
// can then run over it (the installer never clobbers an existing .env, so the
// user's variables come back with their data — see installer.Install).
//
// A folder archive is renamed back, which consumes it: no copy, instant, and the
// bytes never move. A zip archive is extracted, which leaves the zip in place —
// so a zipped backup survives being restored and can be restored again.
//
// It refuses to overwrite a live app: the caller must uninstall first, or use
// Registry.StartRestore, which archives the live folder and restores in one step.
func RestoreBackup(backupsDir, appsDir, project, name string) error {
	_, src, err := resolveBackup(backupsDir, project, name)
	if err != nil {
		return err
	}
	dst := filepath.Join(appsDir, project)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s is already installed — uninstall it before restoring a backup", project)
	}
	if strings.HasSuffix(name, ".zip") {
		return extractZip(src, dst)
	}
	return os.Rename(src, dst)
}

// extractZip unpacks an archive written by archiveDir into dst. archiveDir stores
// paths under the archived folder's leaf name (jellyfin/db/x.db), so the first
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
