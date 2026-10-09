package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
)

// --- the app set -------------------------------------------------------------

// Snapshot captures the app folder under (app, stamp).
//
// The exclusions are installed on the LIVE pass only — and on a consuming uninstall,
// which has no live pass. Both passes share one source and one set of rules, and pass 2
// runs inside the app's downtime window: installing them again there would add a second
// round trip to an outage, for rules that are already exactly right.
func (p *Provider) Snapshot(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) error {
	applyExcludes := opts.Pass <= 1 || opts.Consume
	return p.snapshotSource(ctx, appSource(app), stamp, opts.Pass, opts.Consume, opts.Exclude.Rules(), applyExcludes, emit)
}

// snapshotSource is the shared body. applyExcludes is a decision, not a consequence of
// rules being present: an EMPTY rule set still has to be installable, because installing
// it is how a rule an app author has removed stops applying.
func (p *Provider) snapshotSource(ctx context.Context, sourceID, stamp string, pass int, consume bool, rules []string, applyExcludes bool, emit func(apps.Event)) error {
	args := []string{
		"--source-id=" + sourceID,
		"--path=" + p.sourcePath(sourceID),
		"--stamp=" + stamp,
		"--pass=" + strconv.Itoa(pass),
	}
	if applyExcludes {
		excl, cleanup, err := p.writeExcludes(rules)
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--exclude-file="+excl)
	}
	if consume {
		args = append(args, "--consume")
	}
	return p.call(ctx, unbounded, emit, nil, "snapshot", args...)
}

func (p *Provider) Commit(ctx context.Context, app, stamp string, _ apps.SnapshotOpts, emit func(apps.Event)) (apps.Backup, error) {
	return p.commitSource(ctx, appSource(app), app, stamp, emit)
}

func (p *Provider) commitSource(ctx context.Context, sourceID, app, stamp string, emit func(apps.Event)) (apps.Backup, error) {
	var wb wireBackup
	if err := p.call(ctx, metaTimeout, emit, &wb,
		"commit", "--source-id="+sourceID, "--stamp="+stamp); err != nil {
		return apps.Backup{}, err
	}
	b, ok := wb.backup(app, p.ID())
	if !ok {
		return apps.Backup{}, fmt.Errorf("%s: committed backup has an unusable stamp %q", p.ID(), wb.Stamp)
	}
	return b, nil
}

func (p *Provider) Abort(ctx context.Context, app, stamp string) error {
	return p.call(ctx, deleteTimeout, nil, nil,
		"abort", "--source-id="+appSource(app), "--stamp="+stamp)
}

func (p *Provider) Delete(ctx context.Context, app, stamp string) error {
	return p.call(ctx, deleteTimeout, nil, nil,
		"delete", "--source-id="+appSource(app), "--stamp="+stamp)
}

func (p *Provider) List(ctx context.Context, app string) ([]apps.Backup, error) {
	return p.listSource(ctx, appSource(app), app)
}

func (p *Provider) listSource(ctx context.Context, sourceID, app string) ([]apps.Backup, error) {
	var raw []wireBackup
	if err := p.call(ctx, metaTimeout, nil, &raw, "list", "--source-id="+sourceID); err != nil {
		return nil, err
	}
	out := make([]apps.Backup, 0, len(raw))
	for _, wb := range raw {
		if b, ok := wb.backup(app, p.ID()); ok {
			out = append(out, b)
		}
	}
	sortBackups(out)
	return out, nil
}

// ListAll returns every app's backups in one call.
//
// The user-data set is dropped here: it is a source, not an app, and it has no tile, no
// folder and no per-app page for the global list to link it to. It is reached through
// ListUserData instead.
func (p *Provider) ListAll(ctx context.Context) (map[string][]apps.Backup, error) {
	var raw map[string][]wireBackup
	if err := p.call(ctx, metaTimeout, nil, &raw, "list-all"); err != nil {
		return nil, err
	}
	out := map[string][]apps.Backup{}
	for sourceID, list := range raw {
		app, ok := appOf(sourceID)
		if !ok {
			continue
		}
		for _, wb := range list {
			if b, ok := wb.backup(app, p.ID()); ok {
				out[app] = append(out[app], b)
			}
		}
	}
	for app := range out {
		sortBackups(out[app])
	}
	return out, nil
}

func sortBackups(b []apps.Backup) {
	sort.Slice(b, func(i, j int) bool { return b[i].Stamp > b[j].Stamp })
}

// Materialize brings a backup down into the local archive tree so the ordinary restore
// path can swap it in.
//
// Maison builds the destination, from the validated stamp through AppBackupDir exactly
// as a local archive's path is built — the name never reaches a path unvalidated, and
// the adapter is never asked to know where Maison keeps its archives.
func (p *Provider) Materialize(ctx context.Context, app, stamp string, emit func(apps.Event)) error {
	if _, ok := apps.ParseBackupName(app, stamp); !ok {
		return fmt.Errorf("%s: not a backup stamp: %s", p.ID(), stamp)
	}
	dst := filepath.Join(apps.AppBackupDir(p.cfg.BackupsDir(), app), stamp)
	if err := p.mkdirForEngine(filepath.Dir(dst)); err != nil {
		return err
	}
	return p.call(ctx, unbounded, emit, nil,
		"materialize", "--source-id="+appSource(app), "--stamp="+stamp, "--dest="+dst)
}

func (p *Provider) RestoreInPlace(ctx context.Context, app, stamp, dst string, emit func(apps.Event)) error {
	return p.call(ctx, unbounded, emit, nil,
		"restore-in-place", "--source-id="+appSource(app), "--stamp="+stamp, "--dest="+dst)
}

// mkdirForEngine creates a directory the engine writes into, owned by the data user.
//
// The engine runs as root, so the chown is not what makes the write succeed — it is what
// keeps the archive tree looking like the rest of the data disk instead of a root-owned
// island the user cannot manage from an app. It also repairs a directory an older Maison
// left behind.
func (p *Provider) mkdirForEngine(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	uid, uerr := strconv.Atoi(p.cfg.PUID)
	gid, gerr := strconv.Atoi(p.cfg.PGID)
	if uerr != nil || gerr != nil {
		// No usable ids to hand it to. Left as it is rather than guessed at: guessing an
		// owner for a directory holding backups is worse than not trying.
		return nil
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("hand %s to the engine user: %w", dir, err)
	}
	return nil
}

// --- the user-data set -------------------------------------------------------

// BackupUserData snapshots everything under the data root that is not an app.
//
// One pass, because there is nothing to stop and so nothing a second pass would buy.
// The snapshot has no consistency guarantee — a file written while the engine reads it
// is captured mid-write — which is normal for documents and media and is exactly why
// apps get the stop treatment and this does not. The corollary is that a database must
// never live here.
func (p *Provider) BackupUserData(ctx context.Context, stamp string, emit func(apps.Event)) (string, error) {
	// Reapplied every run rather than only at setup: an engine's policies live in its
	// repository, so they outlive a Maison reinstall — and a Maison bug can leave a
	// stale one behind.
	if err := p.ensureRetention(ctx, userDataID, defaultUserDataKeep); err != nil {
		return "", err
	}
	// The exclusions travel as the ordinary per-snapshot exclusion list, so the set is
	// not a special case inside the engine — what it leaves out is Maison's answer, in
	// Maison's words, on the same path every app's exclusions take.
	//
	// **Installed on this pass even though it is pass 2**, unlike an app's. The
	// "live pass only" rule exists to keep a second round trip out of an app's downtime
	// window, and this set has no downtime window: nothing is stopped for it. Skipping
	// it here would leave AppData/ in the user-data snapshot — and an in-place restore
	// walks the snapshot's top-level entries with --delete-extra, so AppData would
	// become a target and every installed app's data would be written over.
	if err := p.snapshotSource(ctx, userDataID, stamp, 2, false, apps.UserDataExclusions, true, emit); err != nil {
		return "", err
	}
	b, err := p.commitSource(ctx, userDataID, apps.UserDataApp, stamp, emit)
	if err != nil {
		return "", err
	}
	return b.Name, nil
}

func (p *Provider) ListUserData(ctx context.Context) ([]apps.Backup, error) {
	return p.listSource(ctx, userDataID, apps.UserDataApp)
}

// RestoreUserData puts the user-data set back.
//
// Into a fresh directory it is one call: there is nothing there to delete.
//
// **In place it deliberately does not restore the data root as one target.** Each
// top-level entry is restored into its own path, because --delete-extra — what makes a
// restore a restore rather than a merge — aimed at the data root would delete everything
// the snapshot does not contain, and AppData/ is excluded from this snapshot by policy.
// Per-entry, AppData is never a target and cannot be reached.
//
// Which entries those are is decided HERE and passed down, not left to the engine:
// AppDataShared carries the engines' own configuration and must be skipped on a live box
// (the files being replaced are the ones the engine performing the restore is reading),
// and that is knowledge about Maison's layout, not about any engine.
func (p *Provider) RestoreUserData(ctx context.Context, stamp string, opts apps.UserDataRestoreOpts, emit func(apps.Event)) error {
	if opts.Dest != "" {
		if !filepath.IsAbs(opts.Dest) {
			return fmt.Errorf("%s: restore destination must be an absolute path: %s", p.ID(), opts.Dest)
		}
		if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
			return err
		}
		return p.call(ctx, unbounded, emit, nil,
			"materialize", "--source-id="+userDataID, "--stamp="+stamp, "--dest="+opts.Dest)
	}

	var have []wireEntry
	if err := p.call(ctx, metaTimeout, nil, &have,
		"entries", "--source-id="+userDataID, "--stamp="+stamp); err != nil {
		return err
	}
	wanted, err := selectEntries(have, opts.Entries)
	if err != nil {
		return err
	}

	var names []string
	for _, e := range wanted {
		if apps.UserDataInPlaceSkip[e.Name] {
			if emit != nil {
				emit(apps.Event{Pct: apps.PctUnknown, Message: "Leaving " + e.Name + " as it is"})
			}
			continue
		}
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		return fmt.Errorf("%s: this backup holds nothing to restore in place", p.ID())
	}
	return p.call(ctx, unbounded, emit, nil,
		"restore-in-place", "--source-id="+userDataID, "--stamp="+stamp,
		"--dest="+p.cfg.DataRoot, "--entries="+strings.Join(names, ","))
}

// selectEntries narrows a snapshot's members to what was asked for, refusing a name the
// snapshot does not hold — so "restore just Documents" cannot become "restore whatever
// path you name". The engine applies the same rule; this one exists so the refusal
// happens before a container is started.
func selectEntries(have []wireEntry, want []string) ([]wireEntry, error) {
	if len(want) == 0 {
		return have, nil
	}
	byName := map[string]wireEntry{}
	for _, e := range have {
		byName[e.Name] = e
	}
	out := make([]wireEntry, 0, len(want))
	for _, w := range want {
		e, ok := byName[w]
		if !ok {
			return nil, fmt.Errorf("this backup has no %q to restore", w)
		}
		out = append(out, e)
	}
	return out, nil
}

// --- retention ---------------------------------------------------------------

// EnsureRetention applies the operator's tiers to one app's source.
func (p *Provider) EnsureRetention(ctx context.Context, app string, keep backupconfig.Keep) error {
	return p.ensureRetention(ctx, appSource(app), keep)
}

func (p *Provider) ensureRetention(ctx context.Context, sourceID string, keep backupconfig.Keep) error {
	k, err := keepJSON(struct{ Latest, Daily, Weekly, Monthly, Annual int }{
		Latest: keep.Latest, Daily: keep.Daily, Weekly: keep.Weekly,
		Monthly: keep.Monthly, Annual: keep.Annual,
	})
	if err != nil {
		return err
	}
	args := []string{"--path=" + p.sourcePath(sourceID), "--keep=" + k}
	if sourceID == userDataID {
		args = append(args, "--user-data")
	}
	return p.call(ctx, metaTimeout, nil, nil, "ensure-retention", args...)
}

// --- recovery ----------------------------------------------------------------

// candidateFile is where a key the user typed waits for the adapter to test it. Named
// by the protocol: the adapter reads it, promotes it to repository.password if the
// repository opens with it, and deletes it either way.
const candidateFile = "repository.password.candidate"

// Recover reattaches a rebuilt box to the repository its space already holds, with the
// key the user was mailed.
//
// **This is the one place Maison writes into the engine directory**, and it writes only
// the candidate — never repository.password itself. Whether the key is right is a
// question only the repository can answer, so the promotion is the adapter's, made after
// a connect succeeded; a Maison that wrote the password file directly would turn a typo
// into a box that believes it has a key and fails every night.
//
// The candidate is removed on every path, by a defer, even though the adapter removes it
// too. It is the user's unrecoverable secret in a file, and "the other side cleans up"
// is not a property worth trusting with that: an adapter that crashed, timed out or
// predates the verb would leave it behind.
//
// The key is never logged and never part of an argv — it reaches the adapter through
// the file alone, which is the same rule call() states for every other secret.
func (p *Provider) Recover(ctx context.Context, key string) (apps.RecoverResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return apps.RecoverResult{}, fmt.Errorf("%w: no key was given", ErrWrongKey)
	}
	p.secretMu.Lock()
	defer p.secretMu.Unlock()

	// Whatever happens next, the cached status is wrong afterwards — connected on
	// success, and on failure still worth asking again rather than serving a probe
	// from before the attempt.
	defer p.Invalidate()

	path := filepath.Join(p.dir(), candidateFile)
	defer func() { _ = os.Remove(path) }()
	if err := writeSecret(path, key); err != nil {
		return apps.RecoverResult{}, fmt.Errorf("%s: could not hand the key to the engine: %w", p.ID(), err)
	}

	var rr recoverResult
	if err := p.call(ctx, recoverTimeout, nil, &rr, "recover"); err != nil {
		return apps.RecoverResult{}, err
	}
	return apps.RecoverResult{Snapshots: rr.Snapshots, Pinned: rr.Pinned}, nil
}

// nextFile is where a replacement key waits while the adapter installs it. Named by the
// protocol, and — unlike the candidate — the adapter's alone to remove: between the
// repository accepting the new key and the file being swapped, it is the only copy of a
// key that works. See ChangeSecret.
const nextFile = "repository.password.next"

// ChangeSecret replaces the key that opens the repository with the one given.
//
// Maison writes the key to repository.password.next and the adapter does the rest:
// asks the repository to accept it, then promotes the file. Everything already in the
// repository stays readable with the new key; the old one stops opening anything.
//
// **.next is never removed here**, which is the opposite of Recover and deliberate. A
// change cut short — the deadline, a killed container — may have reached the repository
// and not the file, and then .next is the one key that still works. The adapter removes
// it when the outcome is known and settles it on its next invocation when it is not. The
// one exception is an engine that answered "not supported": it never looked at the
// file, and nothing else ever will.
func (p *Provider) ChangeSecret(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("%w: no key was given", ErrWrongKey)
	}
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	defer p.Invalidate()

	path := filepath.Join(p.dir(), nextFile)
	if err := writeSecret(path, key); err != nil {
		return fmt.Errorf("%s: could not hand the key to the engine: %w", p.ID(), err)
	}
	err := p.call(ctx, changeSecretTimeout, nil, nil, "change-secret")
	if errors.Is(err, apps.ErrNotSupported) {
		_ = os.Remove(path)
	}
	return err
}

// Secret is the engine's declared secret, from its descriptor. ok is false for a
// descriptor that declares none; see Descriptor.Secret.
func (p *Provider) Secret() (apps.SecretSpec, bool) {
	if p.desc.Secret == nil {
		return apps.SecretSpec{}, false
	}
	s := p.desc.Secret
	return apps.SecretSpec{Label: s.Label, File: s.File, Escrow: s.Escrow}, true
}

// writeSecret writes a file only its owner can read, through a temporary in the same
// directory, so the adapter never reads half a key. The temporary is created 0600 from
// the start rather than chmodded after: there is no instant at which the key is
// readable by anyone else.
func writeSecret(path, body string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".candidate-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(body)
	cerr := f.Close()
	if err := firstErr(werr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}
