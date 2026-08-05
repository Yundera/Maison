package apps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// newLocalProvider builds the built-in engine over a temp DATA_ROOT, mirroring
// newTestRegistry so both halves of a backup are exercised the same way.
func newLocalProvider(t *testing.T) (p *LocalProvider, appsDir, backupsDir string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}
	if err := os.MkdirAll(cfg.AppsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewLocalProvider(cfg), cfg.AppsDir(), cfg.BackupsDir()
}

const testStamp = "2026-02-02_120000"

func twoPass(t *testing.T, p *LocalProvider, app string, opts SnapshotOpts) Backup {
	t.Helper()
	ctx := context.Background()
	for _, pass := range []int{1, 2} {
		o := opts
		o.Pass = pass
		if err := p.Snapshot(ctx, app, testStamp, o, nil); err != nil {
			t.Fatalf("Snapshot pass %d: %v", pass, err)
		}
	}
	b, err := p.Commit(ctx, app, testStamp, opts, nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return b
}

// A folder backup lands as .backups/<app>/<stamp> holding the app's files, and the
// staging directory is consumed by the rename rather than left behind.
func TestLocalProviderFolderBackup(t *testing.T) {
	p, appsDir, backupsDir := newLocalProvider(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	b := twoPass(t, p, "jellyfin", SnapshotOpts{})

	if b.Name != testStamp || b.Zip {
		t.Fatalf("Commit returned %+v, want name %q and Zip=false", b, testStamp)
	}
	if b.Tier != TierLocal || b.Engine != EngineLocal {
		t.Errorf("Commit returned Tier=%q Engine=%q, want %q/%q", b.Tier, b.Engine, TierLocal, EngineLocal)
	}
	if got := read(t, filepath.Join(backupsDir, "jellyfin", testStamp, "db", "data.sqlite")); got == "" {
		t.Error("archive is missing the app's data file")
	}
	if _, err := os.Stat(filepath.Join(backupsDir, "jellyfin", ".staging-"+testStamp)); !os.IsNotExist(err) {
		t.Error("staging directory survived a folder commit; the rename should have consumed it")
	}
}

// A zip backup lands as <stamp>.zip, and the staged copy — which was only the zip's
// input — is cleaned up.
func TestLocalProviderZipBackup(t *testing.T) {
	p, appsDir, backupsDir := newLocalProvider(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	b := twoPass(t, p, "jellyfin", SnapshotOpts{Zip: true})

	if b.Name != testStamp+".zip" || !b.Zip {
		t.Fatalf("Commit returned %+v, want name %q and Zip=true", b, testStamp+".zip")
	}
	if fi, err := os.Stat(filepath.Join(backupsDir, "jellyfin", testStamp+".zip")); err != nil || fi.Size() == 0 {
		t.Fatalf("zip archive missing or empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupsDir, "jellyfin", ".staging-"+testStamp)); !os.IsNotExist(err) {
		t.Error("staging directory survived a zip commit")
	}
}

// The second pass must build on the first rather than starting over — that is the
// entire reason the app's downtime is proportional to what changed rather than to
// how big it is. Asserted from the outside: pass 2 picks up a change made between
// the passes, and does not lose what pass 1 already copied.
func TestLocalProviderSecondPassIsIncremental(t *testing.T) {
	p, appsDir, backupsDir := newLocalProvider(t)
	appDir := filepath.Join(appsDir, "jellyfin")
	seedApp(t, appDir)
	ctx := context.Background()

	if err := p.Snapshot(ctx, "jellyfin", testStamp, SnapshotOpts{Pass: 1}, nil); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	staging := filepath.Join(backupsDir, "jellyfin", ".staging-"+testStamp)
	if _, err := os.Stat(filepath.Join(staging, ".env")); err != nil {
		t.Fatalf("pass 1 did not stage the app: %v", err)
	}

	// The app writes while it is still up, then is stopped for pass 2.
	if err := os.WriteFile(filepath.Join(appDir, "db", "data.sqlite"), []byte("changed-by-app"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot(ctx, "jellyfin", testStamp, SnapshotOpts{Pass: 2}, nil); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if _, err := p.Commit(ctx, "jellyfin", testStamp, SnapshotOpts{}, nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dst := filepath.Join(backupsDir, "jellyfin", testStamp)
	if got := read(t, filepath.Join(dst, "db", "data.sqlite")); got != "changed-by-app" {
		t.Errorf("pass 2 did not capture the change: data.sqlite = %q", got)
	}
	if got := read(t, filepath.Join(dst, ".env")); got == "" {
		t.Error("pass 2 lost what pass 1 had already copied")
	}
}

// A crashed backup leaves a staging directory behind. The next attempt at the same
// stamp must start from nothing rather than inherit it.
func TestLocalProviderFirstPassClearsStaleStaging(t *testing.T) {
	p, appsDir, backupsDir := newLocalProvider(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	staging := filepath.Join(backupsDir, "jellyfin", ".staging-"+testStamp)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	twoPass(t, p, "jellyfin", SnapshotOpts{})

	if _, err := os.Stat(filepath.Join(backupsDir, "jellyfin", testStamp, "leftover.txt")); !os.IsNotExist(err) {
		t.Error("a stale staging file survived into the committed archive")
	}
}

// An interrupted backup must leave nothing that List would offer for restore.
func TestLocalProviderAbortLeavesNothingListable(t *testing.T) {
	p, appsDir, _ := newLocalProvider(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))
	ctx := context.Background()

	if err := p.Snapshot(ctx, "jellyfin", testStamp, SnapshotOpts{Pass: 1}, nil); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got, _ := p.List(ctx, "jellyfin"); len(got) != 0 {
		t.Fatalf("an uncommitted backup is listable: %+v", got)
	}
	if err := p.Abort(ctx, "jellyfin", testStamp); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got, _ := p.List(ctx, "jellyfin"); len(got) != 0 {
		t.Fatalf("List after Abort = %+v, want empty", got)
	}
}

// What Commit reports and what List reports must agree, or the UI shows one thing
// on completion and another on refresh.
func TestLocalProviderCommitMatchesList(t *testing.T) {
	p, appsDir, _ := newLocalProvider(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	b := twoPass(t, p, "jellyfin", SnapshotOpts{})
	got, err := p.List(context.Background(), "jellyfin")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0] != b {
		t.Fatalf("List = %+v, want exactly the committed backup %+v", got, b)
	}
}

func TestLocalProviderRejectsBadAppName(t *testing.T) {
	p, _, _ := newLocalProvider(t)
	for _, bad := range []string{"../etc", "a/b", ".backups"} {
		if err := p.Snapshot(context.Background(), bad, testStamp, SnapshotOpts{Pass: 1}, nil); err == nil {
			t.Errorf("Snapshot(%q) was accepted; the traversal guard must reject it", bad)
		}
	}
}
