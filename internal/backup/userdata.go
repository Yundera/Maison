package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
)

// The user-data set — everything at the data root except AppData/ — as a thing the
// user can look at and put back.
//
// Backing it up already existed: the scheduler has had a user-data target since the
// engine landed. What did not exist was any way to *see* those snapshots or restore
// one, which meant a box could carry months of nightly snapshots of a media library
// with nothing in the UI acknowledging them. This file is the read and restore half.
//
// It is a separate type rather than a member of Scheduler because the two do different
// jobs: the scheduler decides *when* to write, this decides whether a restore is
// allowed to start and what state the tree is left in if it does not finish. It is
// also deliberately not part of apps.Registry — the registry's unit is an app folder
// with containers to stop, and this set is neither.
//
// ## Why it is not modelled as an app
//
// It has no compose project, no containers, no tile, and its name (`_userdata`) is
// deliberately not a valid project name, so it cannot be pushed through the guards
// written for app paths. See kopia.Source.
//
// ## What a restore does and does not touch
//
// AppData/ is excluded from the snapshot, and the engine restores the set entry by
// entry rather than aiming at the data root, so an in-place restore cannot reach an
// app's data — see kopia.Provider.RestoreUserData for why that is load-bearing rather
// than incidental. That is also why this does **not** stop every app first: a restore
// replaces Documents, Downloads, Media and whatever else sits at the data root, none
// of which holds app state. An app with an open handle on a media file being replaced
// is in the same position as a user overwriting that file over Samba, which is a thing
// that happens on a NAS and is survivable. A database must never live in this set, and
// the app model says so already.
type UserData struct {
	cfg   config.Config
	set   *Set
	store *backupconfig.Store

	// Now exists so tests can pin the stamp of the undo snapshot.
	Now func() time.Time

	// Busy reports a backup run in progress. It is a hook rather than a *Scheduler
	// field because the scheduler is built after this and the dependency is one-way:
	// this needs to know *whether* a run is happening, not to drive one.
	//
	// Nil means "never busy", which is only right in a test.
	Busy func() bool

	// OnChange, if set, is called when the restore state changes, so the dashboard can
	// rebroadcast it.
	OnChange func()

	mu    sync.Mutex
	state RestoreState
}

// RestoreState is what the panel shows while a restore runs, and after it fails.
type RestoreState struct {
	Running bool   `json:"running"`
	Stamp   string `json:"stamp,omitempty"`
	Message string `json:"message,omitempty"`
	// The same five numbers every other long operation on the box reports, derived by
	// apps.Tracker from whatever the engine managed to say. A restore of the user-data
	// set is the longest thing Maison ever does and the one with no tile to fall back
	// on, so "Restoring your files" with no bar was the worst case of the lot.
	Pct   float64 `json:"pct"`
	Done  int64   `json:"done,omitempty"`
	Total int64   `json:"total,omitempty"`
	Rate  float64 `json:"rate,omitempty"`
	ETA   int     `json:"eta,omitempty"`
	// InPlace distinguishes the destructive mode, because the two failures mean very
	// different things: a failed copy into a new folder has left nothing behind, and a
	// failed in-place restore has not.
	InPlace bool `json:"in_place"`
	// Error is sticky: it survives until the next attempt, so a failure that happened
	// while nobody was looking is still on the page afterwards.
	Error string `json:"error,omitempty"`
	// Interrupted reports the marker described in Restore: an in-place restore that
	// started and did not finish, leaving the tree as neither the old state nor the new
	// one. It outlives a Maison restart, which is the point of it being a file.
	Interrupted bool `json:"interrupted"`
	// InterruptedStamp is the backup that interrupted restore was applying, so the page
	// can offer to finish the job rather than only reporting the mess.
	InterruptedStamp string `json:"interrupted_stamp,omitempty"`
}

// UserDataRestoreEngine is implemented by engines that can also *read back* the
// user-data set. It is deliberately wider than UserDataEngine, which the scheduler uses
// and which only has to write: an engine could in principle gain backup support before
// restore support, and the panel has to be able to tell the difference rather than
// offering a restore that returns ErrNotSupported.
type UserDataRestoreEngine interface {
	UserDataEngine
	ListUserData(ctx context.Context) ([]apps.Backup, error)
	RestoreUserData(ctx context.Context, stamp string, opts apps.UserDataRestoreOpts, emit func(apps.Event)) error
}

// UserDataExclusions is what the set leaves out, surfaced for the page that offers a
// restore. It is the engine's list rather than a copy: a second list is a list that
// disagrees with the policy actually applied.
var UserDataExclusions = kopia.UserDataExclusions

// NewUserData builds the coordinator.
func NewUserData(cfg config.Config, set *Set, store *backupconfig.Store) *UserData {
	u := &UserData{cfg: cfg, set: set, store: store}
	// A marker left by a previous process is the whole reason it is a file, so it has to
	// be read at startup rather than only after a restore this process ran.
	u.state.Interrupted, u.state.InterruptedStamp = u.readMarker()
	return u
}

func (u *UserData) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// State returns the current or last restore.
func (u *UserData) State() RestoreState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

// Engine returns the engine that can serve the user-data set, or nil.
//
// It is the **writer**, not any engine that happens to hold snapshots, and that is a
// real limitation rather than an oversight: the set is one source, and an engine
// switch does not migrate it. A box that wrote user-data snapshots to kopia and then
// selected the local engine cannot see them, because the local engine has no user-data
// support at all — there is nothing to dispatch to. Apps do not have this problem
// because every engine implements the app interface.
func (u *UserData) Engine() UserDataRestoreEngine {
	return u.engineFor("")
}

// engineFor resolves one engine by ID, or the writer when the ID is empty.
//
// Writes are always the writer's: the schedule backs the set up to whichever engine is
// the default, and Engine() above is that path. **Reads are not.** A box that wrote
// user-data snapshots to kopia and then switched its default to the local engine still
// has those snapshots, and they are listable and restorable by naming kopia — which is
// the same rule apps already follow, and the reason the Backups page is a tab per
// engine rather than a view of the selected one.
//
// Nil means this engine cannot serve the set at all. The local engine is the case that
// matters: its archives live on the disk the set would be copied from, so it
// deliberately implements nothing here.
func (u *UserData) engineFor(id string) UserDataRestoreEngine {
	if u.set == nil {
		return nil
	}
	var p apps.Provider
	if id == "" {
		p = u.set.Writer()
	} else {
		p, _ = u.set.Get(id)
	}
	if p == nil {
		return nil
	}
	e, ok := p.(UserDataRestoreEngine)
	if !ok {
		return nil
	}
	return e
}

// Available reports whether this box can back up and restore the user-data set at all,
// with a reason when it cannot.
//
// The reason is the deliverable. "No backups" and "this engine will never back this up"
// look identical on a page that only lists rows, and the second is the one that needs
// saying: a default install has the local engine selected and the user-data checkbox
// on, and would otherwise show an empty list that reads as "nothing to worry about".
func (u *UserData) Available() (bool, string) {
	return u.AvailableIn("")
}

// AvailableIn is Available for one named engine, which is what a per-engine view asks.
//
// The two reasons it can answer no are different in kind, and only one of them is about
// this engine. "This engine cannot hold your files" is permanent and true of the local
// engine forever. "It is switched off" is a setting, and it is about the *schedule* —
// so it is only reported for the engine the schedule actually writes to. An engine
// holding snapshots from before the switch is available for reading regardless of what
// the checkbox says now.
func (u *UserData) AvailableIn(id string) (bool, string) {
	if u.engineFor(id) == nil {
		return false, "This backup engine keeps backups on this server's own disk, " +
			"so it cannot back up your files: the copy would sit on the disk it is meant to " +
			"survive. Choose a remote engine to include them."
	}
	if u.isWriter(id) && u.store != nil && !u.store.Get().UserData {
		return false, "Backing up your files is switched off. Turn on \"Include your files\" above."
	}
	return true, ""
}

// isWriter reports whether this ID names the engine new backups go to. An empty ID is
// the writer by definition.
func (u *UserData) isWriter(id string) bool {
	if id == "" {
		return true
	}
	if u.set == nil {
		return false
	}
	w := u.set.Writer()
	return w != nil && w.ID() == id
}

// List returns the user-data snapshots, newest first, or nothing when the engine cannot
// serve them. An engine with no repository yet is not an error — it is the normal state
// of a box whose provisioning has not run.
func (u *UserData) List(ctx context.Context) []apps.Backup {
	return u.ListIn(ctx, "")
}

// ListIn lists one engine's user-data snapshots. See engineFor: reading is per engine,
// writing is the writer's.
func (u *UserData) ListIn(ctx context.Context, id string) []apps.Backup {
	e := u.engineFor(id)
	if e == nil {
		return nil
	}
	got, err := e.ListUserData(ctx)
	if err != nil {
		if !errors.Is(err, apps.ErrNotConfigured) {
			log.Printf("backup: list user data from engine %q: %v", id, err)
		}
		return nil
	}
	return got
}

// Restore puts the user-data set back, detached, and returns once it has started.
//
// ## The in-place mode and its guards
//
// Restoring in place overwrites files the user has now with files they had then. It is
// not atomic and there is no rename to undo it with — the set is one tree the size of
// the disk, so there is no room to move it aside. Three things therefore happen before
// any byte is written, and each of them refuses the restore rather than proceeding on
// best effort:
//
//  1. **An undo snapshot.** The current state is backed up first, so the restore is
//     reversible. It is incremental, so it costs about the delta rather than the tree.
//     If it fails the restore does not run: an unrecoverable overwrite is worse than a
//     restore that did not happen. This is the same rule the app in-place path follows.
//  2. **A marker**, written under Maison's state directory — which is inside AppData/
//     and therefore outside the set being restored, so the restore cannot delete its
//     own marker. It survives a restart, and while it is there the panel says the tree
//     is in an unknown state and offers to run the restore again.
//  3. **One at a time.** A second restore, or a restore during a backup, is refused
//     rather than queued: two writers over one tree produce a state that came from
//     neither backup.
//
// Restoring into a new directory skips all of it. Nothing existing is touched, so
// there is nothing to undo and nothing to warn about.
func (u *UserData) Restore(ctx context.Context, engine, stamp string, opts apps.UserDataRestoreOpts, emit func(apps.Event)) error {
	// The engine the snapshot came from, not the writer: the user picked a row in one
	// engine's tab. The undo snapshot below then lands in that same engine, which keeps
	// a set's history in one repository rather than splitting it across two on a
	// restore.
	e := u.engineFor(engine)
	if e == nil {
		ok, why := u.AvailableIn(engine)
		if !ok {
			return errors.New(why)
		}
		return apps.ErrNotSupported
	}
	if _, ok := apps.ParseBackupName(userDataName, stamp); !ok {
		return fmt.Errorf("not a backup name: %s", stamp)
	}
	inPlace := opts.Dest == ""
	if !inPlace && !filepath.IsAbs(opts.Dest) {
		return fmt.Errorf("restore destination must be an absolute path: %s", opts.Dest)
	}

	// A backup writing the same tree a restore is rewriting produces a snapshot of a
	// state that never existed — and, worse, one that counts against retention and can
	// push a good snapshot out. Refused rather than queued, like a second restore.
	if u.Busy != nil && u.Busy() {
		return errors.New("a backup is running; wait for it to finish before restoring your files")
	}

	// Copy mode writes a full second copy of the set onto the same disk. In place needs
	// essentially nothing, which is why the guard is only on this branch — and why it is
	// here rather than in the engine: the engine streams, the disk is Maison's problem.
	if !inPlace {
		if err := u.roomFor(ctx, engine, stamp, opts.Dest); err != nil {
			return err
		}
	}

	u.mu.Lock()
	if u.state.Running {
		u.mu.Unlock()
		return errors.New("a restore of your files is already running")
	}
	u.state = RestoreState{
		Running: true, Stamp: stamp, InPlace: inPlace, Message: "Preparing",
		Pct:         apps.PctUnknown,
		Interrupted: u.state.Interrupted, InterruptedStamp: u.state.InterruptedStamp,
	}
	u.mu.Unlock()
	u.changed()

	go func() {
		err := u.restore(ctx, e, stamp, opts, inPlace, emit)
		u.finish(err)
	}()
	return nil
}

// roomFor refuses a copy-mode restore that would not fit.
//
// The set is the largest thing on the box by design — it is where the media library
// lives — so copying one back beside itself is the one restore that can fill the data
// disk. Filling it does not merely fail the restore: every app writing to that disk
// starts failing too, which is a far worse outcome than a refusal.
//
// Headroom matches the app path's folder restore (10%), and an unreadable disk reading
// lets the restore proceed rather than blocking it on a guess — also as the app path
// does. A size of zero means the engine did not report one, which is not evidence that
// it fits, so it is allowed through rather than refused on a number that is not there.
func (u *UserData) roomFor(ctx context.Context, engine, stamp, dest string) error {
	var size int64
	for _, b := range u.ListIn(ctx, engine) {
		if b.Name == stamp {
			size = b.Size
			break
		}
	}
	if size == 0 {
		return nil
	}
	needed := int64(float64(size) * copyHeadroom)

	// The destination may not exist yet, so the reading is taken from the data root the
	// restore will land under rather than from dest itself.
	usage, err := disk.Usage(u.cfg.DataRoot)
	if err != nil {
		log.Printf("backup: disk usage for %s: %v (skipping the free-space guard)", u.cfg.DataRoot, err)
		return nil
	}
	if int64(usage.Free) < needed {
		return fmt.Errorf("not enough free space to copy your files back: %s needed, %s free. "+
			"Restore over your existing files instead, which needs no extra space",
			humanBytes(needed), humanBytes(int64(usage.Free)))
	}
	return nil
}

// copyHeadroom matches the app path's folder-restore headroom: the measurement is a
// moment old and the tree is live.
const copyHeadroom = 1.1

// humanBytes is a compact size for an error a user reads.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// restore is the body of a started restore, so the guards read in order.
func (u *UserData) restore(ctx context.Context, e UserDataRestoreEngine, stamp string, opts apps.UserDataRestoreOpts, inPlace bool, emit func(apps.Event)) error {
	track := func(msg string) {
		u.progress(apps.Event{Pct: apps.PctUnknown, Message: msg}, apps.Progress{Pct: apps.PctUnknown})
		if emit != nil {
			emit(apps.Event{Pct: apps.PctUnknown, Message: msg})
		}
	}
	// One tracker per phase-worth of work. The undo snapshot and the restore proper are
	// separate operations against the same tree at different speeds, so they are given
	// separate phase names rather than one estimate spanning both.
	observe := func(phase string) func(apps.Event) {
		tr := &apps.Tracker{}
		return func(ev apps.Event) {
			u.progress(ev, tr.Observe(phase, ev.Pct, ev.Done, ev.Total))
			if emit != nil {
				emit(ev)
			}
		}
	}

	if inPlace {
		// Guard 1: the undo snapshot. Refusing here is the whole point — see Restore.
		track("Backing up the current files first, so this can be undone")
		if _, err := e.BackupUserData(ctx, u.now().Format(apps.StampLayout), observe(apps.PhaseCopy)); err != nil {
			return fmt.Errorf("could not back up the current files first, so nothing was changed: %w", err)
		}
		// Guard 2: the marker. Written before the first byte and cleared only on success.
		if err := u.writeMarker(stamp); err != nil {
			return fmt.Errorf("could not record that a restore started, so nothing was changed: %w", err)
		}
	}

	track("Restoring your files")
	if err := e.RestoreUserData(ctx, stamp, opts, observe(apps.PhaseRestore)); err != nil {
		return err
	}

	if inPlace {
		if err := u.clearMarker(); err != nil {
			// The restore itself worked; failing to clear the marker only means the page keeps
			// warning. Say so rather than reporting the restore as failed.
			return fmt.Errorf("your files were restored, but the restore marker could not be cleared: %w", err)
		}
	}
	return nil
}

// progress records one report. A message-less event keeps whatever the last message
// was: engines emit far more progress lines than distinct messages, and blanking the
// caption between them makes it flicker.
func (u *UserData) progress(ev apps.Event, p apps.Progress) {
	u.mu.Lock()
	if ev.Message != "" {
		u.state.Message = ev.Message
	}
	u.state.Pct = p.Pct
	u.state.Done, u.state.Total = ev.Done, ev.Total
	u.state.Rate, u.state.ETA = p.Rate, int(p.ETA.Seconds())
	u.mu.Unlock()
	u.changed()
}

func (u *UserData) finish(err error) {
	u.mu.Lock()
	u.state.Running = false
	u.state.Message = ""
	u.state.Pct, u.state.Done, u.state.Total = apps.PctUnknown, 0, 0
	u.state.Rate, u.state.ETA = 0, 0
	if err != nil {
		u.state.Error = err.Error()
	} else {
		u.state.Error = ""
	}
	u.state.Interrupted, u.state.InterruptedStamp = u.readMarker()
	u.mu.Unlock()
	u.changed()
}

func (u *UserData) changed() {
	if u.OnChange != nil {
		u.OnChange()
	}
}

// markerPath is under Maison's state directory, which lives in AppData/ and is
// therefore *outside* the set being restored. A marker inside it would be deleted by
// the restore that is meant to be guarded by it.
func (u *UserData) markerPath() string {
	return filepath.Join(u.cfg.StateDir(), "userdata-restoring")
}

func (u *UserData) writeMarker(stamp string) error {
	if err := os.MkdirAll(u.cfg.StateDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(u.markerPath(), []byte(stamp), 0o644)
}

func (u *UserData) clearMarker() error {
	if err := os.Remove(u.markerPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readMarker reports an in-place restore that started and did not finish. The stamp it
// was applying is the file's content, validated on the way out because it is read back
// from disk.
func (u *UserData) readMarker() (bool, string) {
	b, err := os.ReadFile(u.markerPath())
	if err != nil {
		return false, ""
	}
	stamp := string(b)
	if _, ok := apps.ParseBackupName(userDataName, stamp); !ok {
		// The marker is there, which is the fact that matters; an unreadable stamp only
		// costs the offer to finish the job.
		return true, ""
	}
	return true, stamp
}

// userDataName is a placeholder app name used only to reuse apps.ParseBackupName as a
// stamp validator. The set's real reserved name is not a valid project name — which is
// deliberate — and ParseBackupName validates the name it is given, so it needs one that
// passes.
const userDataName = "userdata"
