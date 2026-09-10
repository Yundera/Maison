package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/exclude"
)

// Backup phases, in order. A folder backup skips `compress`.
const (
	PhaseCopy     = "copy"     // mirroring the app folder while the app is still up
	PhaseSync     = "sync"     // app stopped: copying the delta the first pass missed
	PhaseStart    = "start"    // bringing the app back up
	PhaseCompress = "compress" // zipping the snapshot (zip backups only)
	PhaseRestore  = "restore"  // putting an archive back as the live app folder
)

// Headroom multipliers applied to the app folder's measured size when deciding
// whether a backup fits. A folder backup needs one copy; a zip needs the snapshot
// *and* the zip on disk at the same time, and a zip of incompressible data is no
// smaller than its input — so the worst case is two.
//
// Filling the data disk does not just fail the backup: every other app on the box
// starts failing writes. Refusing early is the whole point of measuring.
const (
	folderHeadroom = 1.1
	zipHeadroom    = 2.0
)

// BackupEvent is a progress update emitted while an app is backed up. Like an
// uninstall it has independent tracks that the UI shows one at a time on a single
// bar: Copy (the live pass), Sync (the stopped pass), then Compress.
type BackupEvent struct {
	Phase    string  `json:"phase"`    // copy | sync | start | compress | done | error
	Message  string  `json:"message"`  // human-readable detail
	Copy     float64 `json:"copy"`     // first (live) mirror pass, 0-100
	Sync     float64 `json:"sync"`     // second (stopped) mirror pass, 0-100
	Compress float64 `json:"compress"` // zipping the snapshot, 0-100

	// Done and Total are the byte counts behind whichever track is moving, passed
	// straight through from the engine's Event. Zero means the engine did not report
	// them, which is normal rather than exceptional.
	Done  int64 `json:"done,omitempty"`
	Total int64 `json:"total,omitempty"`

	// Rate (bytes/second) and ETA (seconds) are *derived*, not reported: they are
	// filled in by the Tracker in trackBackup, on the way past, and a provider must
	// never set them. Zero means not yet knowable — see apps.Progress.
	Rate float64 `json:"rate,omitempty"`
	ETA  int     `json:"eta,omitempty"`

	// Engine is which destination this event is about, and EngineIndex/Engines are its
	// place in the set ("2 of 3"). A backup can be written to several engines in one
	// operation, and without these the tile would show one bar restarting per engine
	// with no way to say why.
	//
	// Providers never set them, the same way they never set Rate and ETA: BackupTo
	// stamps them on the way past, because only it knows the set.
	Engine      string `json:"engine,omitempty"`
	EngineIndex int    `json:"engineIndex,omitempty"`
	Engines     int    `json:"engines,omitempty"`
}

// BackupState is a snapshot of one in-flight (or failed) backup or restore. The
// server overlays it onto the app's tile so the progress survives a reload and
// rides the live app list, exactly like UninstallState does.
type BackupState struct {
	ID       string  `json:"id"`       // compose project name (== app tile id)
	Phase    string  `json:"phase"`    // copy | sync | start | compress | restore | done | error
	Message  string  `json:"message"`  // human-readable detail
	Copy     float64 `json:"copy"`     // first (live) mirror pass, 0-100
	Sync     float64 `json:"sync"`     // second (stopped) mirror pass, 0-100
	Compress float64 `json:"compress"` // zipping the snapshot, 0-100
	Done     int64   `json:"done"`     // bytes processed in the current phase, 0 = unknown
	Total    int64   `json:"total"`    // bytes expected in the current phase, 0 = unknown
	Rate     float64 `json:"rate"`     // bytes/second, 0 = unknown
	ETA      int     `json:"eta"`      // seconds left in this phase, 0 = unknown
	Error    string  `json:"error"`    // set when Phase == error
}

// Estimate is the up-front answer to "can this app be backed up right now",
// shown in the confirmation dialog before anything is copied.
type Estimate struct {
	Size   int64 `json:"size"`   // measured size of the app folder
	Needed int64 `json:"needed"` // free space required, including headroom
	Free   int64 `json:"free"`   // free space on the data filesystem
	Enough bool  `json:"enough"` // Free >= Needed
	Zip    bool  `json:"zip"`    // which headroom Needed was computed with

	// Streamed is true when the engine sends the app straight to a repository
	// instead of staging a copy beside it, so the backup needs no free disk and the
	// guard below does not apply. The dialog uses this to explain why it is not
	// showing a space requirement, rather than showing "0 bytes needed".
	Streamed bool `json:"streamed"`

	// Excluded is what the app declared as derived and asked to be left out
	// (x-compose-app backup.exclude), in its canonical spelling, and ExcludedSize is
	// what that comes to on disk. Size above already has it subtracted.
	//
	// They are here because a backup that does not contain something has to say so:
	// an app that comes back from a restore without its cache is working as declared,
	// and an app that comes back missing something its author wrongly marked derived
	// is a bug report — and only a UI that names the paths tells the two apart. Read
	// from the parsed rules, never echoed from the compose file, so what is shown
	// cannot drift from what is applied.
	Excluded     []string `json:"excluded,omitempty"`
	ExcludedSize int64    `json:"excludedSize,omitempty"`

	// ExcludeErrors are the entries Maison refused, in the author's own spelling.
	// A refused pattern excludes nothing — the backup is a superset, never short —
	// but it means the app is not getting what it asked for, and the only way that
	// ever gets fixed is by being visible here.
	ExcludeErrors []string `json:"excludeErrors,omitempty"`
}

// exclusionsFor resolves what app `id` declared as derived data — the directories
// x-compose-app `backup.exclude` asks Maison to leave out of its backups — together
// with the entries it had to refuse.
//
// Read from the app's compose on every call rather than cached. The declaration
// moves when a store update lands or an operator edits the override, and a backup
// running against a stale copy either stores what the author has retired or drops
// what they have just added back.
//
// A refused entry is dropped, never fatal: what it costs is a backup that carries
// MORE than was asked, which is not data loss, while refusing the whole declaration
// over one typo would be. The refusals are returned so the dialog can show them —
// a mistake in a store app should be visible, not silent.
func (r *Registry) exclusionsFor(id string) (*exclude.Set, []error) {
	_, ca := r.metaFor(id, "")
	if ca == nil || len(ca.Backup.Exclude) == 0 {
		return nil, nil
	}
	patterns := make([]string, 0, len(ca.Backup.Exclude))
	for _, p := range ca.Backup.Exclude {
		// Rendered the way a folder path is, then mapped back into this container's
		// data mount — so the absolute spelling an author copies out of `folders:`
		// (/DATA/AppData/${AppID}/cache, a HOST path) is compared against the app
		// folder Maison actually reads.
		patterns = append(patterns, envinject.ContainerPath(envinject.Render(p, r.cfg, id, nil), r.cfg))
	}
	return exclude.Parse(patterns, filepath.Join(r.cfg.AppsDir(), id))
}

// excludeSet is exclusionsFor for the paths that only need the answer, logging what
// it refused so a rejected pattern leaves a trace even where nothing renders it.
func (r *Registry) excludeSet(id string) *exclude.Set {
	set, errs := r.exclusionsFor(id)
	for _, err := range errs {
		log.Printf("backup %s: ignoring backup.exclude entry: %v", id, err)
	}
	return set
}

// EstimateBackup measures what a backup of `id` would cost. The size walk is
// stat-only, but it does visit every file — the dialog calls this once on open,
// not on a poll.
func (r *Registry) EstimateBackup(id, engine string, zip bool) (Estimate, error) {
	if !projectRe.MatchString(id) {
		return Estimate{}, fmt.Errorf("invalid app name: %s", id)
	}
	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		return Estimate{}, fmt.Errorf("%s has no folder to back up", id)
	}
	// What the app declared as derived is not copied, so it must not be reserved for
	// either: sizing the whole folder would refuse a backup that fits comfortably.
	skip, skipErrs := r.exclusionsFor(id)
	size, excludedSize := measureDir(appDir, skip)
	// Carried on every return below, including the refusals: what was left out is
	// exactly the thing a user needs to see when a backup — or a restore from it —
	// does not contain what they expected.
	est := Estimate{
		Size: size, Zip: zip,
		Excluded: skip.Patterns(), ExcludedSize: excludedSize,
		ExcludeErrors: errorStrings(skipErrs),
	}

	// An engine that streams to a repository needs no room beside the app, so the
	// guard is skipped entirely rather than computed and passed. This is what lets an
	// app occupying most of its own disk be backed up at all — the local engine
	// refuses it, because a full second copy genuinely does not fit.
	//
	// Over the WHOLE SET this backup will be written to, not one engine of it. A
	// backup that goes to a repository *and* the local disk still needs the room the
	// local copy takes, and asking only the first engine gets this exactly backwards:
	// on a repository-default box with local also ticked, the estimate would report
	// "streamed", skip the guard, and let the local mirror fill the data disk — which
	// does not merely fail the backup, it fails every app still writing to that disk.
	targets, err := r.enginesFor(engine, TriggerSchedule)
	if err != nil {
		return Estimate{}, err
	}
	needsRoom := false
	for _, t := range targets {
		if t.Caps().NeedsLocalSpace {
			needsRoom = true
			break
		}
	}
	if !needsRoom {
		est.Free, est.Enough, est.Streamed = freeSpace(r.cfg.DataRoot), true, true
		return est, nil
	}

	headroom := folderHeadroom
	if zip {
		headroom = zipHeadroom
	}
	est.Needed = int64(float64(size) * headroom)

	u, err := disk.Usage(r.cfg.DataRoot)
	if err != nil {
		// Without a usable reading we cannot refuse honestly, so we let the backup
		// proceed and fail on ENOSPC rather than blocking it on a guess.
		log.Printf("backup: disk usage for %s: %v (skipping free-space guard)", r.cfg.DataRoot, err)
		est.Free, est.Enough = -1, true
		return est, nil
	}
	est.Free = int64(u.Free)
	est.Enough = est.Free >= est.Needed
	return est, nil
}

// errorStrings renders refusals for the wire. Nil rather than an empty slice when
// there are none, so the field is simply absent from the JSON.
func errorStrings(errs []error) []string {
	if len(errs) == 0 {
		return nil
	}
	out := make([]string, 0, len(errs))
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
}

// StartBackup launches a detached backup of app `id` and tracks its progress so it
// rides the live app list — the confirmation dialog closes immediately and the
// tile carries the progress bar to the end.
//
// It returns only what is knowable up front: an unknown app, or not enough free
// space. A later failure lands in the tracked state (Phase == error) and stays on
// the tile until it is retried or dismissed.
//
// Idempotent: a second call while the same app is being backed up is a no-op.
func (r *Registry) StartBackup(id, engine string, zip bool) error {
	est, err := r.EstimateBackup(id, engine, zip)
	if err != nil {
		return err
	}
	if !est.Enough {
		return fmt.Errorf("not enough free space: %s needs %s, %s available",
			id, humanBytes(est.Needed), humanBytes(est.Free))
	}

	if err := r.beginBackup(id); err != nil {
		return nil // already running — attach, don't restart
	}
	r.changed()

	go func() {
		// Deliberately not a request context: the backup must outlive the request
		// that asked for it.
		res, err := r.Backup(context.Background(), id, engine, zip, r.trackBackup(id, nil))
		if err == nil {
			err = firstErr(res)
		}
		r.finishBackup(id, err, "backup")
	}()
	return nil
}

// StartRestore launches a detached restore of `name` over app `id`.
//
// The live folder is not thrown away: it is archived first, which — being a
// rename within the same directory tree — costs nothing and takes no time. So a
// restore is reversible, and a folder archive being consumed by the restore is
// balanced by the archive the restore just made of what was there.
// ctx bounds the up-front lookup only, not the restore: for a remote engine that
// lookup is a subprocess against the repository, so a caller that goes away should not
// leave it running. Nothing has been touched at that point, so refusing is safe. The
// restore itself is deliberately detached below.
func (r *Registry) StartRestore(ctx context.Context, id, engine, name string) error {
	// Resolve through the engines, not the data disk. Checking .backups/ here would
	// reject a backup that exists only in a repository — the restore path below
	// handles it perfectly well via locate() — so the check has to ask the same
	// question the restore will: does any engine have this backup.
	if _, _, err := r.locate(ctx, id, engine, name); err != nil {
		return err
	}

	r.mu.Lock()
	if st := r.backups[id]; st != nil && st.Phase != PhaseError {
		r.mu.Unlock()
		return nil
	}
	r.backups[id] = &BackupState{ID: id, Phase: PhaseRestore, Message: "Queued"}
	r.mu.Unlock()
	r.changed()

	go func() {
		err := r.Restore(context.Background(), id, engine, name, r.trackBackup(id, nil))
		r.finishBackup(id, err, "restore")
	}()
	return nil
}

// trackBackup returns the emit function that derives rate and ETA, copies the
// result into the tracked state, pokes the throttled progress hook, and passes the
// finished event on to `extra`.
//
// The Tracker is created here, once per operation, which is what makes it correct
// for every engine at once: whatever a provider managed to report — a percentage,
// byte counts, or neither — is turned into the same two derived numbers in the same
// place. Nothing below this line is engine-aware.
//
// `extra` is how the same event reaches a second observer without a second tracker
// producing a second, slightly different estimate of the same work. The whole-box
// run uses it to mirror an app's progress into its own target list while the tile
// shows exactly the same numbers.
func (r *Registry) trackBackup(id string, extra func(BackupEvent)) func(BackupEvent) {
	tr := &Tracker{}
	return func(ev BackupEvent) {
		// Keyed on the phase AND the engine, so an estimate never spans the boundary
		// between the live pass and the stopped one, nor between two destinations.
		// They are different work at different speeds — and the stopped pass's ETA is
		// how much longer the app is *down*, which is the one number here worth being
		// careful about. An anchor carried from one engine into the next would derive
		// a rate from two unrelated byte streams.
		p := tr.Observe(ev.Phase+"/"+ev.Engine, ev.TrackPct(), ev.Done, ev.Total)
		ev.Rate, ev.ETA = p.Rate, int(p.ETA.Seconds())

		r.mu.Lock()
		if st := r.backups[id]; st != nil {
			st.Phase, st.Message = ev.Phase, ev.Message
			st.Copy, st.Sync, st.Compress = ev.Copy, ev.Sync, ev.Compress
			st.Done, st.Total, st.Rate, st.ETA = ev.Done, ev.Total, ev.Rate, ev.ETA
		}
		r.mu.Unlock()
		r.progressed()
		if extra != nil {
			extra(ev)
		}
	}
}

// TrackPct is the percentage of the track that is actually moving.
//
// The tracks are cumulative on the wire — a Sync event carries Copy: 100 so the bar
// does not rewind — so reading the wrong one gives the progress of a phase that
// finished a minute ago and is pinned at 100%. Exported because the whole-box run
// mirrors these events into its own target list and needs the same answer.
func (ev BackupEvent) TrackPct() float64 {
	switch ev.Phase {
	case PhaseCompress:
		return ev.Compress
	case PhaseSync:
		return ev.Sync
	case PhaseCopy, PhaseRestore:
		return ev.Copy
	default:
		// Phases with no track of their own (start, done, error). They are moments
		// rather than stretches, and an ETA for one is meaningless.
		return PctUnknown
	}
}

// BackupTracked runs a backup to completion while showing its progress on the app's
// tile, and reports every event to `extra` as well.
//
// It is the synchronous twin of StartBackup, and exists for the whole-box run: that
// run needs to wait for each target before starting the next one (two apps stopped at
// once is not a backup strategy), but it should light up the tiles exactly as a
// single-app backup does. Before this existed the nightly run called Backup directly
// with a nil emit, so a run started from Settings left every tile on the box inert —
// the one moment the user is most likely to be watching them.
func (r *Registry) BackupTracked(ctx context.Context, id, engine string, zip bool, extra func(BackupEvent)) ([]Result, error) {
	if err := r.beginBackup(id); err != nil {
		return nil, err
	}
	r.changed()
	res, err := r.Backup(ctx, id, engine, zip, r.trackBackup(id, extra))
	// A partially failed backup is a failure on the tile even though something was
	// written: the destination the user is missing is the one worth saying out loud,
	// and finishBackup's message is the only place the tile can say it.
	if err == nil {
		err = firstErr(res)
	}
	r.finishBackup(id, err, "backup")
	return res, err
}

// ErrBackupInFlight reports that this app is already being backed up, so a second
// attempt has nothing to do. It is a sentinel rather than a plain error because the
// two callers want opposite things from it: StartBackup attaches to the operation
// already running, and the whole-box run marks the target skipped rather than failed
// — an app that is being backed up right now is the one case where "we did not back
// it up" is not a problem worth mailing anyone about.
var ErrBackupInFlight = errors.New("a backup of this app is already running")

// beginBackup claims the tracking slot for an app, or reports that something else
// already has it.
func (r *Registry) beginBackup(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.backups[id]; st != nil && st.Phase != PhaseError {
		return fmt.Errorf("%s: %w", id, ErrBackupInFlight)
	}
	r.backups[id] = &BackupState{ID: id, Phase: PhaseCopy, Message: "Queued"}
	return nil
}

// finishBackup settles a tracked operation: a failure stays visible on the tile
// until the operator retries or dismisses it, a success drops the overlay.
func (r *Registry) finishBackup(id string, err error, what string) {
	r.mu.Lock()
	if err != nil {
		log.Printf("%s %s failed: %v", what, id, err)
		if st := r.backups[id]; st != nil {
			st.Phase, st.Error, st.Message = PhaseError, err.Error(), err.Error()
		}
	} else {
		delete(r.backups, id)
	}
	r.mu.Unlock()
	r.changed()
}

// Backups returns a snapshot of every tracked backup/restore (in-flight or errored).
func (r *Registry) Backups() []BackupState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BackupState, 0, len(r.backups))
	for _, st := range r.backups {
		out = append(out, *st)
	}
	return out
}

// ClearBackup drops a tracked backup (used to dismiss a failed one).
func (r *Registry) ClearBackup(id string) {
	r.mu.Lock()
	_, existed := r.backups[id]
	delete(r.backups, id)
	r.mu.Unlock()
	if existed {
		r.changed()
	}
}

// Backup archives the app's folder without uninstalling it, and returns the new
// archive's base name.
//
// The app is stopped for the archive to be consistent — a database zipped while
// its process is writing restores as corruption — but it is stopped for as little
// time as possible, by copying twice:
//
//	copy      app up      full mirror into a staging folder     (no downtime)
//	stop      ────────────────────────────────────────────────  downtime starts
//	sync      app down    only what changed since the first pass
//	start     ────────────────────────────────────────────────  downtime ends
//	compress  app up      zip the snapshot (zip backups only)
//
// So downtime is proportional to what the app wrote *during* the first pass, not
// to how big it is. The snapshot itself is consistent either way: every byte in it
// was read either before the app touched it again, or while the app was down.
//
// The restart is deferred, so a failure anywhere after the stop still brings the
// app back up. Leaving an app down is a worse outcome than a missing backup.
func (r *Registry) Backup(ctx context.Context, id, engine string, zip bool, emit func(BackupEvent)) ([]Result, error) {
	ps, err := r.enginesFor(engine, TriggerSchedule)
	if err != nil {
		return nil, err
	}
	return r.BackupTo(ctx, ps, id, zip, emit)
}

// Result is what one engine made of one backup.
//
// A backup can land in several engines at once and they do not agree on everything:
// the name is engine-relative (the local engine's zip mode produces "<stamp>.zip"
// where every other case is "<stamp>"), and one engine failing does not stop the
// others. So the answer is a row per engine rather than a name and an error.
type Result struct {
	Engine string
	Name   string
	Err    error
}

// BackupWith runs a backup through one named engine and returns the single name it
// produced.
//
// It exists for the update path, which needs a rollback point it can restore by
// *rename*: an update is undone in the seconds after it broke something, and a
// download is not that. It keeps the narrow signature deliberately — that caller hands
// the name it gets straight to RollBack, and there is exactly one engine involved, so
// widening it would make every caller unpack a slice to find the one row it wanted.
func (r *Registry) BackupWith(ctx context.Context, p Provider, id string, zip bool, emit func(BackupEvent)) (string, error) {
	res, err := r.BackupTo(ctx, []Provider{p}, id, zip, emit)
	if err != nil {
		return "", err
	}
	if len(res) != 1 {
		return "", fmt.Errorf("backup %s: expected one result, got %d", id, len(res))
	}
	return res[0].Name, res[0].Err
}

// BackupTo backs one app up to every engine given, inside a SINGLE stop window.
//
// That is the whole reason this is one function rather than a loop over the old
// single-engine one. Stopping the app per engine would multiply the outage by the
// number of destinations, and — worse — the first engine's deferred restart would
// bring the app back up while the second was still taking its "consistent" stopped
// pass, quietly producing a torn snapshot that looks fine until someone restores it.
//
// So the passes are interleaved instead of the operations:
//
//	pass 1    app up      each engine mirrors, in turn      (no downtime)
//	stop      ──────────────────────────────────────────    downtime starts
//	pass 2    app down    each engine syncs and commits
//	start     ──────────────────────────────────────────    downtime ends
//
// Engines run one after another, not concurrently: they read the same tree, so the
// local mirror and a repository upload contend for exactly the same disk during the
// pass whose whole point is being cheap while the app is serving. The Tracker, the
// emit callback and BackupState are also single-operation state — see trackBackup.
//
// An engine that fails is dropped from the later phases and reported in its Result;
// the rest carry on. A failure is not contagious because the destinations are not:
// losing the offsite copy of an app is not a reason to also lose the local one.
func (r *Registry) BackupTo(ctx context.Context, ps []Provider, id string, zip bool, emit func(BackupEvent)) ([]Result, error) {
	if emit == nil {
		emit = func(BackupEvent) {}
	}
	if !projectRe.MatchString(id) {
		return nil, fmt.Errorf("invalid app name: %s", id)
	}
	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		return nil, fmt.Errorf("%s has no folder to back up", id)
	}
	// Validated before anything is touched, and certainly before the app is stopped: a
	// nil engine discovered mid-loop would panic with the app already down.
	if len(ps) == 0 {
		return nil, fmt.Errorf("no backup engine to write %s to", id)
	}
	for _, p := range ps {
		if p == nil {
			return nil, fmt.Errorf("backup %s: nil engine", id)
		}
	}

	r.enter(id)
	defer r.leave(id)

	// Resolved once, here, and carried through both passes: re-reading it between them
	// would let a store update landing mid-backup make pass 2 disagree with pass 1
	// about what the snapshot is supposed to contain.
	opts := SnapshotOpts{Zip: zip, Exclude: r.excludeSet(id)}
	// ONE stamp for every engine. The same backup in two places should have the same
	// name — that is what lets the page put them on one row per app and what makes
	// "the copy from Tuesday" mean one thing.
	stamp := time.Now().Format(StampLayout)

	res := make([]Result, len(ps))
	for i, p := range ps {
		res[i] = Result{Engine: p.ID()}
	}
	committed := make([]bool, len(ps))

	// Nothing the engine stages is durable until Commit, so an interrupted backup
	// discards it rather than leaving something a later List might offer for restore.
	//
	// ONE defer covering every engine, registered before the stop below so that it runs
	// *after* the restart: bringing the app back up always takes precedence over
	// cleaning up. Registering one per engine inside the loop would invert that for
	// every engine reached after the stop — defers are LIFO — and a repository's Abort
	// is a listing plus a delete per match, so a failed three-engine run would hold the
	// app down for tens of minutes of cleanup.
	//
	// WithoutCancel, never downCtx: a window that expired is exactly when cleanup has
	// to run, and handing it a cancelled context orphans the staging it came to remove.
	defer func() {
		for i, p := range ps {
			if committed[i] {
				continue
			}
			if err := p.Abort(context.WithoutCancel(ctx), id, stamp); err != nil {
				log.Printf("backup %s (%s): discard incomplete backup: %v", id, p.ID(), err)
			}
		}
	}()

	// Pass 1 — the app is still serving. This is the long one.
	opts.Pass = 1
	for i, p := range ps {
		if err := ctx.Err(); err != nil {
			res[i].Err = err
			continue
		}
		emit(r.engineEvent(ps, i, BackupEvent{Phase: PhaseCopy, Message: "Copying " + id}))
		if err := p.Snapshot(ctx, id, stamp, opts, func(ev Event) {
			emit(r.engineEvent(ps, i, BackupEvent{Phase: PhaseCopy, Message: ev.Message, Copy: ev.Pct,
				Done: ev.Done, Total: ev.Total}))
		}); err != nil {
			res[i].Err = fmt.Errorf("copy app folder: %w", err)
		}
	}
	emit(r.engineEvent(ps, len(ps)-1, BackupEvent{Phase: PhaseCopy, Message: "Copied", Copy: 100}))

	// Stop only if it is actually up, so a backup of an already-stopped app does
	// not start it afterwards.
	wasRunning := r.isRunning(ctx, id)
	if wasRunning {
		emit(BackupEvent{Phase: PhaseSync, Message: "Stopping " + id, Copy: 100})
		if err := r.dx.StopProject(ctx, id); err != nil {
			return nil, fmt.Errorf("stop app: %w", err)
		}
		defer func() {
			emit(BackupEvent{Phase: PhaseStart, Message: "Starting " + id, Copy: 100, Sync: 100})
			if err := r.EnsureStarted(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("backup %s: restart after backup: %v", id, err)
			}
		}()
	}

	// From here until the restart the app is down, so the window is bounded.
	//
	// The whole reason the direct shape gives up provider-independence is that a
	// hanging repository now extends an outage instead of merely failing a backup.
	// This turns that back into "the backup failed, the app is up": the engine's
	// container is killed and removed (internal/engine), the deferred restart runs,
	// and the failure lands on the tile.
	//
	// It has no effect on the local engine, whose copy is a local file walk that does
	// not watch the context — which is correct, because a local copy cannot hang on
	// anything but the disk itself.
	downCtx := ctx
	if wasRunning {
		var cancel context.CancelFunc
		downCtx, cancel = context.WithTimeout(ctx, r.stoppedPassTimeout())
		defer cancel()
	}

	// Pass 2 and the commit — the app is down, so whatever this copies is the last word.
	//
	// The commit runs after the deferred restart is queued but before it runs, so the
	// app is still down for it. That is deliberate for the engine whose commit does
	// real work — zipping the snapshot: the alternative, start then zip, races the app
	// writing into a folder we are not reading anyway, and buys nothing, because the
	// zip reads the staged copy rather than the app folder.
	opts.Pass = 2
	for i, p := range ps {
		if res[i].Err != nil {
			continue // pass 1 already failed for this engine; do not hold the app down for it
		}
		// The downtime budget belongs to the APP, not to an engine, so it is one budget
		// shared by every destination rather than one each — a per-engine timeout would
		// silently multiply the maximum outage. An engine that finds it already spent is
		// told so plainly, rather than being started and killed three seconds later with
		// a bare deadline error.
		if err := downCtx.Err(); err != nil {
			res[i].Err = fmt.Errorf("the app's downtime budget was used up before %s could sync: %w", p.ID(), err)
			continue
		}
		emit(r.engineEvent(ps, i, BackupEvent{Phase: PhaseSync, Message: "Syncing changes", Copy: 100}))
		if err := p.Snapshot(downCtx, id, stamp, opts, func(ev Event) {
			emit(r.engineEvent(ps, i, BackupEvent{Phase: PhaseSync, Message: ev.Message, Copy: 100, Sync: ev.Pct,
				Done: ev.Done, Total: ev.Total}))
		}); err != nil {
			res[i].Err = fmt.Errorf("sync app folder: %w", err)
			continue
		}

		b, err := p.Commit(downCtx, id, stamp, opts, func(ev Event) {
			emit(r.engineEvent(ps, i, BackupEvent{
				Phase: PhaseCompress, Message: ev.Message,
				Copy: 100, Sync: 100, Compress: ev.Pct,
				Done: ev.Done, Total: ev.Total,
			}))
		})
		if err != nil {
			res[i].Err = fmt.Errorf("finalise backup: %w", err)
			continue
		}
		committed[i] = true
		res[i].Name = b.Name
	}

	emit(BackupEvent{Phase: PhaseDone, Message: "Backed up", Copy: 100, Sync: 100, Compress: 100})

	// The operation failed only if EVERY destination did. One engine losing its copy is
	// reported on its row and does not throw away the copies that did land — losing the
	// offsite copy of an app is not a reason to also lose the local one.
	if err := firstErr(res); err != nil && !anyCommitted(committed) {
		return res, err
	}
	return res, nil
}

// engineEvent stamps an event with the engine it came from and scales its progress
// tracks into that engine's share of the operation.
//
// Both halves matter. The engine is what lets the Tracker keep a separate rate and ETA
// per destination — two engines are two different pieces of work at two different
// speeds, and one anchor across both derives a number that is fiction. The scaling is
// what stops the bar restarting: each track is 0-100, so three engines would drive it
// to 100 three times, which reads as a backup that keeps starting over. Same trick, and
// the same reason, as removeContainers' `earlier`.
func (r *Registry) engineEvent(ps []Provider, i int, ev BackupEvent) BackupEvent {
	n := len(ps)
	if n == 0 || i < 0 || i >= n {
		return ev
	}
	ev.Engine = ps[i].ID()
	ev.EngineIndex, ev.Engines = i+1, n
	share := func(v float64) float64 { return (float64(i) + v/100) / float64(n) * 100 }
	ev.Copy, ev.Sync, ev.Compress = share(ev.Copy), share(ev.Sync), share(ev.Compress)
	return ev
}

func anyCommitted(committed []bool) bool {
	for _, c := range committed {
		if c {
			return true
		}
	}
	return false
}

// firstErr is the error to report for the operation as a whole, in engine order, so
// the message names a destination rather than being whichever goroutine lost a race.
func firstErr(res []Result) error {
	for _, r := range res {
		if r.Err != nil {
			return fmt.Errorf("%s: %w", r.Engine, r.Err)
		}
	}
	return nil
}

// defaultStoppedPassTimeout bounds how long an app may be held down for a backup.
//
// It is one budget for the whole stopped window however many engines are writing:
// what it promises is how long the APP is down, and the app cannot be down per engine.
// Generous enough that a large delta over a slow uplink finishes, short enough that
// a hung repository is an inconvenience rather than an outage — and with several
// destinations sharing it, an engine that finds it spent fails while the app comes
// back up, which is the right way round.
const defaultStoppedPassTimeout = 15 * time.Minute

func (r *Registry) stoppedPassTimeout() time.Duration {
	if r.StoppedPassTimeout > 0 {
		return r.StoppedPassTimeout
	}
	return defaultStoppedPassTimeout
}

// freeSpace reports free bytes on the filesystem holding dir, or -1 when it cannot
// be read — in which case callers proceed and let the write fail honestly rather
// than refusing on a guess.
func freeSpace(dir string) int64 {
	u, err := disk.Usage(dir)
	if err != nil {
		log.Printf("backup: disk usage for %s: %v (skipping free-space guard)", dir, err)
		return -1
	}
	return int64(u.Free)
}

// engineFor resolves ONE named engine, for a caller that picked a target.
//
// An empty ID is not valid here — "wherever the settings say" is a *set*, and
// enginesFor below is the function that answers it. Keeping the two apart is what
// stopped the empty string quietly meaning "the first engine" once there could be
// several.
func (r *Registry) engineFor(id string) (Provider, error) {
	if r.Engines == nil {
		// No engine set: the built-in local provider, which is what every caller that
		// predates the seam gets. Naming another engine here cannot be honoured.
		if id != "" && id != EngineLocal {
			return nil, fmt.Errorf("unknown backup engine: %s", id)
		}
		return NewLocalProvider(r.cfg), nil
	}
	p, ok := r.Engines.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown backup engine: %s", id)
	}
	return p, nil
}

// enginesFor resolves where a backup should be written.
//
// An empty ID means "wherever this trigger is configured to go", which is a set and
// may be several engines — the nightly run and an uninstall both take this path,
// because nobody is there to pick. A named ID narrows THIS ONE operation to that
// engine: "keep a local copy of this app too", or "push this one offsite now".
//
// The narrowing is deliberately a target and not a stored per-app preference. A
// preference would be per-app *participation* — a thing the nightly run would also
// have to honour — and if the two ever disagreed, an app the user believed was going
// offsite would quietly stop. That is the one outcome this feature must not produce.
//
// **An empty result is an error, not an empty backup.** A run that writes nowhere and
// returns success is the worst failure this package can produce: the scheduler would
// record a healthy run, the incident register would stay quiet, and the box would look
// backed up while nothing had been written.
func (r *Registry) enginesFor(id string, t Trigger) ([]Provider, error) {
	if id != "" {
		p, err := r.engineFor(id)
		if err != nil {
			return nil, err
		}
		return []Provider{p}, nil
	}
	if r.Engines == nil {
		return []Provider{NewLocalProvider(r.cfg)}, nil
	}
	ps := r.Engines.Writers(t)
	// Deduped by ID rather than trusted: the set is assembled from configuration, and
	// one engine listed twice would take two passes over the same folder and race its
	// own staging directory.
	out := make([]Provider, 0, len(ps))
	seen := map[string]bool{}
	for _, p := range ps {
		if p == nil || seen[p.ID()] {
			continue
		}
		seen[p.ID()] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no backup engine is set to receive %s backups — choose one in Settings, Backups", t)
	}
	return out, nil
}

// locate finds the engine that holds a backup and the backup itself.
//
// An empty engine means "whichever engine has it", which is right for the store's
// install-from-backup path and wrong everywhere a user picked a row: two engines can
// hold the same stamp, and restoring the other one is not what was clicked.
//
// Dispatch is on where the backup *is*, never on which engine is selected — the
// rule that keeps a user's existing backups reachable after they switch engines.
func (r *Registry) locate(ctx context.Context, id, engine, name string) (Provider, Backup, error) {
	if r.Engines != nil {
		if engine != "" {
			return r.Engines.LocateIn(ctx, engine, id, name)
		}
		return r.Engines.Locate(ctx, id, name)
	}
	b, _, err := resolveBackup(r.cfg.BackupsDir(), id, name)
	if err != nil {
		return nil, Backup{}, err
	}
	return NewLocalProvider(r.cfg), b, nil
}

// Restore puts a backup back as the app's live folder, without going through the
// store. The app is stopped for it and started again afterwards if it was running,
// on the same deferred-restart rule as Backup.
//
// Which of three paths it takes depends on where the backup is and whether there is
// room, never on which engine is currently selected:
//
//	on disk            rename it into place, after renaming the live folder aside.
//	                   Instantaneous, atomic, and the displaced state becomes an
//	                   archive of its own, so the restore is undoable.
//	remote, room       fetch it beside the app, then exactly the above.
//	remote, no room    write it over the live folder. Not atomic; the undo is a
//	                   snapshot taken first. See restoreInPlace.
func (r *Registry) Restore(ctx context.Context, id, engine, name string, emit func(BackupEvent)) error {
	if emit == nil {
		emit = func(BackupEvent) {}
	}
	if !projectRe.MatchString(id) {
		return fmt.Errorf("invalid app name: %s", id)
	}
	src, _, err := r.locate(ctx, id, engine, name)
	if err != nil {
		return err
	}

	r.enter(id)
	defer r.leave(id)

	appDir := filepath.Join(r.cfg.AppsDir(), id)
	_, liveErr := os.Stat(appDir)
	live := liveErr == nil

	wasRunning := live && r.isRunning(ctx, id)
	if wasRunning {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Stopping " + id})
		if err := r.dx.StopProject(ctx, id); err != nil {
			return fmt.Errorf("stop app: %w", err)
		}
		defer func() {
			emit(BackupEvent{Phase: PhaseStart, Message: "Starting " + id})
			if err := r.EnsureStarted(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("restore %s: restart after restore: %v", id, err)
			}
		}()
	}

	downCtx := ctx
	if wasRunning {
		var cancel context.CancelFunc
		downCtx, cancel = context.WithTimeout(ctx, r.stoppedPassTimeout())
		defer cancel()
	}

	// Held by an engine that restores by rename? Then it costs nothing, which is what
	// makes the local tier worth keeping for apps that fit it.
	//
	// The question is asked of the ENGINE THE USER PICKED, not of whether a file with
	// this name happens to be on the data disk. Those were nearly the same thing while
	// two engines rarely held one stamp; a backup written to several engines at once
	// makes them the same name by design, and probing the disk would answer "local" for
	// a row the user clicked in the repository's tab — then restoreBySwap would consume
	// the local archive, so the row they did not click would vanish and the one they did
	// would sit there untouched. See the identity rule in backup.Set.
	if src.Caps().InstantRestore {
		return r.restoreBySwap(downCtx, id, name, live, emit)
	}

	// Otherwise it has to come out of a repository. Downloading it beside the app
	// needs room for a second full copy; when there is room that is the better path,
	// because the swap that follows is atomic and leaves a local archive behind. When
	// there is not — the case this whole design exists for — the engine writes over
	// the live folder instead.
	est, err := r.EstimateRestore(id, engine, name)
	if err != nil {
		return err
	}
	if est.Enough {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Fetching " + name})
		if err := src.Materialize(downCtx, id, name, func(ev Event) {
			emit(BackupEvent{Phase: PhaseRestore, Message: ev.Message, Copy: ev.Pct,
				Done: ev.Done, Total: ev.Total})
		}); err != nil {
			return fmt.Errorf("fetch backup: %w", err)
		}
		return r.restoreBySwap(downCtx, id, name, live, emit)
	}
	if !src.Caps().InPlaceRestore {
		return fmt.Errorf("not enough free space to restore %s: %s needed, %s available, and %s cannot restore in place",
			id, humanBytes(est.Needed), humanBytes(est.Free), src.ID())
	}
	return r.restoreInPlace(downCtx, src, id, name, appDir, emit)
}

// RestoreForInstall puts a backup back as the app's folder for an app that is **not
// installed** — the store's install-from-backup path, where the user picked an app in
// the catalog and one of its backups and there is nothing on disk yet. The install
// then runs over what this leaves, keeping the restored .env and data.
//
// It is deliberately not Restore. Restore is for an app that is *live*: it stops the
// app, archives the folder it is about to replace so a mis-click is undoable, and
// falls back to writing over that folder when there is no room for two copies. None of
// that applies here — there is no folder and nothing is running — so this is the short
// path. RestoreBackup still refuses to overwrite a folder, so an app that appeared
// between the click and now fails loudly rather than being written over.
//
// What it replaces is a direct apps.RestoreBackup call, which read the data disk and
// nothing else. That was the same "writes go through the engine set, reads do not" bug
// the Backups page had: on a box whose backups live in a repository the store could
// neither list them nor install from them, and the seam to do it already existed.
func (r *Registry) RestoreForInstall(ctx context.Context, id, engine, name string, emit func(Event)) error {
	if !projectRe.MatchString(id) {
		return fmt.Errorf("invalid app name: %s", id)
	}
	if emit == nil {
		emit = func(Event) {}
	}

	r.enter(id)
	defer r.leave(id)

	// The engine comes from the row the user clicked — the store lists a group per
	// engine — so this resolves through LocateIn. Empty still means "whichever engine
	// has it", which is what a client that predates the grouped list sends.
	src, _, err := r.locate(ctx, id, engine, name)
	if err != nil {
		return err
	}

	// An archive held by an engine that restores by rename is already where
	// RestoreBackup looks, so there is nothing to fetch and no space to check. Only a
	// backup that lives in a repository has to come down first.
	//
	// Asked of the located engine rather than of the data disk, for the reason spelled
	// out in Restore: with fan-out the same stamp exists in several engines, and a disk
	// probe would quietly redirect an install from the repository row the user chose.
	if !src.Caps().InstantRestore {
		est, err := r.EstimateRestore(id, engine, name)
		if err != nil {
			return err
		}
		// No in-place fallback, unlike Restore. In-place exists to write over a live
		// folder that is too big to duplicate; here there is no folder to write over and
		// no app to keep running, so refusing early is the whole answer.
		if !est.Enough {
			return fmt.Errorf("not enough free space to install %s from its backup: %s needed, %s available",
				id, humanBytes(est.Needed), humanBytes(est.Free))
		}
		emit(Event{Pct: PctUnknown, Message: "Fetching " + name})
		if err := src.Materialize(ctx, id, name, emit); err != nil {
			return fmt.Errorf("fetch backup: %w", err)
		}
	}
	return RestoreBackup(r.cfg.BackupsDir(), r.cfg.AppsDir(), id, name)
}

// restoreBySwap is the original restore: archive the live folder by renaming it —
// instantaneous, and what makes a mis-clicked restore undoable — then move the
// archive into its place.
func (r *Registry) restoreBySwap(ctx context.Context, id, name string, live bool, emit func(BackupEvent)) error {
	backupsDir := r.cfg.BackupsDir()
	if live {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Archiving current state"})
		dir := AppBackupDir(backupsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create backup dir: %w", err)
		}
		safety := filepath.Join(dir, time.Now().Format(StampLayout))
		if err := os.Rename(filepath.Join(r.cfg.AppsDir(), id), safety); err != nil {
			return fmt.Errorf("archive current state: %w", err)
		}
	}
	emit(BackupEvent{Phase: PhaseRestore, Message: "Restoring " + name})
	if err := RestoreBackup(backupsDir, r.cfg.AppsDir(), id, name); err != nil {
		return err
	}
	emit(BackupEvent{Phase: PhaseDone, Message: "Restored", Copy: 100, Sync: 100, Compress: 100})
	return nil
}

// restoreInPlace writes the backup over the live app folder, for an app too large
// to hold two copies of.
//
// This is the one destructive path in Maison's backup handling, and it is not
// atomic: an interruption leaves the folder neither the old state nor the new one.
// Three things make that survivable, and none of them is optional.
func (r *Registry) restoreInPlace(ctx context.Context, src Provider, id, name, appDir string, emit func(BackupEvent)) error {
	// 1. The undo. There is no rename to make a safety archive from — the live folder
	//    *is* the target — so the only way back is a snapshot of the current state.
	//    The app is already stopped, so it is consistent, and it is incremental, so it
	//    costs roughly the delta since the last backup.
	//
	//    If it fails the restore is refused. An unrecoverable overwrite is a worse
	//    outcome than a restore that did not happen.
	undo := time.Now().Format(StampLayout)
	emit(BackupEvent{Phase: PhaseRestore, Message: "Saving current state"})
	// Honouring the app's exclusions here too. This path exists *because* the app
	// barely fits on its own disk, which is the moment a derived tree is least worth
	// a second copy — and an undo that restores the cache with everything else would
	// be putting back what the next backup drops again.
	if err := src.Snapshot(ctx, id, undo, SnapshotOpts{Pass: 2, Exclude: r.excludeSet(id)}, nil); err != nil {
		return fmt.Errorf("could not save the current state, so the restore was refused: %w", err)
	}
	if _, err := src.Commit(ctx, id, undo, SnapshotOpts{}, nil); err != nil {
		return fmt.Errorf("could not save the current state, so the restore was refused: %w", err)
	}

	// 2. The marker. It lives outside the folder being written, because a restore
	//    that deletes files absent from the backup would otherwise delete it. Its
	//    name cannot parse as a stamp, so ListBackups ignores it for free.
	if err := r.markRestoring(id, name); err != nil {
		return err
	}

	emit(BackupEvent{Phase: PhaseRestore, Message: "Restoring " + name})
	if err := src.RestoreInPlace(ctx, id, name, appDir, func(ev Event) {
		emit(BackupEvent{Phase: PhaseRestore, Message: ev.Message, Copy: ev.Pct,
			Done: ev.Done, Total: ev.Total})
	}); err != nil {
		// 3. The marker stays. The app must not come back up on a half-restored
		//    folder: it would initialise over the gap — fresh database, default
		//    config — and that state would become the next night's backup.
		return fmt.Errorf("restore %s: %w (its data is incomplete; restore it again or roll back to %s)", id, err, undo)
	}
	if err := r.clearRestoring(id); err != nil {
		return err
	}
	emit(BackupEvent{Phase: PhaseDone, Message: "Restored", Copy: 100, Sync: 100, Compress: 100})
	return nil
}

// restoringMarker is written while an app's folder is being overwritten in place.
// The name deliberately fails stampRe so no lister mistakes it for an archive.
func (r *Registry) restoringMarker(id string) string {
	return filepath.Join(AppBackupDir(r.cfg.BackupsDir(), id), ".restoring")
}

func (r *Registry) markRestoring(id, name string) error {
	if err := os.MkdirAll(AppBackupDir(r.cfg.BackupsDir(), id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.restoringMarker(id), []byte(name), 0o644)
}

func (r *Registry) clearRestoring(id string) error {
	err := os.Remove(r.restoringMarker(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RestoreInterrupted reports whether an in-place restore of this app was cut short,
// leaving its folder holding neither the old state nor the new one.
func (r *Registry) RestoreInterrupted(id string) bool {
	_, err := os.Stat(r.restoringMarker(id))
	return err == nil
}

// EstimateRestore answers "is there room to fetch this backup and swap it in".
//
// It is the mirror of EstimateBackup and exists for the same reason: downloading a
// backup beside the app needs space the app itself may already be occupying. It is
// what chooses between the two restore paths.
//
// engine names which copy is being sized, on the same rule as everything else that
// reads a picked row: two engines can hold the same stamp, and a local archive and an
// offsite snapshot of it are not the same number of bytes to bring down. Empty means
// "whichever engine has it", which is only right for a caller that has none to name.
func (r *Registry) EstimateRestore(id, engine, name string) (Estimate, error) {
	if !projectRe.MatchString(id) {
		return Estimate{}, fmt.Errorf("invalid app name: %s", id)
	}
	_, backup, err := r.locate(context.Background(), id, engine, name)
	if err != nil {
		return Estimate{}, err
	}
	size := backup.Size
	if size == 0 {
		// A folder archive is listed unmeasured, and a repository that reports no size
		// tells us nothing — fall back to the live app, which is the best available
		// proxy for how big its backup is.
		// Excluding what the app declares as derived, because that is what the backup
		// itself left out — sizing the live folder whole would over-reserve by exactly
		// the cache the author asked not to store.
		size, _ = measureDir(filepath.Join(r.cfg.AppsDir(), id), r.excludeSet(id))
	}
	needed := int64(float64(size) * folderHeadroom)
	free := freeSpace(r.cfg.DataRoot)
	if free < 0 {
		return Estimate{Size: size, Needed: needed, Free: -1, Enough: true}, nil
	}
	return Estimate{Size: size, Needed: needed, Free: free, Enough: free >= needed}, nil
}

// Running reports whether any of the app's containers are up. See isRunning for why
// a Docker hiccup answers no.
func (r *Registry) Running(ctx context.Context, id string) bool { return r.isRunning(ctx, id) }

// isRunning reports whether any of the project's containers are up. A Docker
// hiccup answers "no", which is the safe direction: the worst case is that a
// stopped-looking app is not restarted, and the operator can start it from the
// tile.
func (r *Registry) isRunning(ctx context.Context, id string) bool {
	if r.dx == nil {
		return false
	}
	conts, err := r.dx.ListProjectContainers(ctx)
	if err != nil {
		return false
	}
	for _, c := range conts {
		if c.Project == id && c.State == "running" {
			return true
		}
	}
	return false
}

func pct(done, total int64) float64 {
	if total <= 0 {
		return 100
	}
	return float64(done) / float64(total) * 100
}

// mirror makes dst an exact copy of src, and reports progress over the bytes it
// actually transfers.
//
// It is incremental on purpose — a file already in dst with the same size and
// modification time is left alone. That is what makes the second pass cheap: it
// copies only what the app wrote during the first one, which is the entire reason
// the app's downtime is short. Files in dst that are gone from src are removed, so
// a second pass cannot leave deleted data in the snapshot.
//
// Rather than shelling out to rsync (which the runtime image does not carry), this
// is a plain two-walk implementation: it needs no external binary and reports
// bytes through the same progressReader the zip path uses.
//
// skip is what the app declared as derived (x-compose-app backup.exclude); a nil Set
// mirrors everything. It is applied in BOTH walks — leaving it out of the measuring
// one would make the bar promise bytes that are never moved — and again in prune, so
// dst holds nothing the caller asked to leave behind.
func mirror(src, dst string, skip *exclude.Set, onProgress func(copied, total int64)) error {
	if onProgress == nil {
		onProgress = func(int64, int64) {}
	}

	// Measure the work first so the bar means something: only the files that will
	// actually be transferred count, so an unchanged second pass reads as instant
	// rather than as a full copy that mysteriously flies by.
	var total int64
	stale := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if skip.Match(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return fs.SkipDir // an excluded tree is never even walked
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if upToDate(filepath.Join(dst, rel), fi) {
			return nil
		}
		stale[rel] = true
		total += fi.Size()
		return nil
	})
	if err != nil {
		return err
	}

	var copied int64
	onProgress(0, total)

	// Pass A — create directories and copy the stale files.
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if skip.Match(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Sockets, fifos and devices are runtime artefacts, not data: an app that
		// leaves a socket in its folder must not make its backup unrunnable.
		if !d.Type().IsRegular() {
			return nil
		}
		if !stale[rel] {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(path, target, fi, func(n int64) {
			copied += n
			onProgress(copied, total)
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	onProgress(total, total)

	// Pass B — drop anything in dst that src no longer has, or that is now excluded.
	return prune(src, dst, skip, "")
}

// upToDate reports whether dst already holds this exact file. Size plus
// modification time is the same test rsync makes by default: cheap, and wrong only
// for a write that preserves both, which no app does by accident.
func upToDate(dst string, src fs.FileInfo) bool {
	fi, err := os.Stat(dst)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Size() == src.Size() && fi.ModTime().Equal(src.ModTime())
}

// copyFile writes src to dst through a temporary, then renames — so an
// interrupted copy leaves the previous version (or nothing), never a truncated
// file that upToDate would go on to accept as current. The source's mode and
// modification time are carried over, the latter because the next pass's
// up-to-date test depends on it.
func copyFile(src, dst string, fi fs.FileInfo, onRead func(n int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, &progressReader{r: in, onRead: onRead}); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, fi.ModTime(), fi.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// prune removes everything under dst that no longer exists under src, or that the
// app now declares as derived. Directories are handled after their contents, so
// removing a whole deleted subtree takes one RemoveAll at its root rather than a
// walk. rel is the path of dst relative to the mirror root, which is what the
// exclusion Set is written in terms of; the top-level call passes "".
//
// The exclusion half is defensive rather than load-bearing: a staging directory is
// cleared before pass 1, so a tree that stopped being copied cannot normally still
// be sitting there. But the copy pass simply never visits an excluded path, so
// nothing else here would ever remove one — and "unreachable given today's call
// sites" is a thin guarantee when the failure it guards is silently shipping the
// data an author asked to leave out.
func prune(src, dst string, skip *exclude.Set, rel string) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		target := filepath.Join(dst, e.Name())
		name := strings.TrimSuffix(e.Name(), ".partial")
		if skip.Match(path.Join(rel, name)) {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			continue
		}
		// A .partial left by a failed copy belongs to no source file; drop it.
		origin := filepath.Join(src, name)
		if _, err := os.Stat(origin); err != nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			continue
		}
		if e.IsDir() {
			if err := prune(filepath.Join(src, e.Name()), target, skip, path.Join(rel, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// humanBytes formats a byte count for an error message the operator reads.
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
