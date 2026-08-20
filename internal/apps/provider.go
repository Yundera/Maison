package apps

import (
	"context"
	"errors"
)

// A Provider is one backup engine: local archives, kopia, restic, whatever comes
// next. It is declared here, in the package that *consumes* it, so that an engine
// implementation can import this package (it needs Backup, and the local engine
// needs mirror/archiveDir) without this package having to import any engine.
//
// The split of responsibility is the load-bearing part, and it is what keeps an
// engine from being able to hurt an app:
//
//   - The registry owns everything that is not storage — the per-app lock, the
//     stop → snapshot → *deferred* restart sequence, the two-pass structure, the
//     tracked BackupState, idempotency, and the sticky error on the tile.
//   - A provider owns exactly one thing: getting bytes to durable storage and back.
//
// So no engine can extend an app's downtime by restructuring the sequence, and no
// engine can produce an inconsistent snapshot by choosing when to read.
//
// See docs/backup.md, which is authoritative for this design.
type Provider interface {
	// ID is the engine's permanent identifier ("local", "kopia", …). It is written
	// into every Backup this engine produces and is how an existing backup finds its
	// way home after the user switches engines, so **it may never change or be
	// reused** once shipped.
	ID() string

	// Caps declares what this engine can do. Callers branch on capabilities rather
	// than on ID, so that adding an engine does not mean editing a switch statement
	// somewhere far away.
	Caps() Caps

	// Snapshot captures the app folder's current state under (app, stamp).
	//
	// It is called twice for one backup — pass 1 with the app running, pass 2 with
	// it stopped — against the same (app, stamp). Implementations must therefore be
	// incremental: pass 2 exists to capture only what changed during pass 1, and
	// that is the entire reason the app's downtime is proportional to the delta
	// rather than to its size.
	//
	// Nothing produced here is durable until Commit succeeds.
	Snapshot(ctx context.Context, app, stamp string, opts SnapshotOpts, emit func(Event)) error

	// Commit makes (app, stamp) a real, listable backup and returns it. This is the
	// operation's commit point: before it, an interrupted backup must leave nothing
	// that List would return; after it, the backup is complete.
	//
	// It returns the Backup rather than just an error because only the engine knows
	// what it actually produced — the local engine's zip mode names its artefact
	// "<stamp>.zip" while every other case is "<stamp>", and that is engine-internal
	// knowledge the caller should not have to reproduce.
	Commit(ctx context.Context, app, stamp string, opts SnapshotOpts, emit func(Event)) (Backup, error)

	// Abort discards whatever an interrupted Snapshot left behind. It is best-effort
	// and its error is logged, never surfaced — a failed cleanup must not mask the
	// failure that caused it.
	Abort(ctx context.Context, app, stamp string) error

	// List returns this engine's backups of one app, newest first, with Tier and
	// Engine set. It must tolerate an app it has never heard of by returning
	// nothing, not an error: the global page asks every engine about every app.
	List(ctx context.Context, app string) ([]Backup, error)

	// ListAll returns every backup this engine holds, grouped by app, each group newest
	// first with Tier and Engine set. It backs the global page.
	//
	// It is one call rather than "which apps do you have" followed by a List per app
	// because for a remote engine each call is a subprocess against the repository, and
	// the per-app shape would make the page cost one of those per installed app. A
	// repository can answer this in a single query; the interface should let it.
	//
	// It cannot be derived from what is installed. The case that matters is a rebuilt
	// box — nothing installed, nothing on the data disk, a repository freshly
	// reconnected — where the repository is the only thing that still knows the apps
	// existed. Enumerating installed apps would return nothing at exactly the moment
	// the list matters most.
	//
	// App names that are not valid compose projects must be dropped rather than
	// returned: they feed path construction downstream, and a repository is untrusted
	// input.
	ListAll(ctx context.Context) (map[string][]Backup, error)

	// Materialize places (app, stamp) on local disk at .backups/<app>/<stamp> so the
	// ordinary restore path can swap it in. For an engine whose backups are already
	// there it is a no-op; for a remote one it is a download, and the caller is
	// responsible for having checked there is room.
	Materialize(ctx context.Context, app, stamp string, emit func(Event)) error

	// RestoreInPlace writes (app, stamp) directly over dst, without staging a second
	// copy anywhere — the only way to restore an app too large to fit twice on its
	// own disk. Files present in dst but absent from the backup must be removed, or
	// the result is a merge rather than a restore.
	//
	// Engines that cannot do this return ErrNotSupported and Caps().InPlaceRestore is
	// false. It is not atomic: an interruption leaves dst neither the old state nor
	// the new one, which is why callers take an undo snapshot first.
	RestoreInPlace(ctx context.Context, app, stamp, dst string, emit func(Event)) error

	// Delete removes one backup from this engine.
	Delete(ctx context.Context, app, stamp string) error
}

// Engines is the set of backup engines a deployment knows about. It is declared
// here, and satisfied by internal/backup.Set, for the same reason Provider is: the
// Registry needs to read across every engine, and cannot import the package that
// assembles them without a cycle.
//
// The distinction the interface encodes is the one everything else depends on:
// Writer is a *choice*, and only new backups follow it. List and Locate answer
// "where is this backup", which must not depend on that choice — otherwise
// switching engine strands everything the previous one wrote.
type Engines interface {
	Writer() Provider
	List(ctx context.Context, app string) []Backup
	// LocateIn finds a backup in one named engine — the shape every user-initiated
	// restore uses, because the user picked a row and a row belongs to an engine.
	LocateIn(ctx context.Context, engine, app, stamp string) (Provider, Backup, error)
	// Locate is the engine-less form, for the store's install-from-backup path.
	Locate(ctx context.Context, app, stamp string) (Provider, Backup, error)
	Delete(ctx context.Context, engine, app, stamp string) error
}

// UserDataRestoreOpts says what to put back from the user-data set, and where.
//
// It lives here, with Provider and Backup, because both the engine that implements the
// restore and the coordinator that guards it need the vocabulary and neither can import
// the other.
type UserDataRestoreOpts struct {
	// Dest is where the files land. Empty means **in place**, over the data root: the
	// destructive mode. Any other value is a directory the snapshot is written into,
	// which touches nothing the user already has.
	Dest string

	// Entries limits the restore to these top-level names ("Documents", "Media"). Empty
	// means everything the snapshot holds. An engine must match them against the
	// snapshot's own listing and refuse anything else, so a caller cannot name a path.
	Entries []string
}

// Caps describes an engine's abilities. Zero values are the conservative answer,
// so a new field defaults to "this engine cannot", not "this engine can".
type Caps struct {
	// Offsite is true when the backup survives the loss of the machine. The local
	// engine is the one that is not: it writes to the same disk as the apps, which
	// makes it a rollback mechanism rather than disaster recovery.
	Offsite bool

	// InstantRestore is true when restoring is a rename rather than a copy or a
	// download. It is what makes the local tier worth keeping for apps that fit it.
	InstantRestore bool

	// NeedsLocalSpace is true when a backup requires free disk proportional to the
	// app — which is what makes the local engine refuse an app larger than the
	// remaining disk. An engine that streams straight to a repository does not.
	NeedsLocalSpace bool

	// InPlaceRestore is true when the engine can restore over a live app folder
	// without staging a second copy. It is the only way to restore an app too large
	// to fit twice on its own disk.
	InPlaceRestore bool

	// Retention is true when the engine applies its own retention policy, so Maison
	// configures the tiers instead of deleting backups itself.
	Retention bool
}

// SnapshotOpts carries the per-call knobs the registry passes down.
type SnapshotOpts struct {
	// Pass is 1 for the live pass and 2 for the stopped pass. Pass 1 may be torn and
	// must never be offered for restore; pass 2 is the consistent one.
	Pass int

	// Zip asks for a compressed archive. It is meaningful only to the local engine —
	// engines that deduplicate ignore it, because a zip is one opaque blob whose
	// every byte changes when anything inside it does, which defeats deduplication
	// and turns every backup into a full upload.
	Zip bool
}

// Event is a progress update from a provider. Pct is optional: an engine that can
// only report that it started and finished sets Message alone and leaves Pct at -1,
// and the bar renders as indeterminate rather than as a lie.
type Event struct {
	Pct     float64
	Message string
}

// PctUnknown marks an Event that carries no measurable progress.
const PctUnknown = -1.0

// ErrNotConfigured is returned by an engine that has no usable configuration — no
// repository connected, no credentials rendered yet. It is a normal state on a box
// whose host-side setup has not run, not a failure, and callers turn it into "not
// configured" in the UI rather than an error.
var ErrNotConfigured = errors.New("backup engine is not configured")

// ErrNotSupported is returned for an operation this engine cannot perform, such as
// an in-place restore on an engine whose backups are plain folders.
var ErrNotSupported = errors.New("operation not supported by this backup engine")
