# Backup and recovery

> **Status: mostly implemented.** The engine seam, the `local` and `kopia` engines,
> the two-pass app backup, the user-data set, all three restore paths, the nightly
> schedule, retention and failure notifications are in the tree and tested.
>
> **Not yet built:** disaster recovery / recovery mode ([below](#disaster-recovery)),
> and the host-side `ensure-backup-config.sh` that renders storage credentials onto
> the box — until that exists, a repository is connected by hand, which is also how
> this is developed and tested.
>
> Two things changed during implementation and are corrected in place below: there is
> **no local staging copy** on the remote path (§[Why there is no local staging
> copy](#why-there-is-no-local-staging-copy)), and restoring an app too large to hold
> two copies of writes **in place**, which is not atomic (§[Restore](#restore)).

Its companions:

- [`app-model.md`](./app-model.md) — where an app **lives** on disk. Backup is
  defined entirely in terms of that layout; read it first.
- [`lifecycle.md`](./lifecycle.md) — install / start / update / uninstall. Backup
  hangs off the stop and restart sequences documented there, and does not invent
  its own.

The server-side half — how a PCS is issued storage credentials in the first place —
is out of scope here and is treated as a contract Maison consumes. See
[What Maison consumes from the PCS](#what-maison-consumes-from-the-pcs).

---

## The two sets

Maison backs up **two independent things**, and almost every design consequence in
this document follows from them being different.

| | Apps | User data |
|---|---|---|
| Source | `${DATA_ROOT}/AppData/<app>`, one source per app | `${DATA_ROOT}` minus `AppData/` |
| Contents | compose files, `.env`, the app's own data | Documents, Downloads, Media, whatever else the user drops at the data root |
| Consistency | containers stopped — a real point-in-time snapshot | none; a live filesystem, read as-is |
| Orchestration | stop → snapshot → start, per app | none — nothing to stop |
| Granularity | restore one app without touching others | one set |

**User data is the simpler path and the right thing to build first.** Nothing is
stopped, no staging copy exists, and its source path never changes. Every piece of
the provider layer can be exercised against it before app orchestration enters the
picture.

**A database must never live under the user-data set.** It has no consistency
guarantee: a file being written while the engine reads it is captured mid-write.
That is normal and accepted for documents and media, and it is precisely why apps
get the stop treatment and user data does not.

---

## Where things live on disk

```
${DATA_ROOT}/
  AppData/
    <app>/                     an app — the backup source for the app set
    .backups/<app>/<stamp>/    local archives (exists today: Config.BackupsDir)
    maison/                    Maison's own state (Config.StateDir)
  AppDataShared/
    backup/<engine>/
      repository.config        endpoint, region, bucket, prefix
      repository.password      0600, generated on this PCS
      cache/                   engine cache — excluded from backup
      logs/                    excluded from backup
  Documents/ Downloads/ Media/ …   the user-data set
```

`AppDataShared/` is **deliberately outside `AppData/`**, which means it falls inside
the user-data set and therefore gets backed up. That is the point: on a box running
more than one engine, each engine's backup carries the other's configuration, so
recovering either one returns the rest. The operator has to have kept **one** set of
credentials, not one per engine.

Two exclusions make that safe, and they are matched **by pattern** (`**/cache/`,
`**/logs/`) rather than by a fixed list — otherwise the third engine someone adds
later silently ships its cache offsite forever:

- **`cache/` must never be backed up.** It is multi-gigabyte, it turns over between
  runs, and it dedups poorly because its contents are already compressed and
  encrypted. Backing it up inflates snapshot time and inflates metered usage against
  the storage quota, nightly, for data that is rebuilt on demand.
- **`logs/`** — same churn, no value.

`repository.password` riding along inside the backup is **harmless**, and worth
stating because it looks alarming: reading it requires the password already. It is
not a leak, it is merely useless — the recovery path runs on the emailed copy, never
on the repo. `repository.config` riding along is mildly useful; a restored box gets
its endpoint, bucket and prefix back without re-deriving them.

`.backups/` needs no exclusion. It lives inside `AppData/` and is therefore already
outside the user-data set.

---

## The engine is pluggable

The engine is a **user-facing choice** — kopia first, then restic, then custom — set
globally rather than per app. The seam is the deliverable; the first engine is just
the first tenant of it.

```go
type Provider interface {
    ID() string        // "local" | "kopia" | "restic" | …
    Caps() Caps        // LocalSpace, Instant, Retention, Offsite
    Snapshot(ctx, app, stamp string, emit func(Event)) (Ref, error)
    List(ctx, app string) ([]Backup, error)
    Materialize(ctx, app, stamp string, emit func(Event)) error
    Delete(ctx, app, stamp string) error
}
```

### What the registry owns, and what a provider owns

The registry keeps everything that is not storage: the per-app `enter`/`leave` lock,
stop → snapshot → **deferred** restart, the two-pass structure, tracked
`BackupState`, idempotency, the sticky error on the tile, and archiving a live folder
before restoring over it. **A provider owns exactly one thing: getting bytes to
durable storage and back.**

That boundary is what keeps a plugin from being able to extend an app's downtime or
produce an inconsistent snapshot.

### The rule that survives a provider switch

> **The active provider governs writes only.**

Listing is the **union** across all known providers, and restore and delete dispatch
on *where the backup actually is* — not on which provider is currently selected.
Without this, switching from kopia to restic orphans every kopia snapshot and every
local zip.

Two consequences:

- **`ID()` values are permanent once shipped.** They are how an existing backup finds
  its way home.
- **A provider removed from the picker stays registered read-only.** You can stop
  offering an engine; you cannot stop reading what it wrote.

---

## Backing up an app

### The shape

```
pass 1   app running    snapshot AppData/<app>  →  S1   bulk upload; torn, throwaway
         stop app
pass 2   app stopped    snapshot AppData/<app>  →  S2   delta only; consistent
         start app                                      deferred — always runs
         delete S1
```

This is a **direct port of what `Registry.Backup` already does**, with the engine
replacing `mirror()`. The existing code runs a live copy pass, stops the app, runs a
second pass, and renames the result into place; its own header comment states the
property that carries over unchanged:

> downtime is proportional to what the app wrote *during* the first pass, not to how
> big it is

The source path is identical across both passes, so the engine's size+mtime fast path
applies on pass 2 and only what changed during pass 1 is re-read and re-uploaded. S2
is taken with the app down, so it is consistent by exactly the argument the current
code already makes.

**The restart is deferred and must stay that way.** Any failure after the stop still
brings the app back up. Leaving an app down is a worse outcome than a missing backup.

### Why there is no local staging copy

Because on a large app there is nowhere to put one, and today that means the backup
simply does not happen.

`Registry.Backup` mirrors the app folder into a staging directory on the same
filesystem — a full second copy — and `EstimateBackup` guards it with
`folderHeadroom = 1.1`. A 300 GB app therefore needs 330 GB free, so on a 400 GB disk
holding it, `Estimate.Enough` is **false and the backup is refused**. That is current
behaviour, not a hypothetical.

Snapshotting `AppData/<app>` directly removes the requirement entirely rather than
raising the ceiling. It also happens to give the stable source path that real
incremental backup needs — the most stable path an app has is its own directory.

What it costs:

- **Downtime is no longer provider-independent.** A hung repository extends an outage
  instead of merely failing a backup. Guarded by the deferred restart above and by a
  **pass-2 timeout**, which turns "the repo is hanging" into "the backup failed, the
  app is up".
- **No local archive for that app**, so its restores are always a `Materialize`
  download. The uninstall path is unaffected — it renames `AppData/<app>` into
  `.backups/<app>/<stamp>`, and a rename costs nothing at any size.

The local tier remains available for apps that fit it — instant restore is worth
having where it is free — but nothing may *require* it. `EstimateBackup` already
computes the number that decides which mode an app gets.

### The throwaway snapshot must be pruned

S1 is torn. Left in the repository it doubles the snapshot count, pollutes the
retention set, and — worst — a user browsing snapshots can restore an inconsistent
one. **Delete S1 once S2 succeeds.** Content blobs are shared, so nothing S2 needs is
lost with it.

If pass-1 churn is high (an Immich mid-import), pass 1 can be repeated until the delta
stops shrinking before stopping the app. Not needed for a first version.

### Per-app exclusions

`AppData/<app>` is the source, but not all of it is worth storing. Apps keep large **regenerable**
caches inside their own directory — thumbnails, transcodes, search indexes, model downloads — and
without a way to skip them, every one ships nightly and is billed as if it mattered. On a media app
the cache can rival the real data.

The declaration belongs in **`x-compose-app`**, next to `folders` and `hooks`: the app author knows
which of their directories are derived, and it travels with the app instead of living in a list
Maison has to maintain per store app. A user-level override is worth having for apps whose authors
have not declared anything.

Two rules keep this from becoming a footgun:

- **Exclusions are a store-app declaration, not a heuristic.** Never infer "this looks like a
  cache" from a directory name — a wrong guess silently omits real data from a backup, which is the
  worst failure this system can have.
- **What was excluded must be visible at restore time.** A restored app missing its cache should
  rebuild it; a restored app missing something the author wrongly marked derived is a bug report,
  and the restore UI showing "these paths were excluded" is what makes that diagnosable.

### Identity is unchanged

`<stamp>` — `YYYY-MM-DD_HHMMSS` — stays the canonical name of a backup. `stampRe`
(`archive.go`) is the traversal guard that makes `DeleteBackup` and `resolveBackup`
safe, and **it must not be loosened** to accommodate an engine whose native refs look
different. A snapshot ID is a provider-internal detail; `(app, stamp)` is the identity
the API routes and the frontend already use, and keeping it removes what would
otherwise be the largest refactor in this work.

---

## Scheduling

**Daily, default 03:00 local, driven by Maison.** Maison has no scheduler today —
backups are uninstall-triggered or manual — so this is new code, not a setting.

It cannot be delegated to the engine's own scheduler at any price: a consistent app
snapshot requires stopping containers, and no backup tool can do that.

Three requirements beyond a ticker:

| | Why |
|---|---|
| **Serialise across apps** | The per-app `enter`/`leave` lock protects one app. Nothing stops a nightly run from taking six apps down at once. |
| **Catch up, do not pile up** | A PCS that was off at 03:00 backs up when it returns; a run that overruns its window is skipped, not queued behind itself. |
| **Jitter per box** | A fixed 03:00 fleet-wide is a self-inflicted thundering herd against one bucket. The `deviceId` issued to the PCS is a ready-made stable seed. |

### The first run is a different problem from the nightly one

Jitter spreads the recurring 03:00 load across minutes. **The first backup a box ever takes needs
spreading across days.**

Configuration reaches the fleet through `ensure-template-sync`, so every PCS becomes
backup-capable at roughly the same moment. Each then seeds its *entire* `/DATA` on its next
scheduled run. Two hundred boxes holding even 100 GB each is tens of terabytes converging on one
bucket in a single window — and the users are concentrated in a handful of timezones, so "03:00
local" barely spreads it.

**Maison owns this**, because Maison owns the trigger. Nothing upstream can stagger it. Derive a
per-box offset over a multi-day window from a stable seed (the `deviceId` works), and hold the
first run until that offset elapses. Subsequent runs fall back to normal nightly jitter.

This matters most on the day the feature ships to existing boxes, which is exactly when it is
least convenient to discover.

### Upload throttling

A backup that saturates a home uplink is a support ticket, and the user will blame the PCS rather
than the backup. Expose a bandwidth cap and set a conservative default; the engines take a flag for
this (`--upload-limit-mb` in kopia's case). It belongs in the same settings surface as the schedule.

---

## Retention

**Tiered / GFS: keep 7 daily, then one per week, then one per month** — the default of
a setting, not a constant.

Because each app is one stable source accumulating snapshots over time, this maps
directly onto an engine's own retention policy (`--keep-daily 7 --keep-weekly 4
--keep-monthly 12`) instead of having to be reimplemented. That is a direct dividend
of the stable source path: with a fresh source per backup, every source would hold
exactly one snapshot and per-source policies would be meaningless.

**Local and remote retention are separate counts.** Local archives cost real disk and
are governed by Maison's own "keep N local"; remote retention may be delegated to the
engine.

> "Remove local" is a **count, not a boolean** — `keep N local` / `keep N remote`,
> where the boolean is N=0. A binary switch means one silently broken repository
> leaves nothing at all.

Local pruning applies only after the provider has **verifiably confirmed** the
snapshot — not merely after a subprocess exited 0.

Delegating remote retention is clean for kopia. An engine whose policy model differs
means Maison expresses the *intent* through the provider interface rather than
assuming the flags.

---

## Encryption and the master password

> **The master password is generated on the PCS and never reaches Yundera.**

It lives at `AppDataShared/backup/<engine>/repository.password`, mode 0600. It has to
stay on the box — an unattended nightly backup cannot prompt for it.

The user's only off-box copy is the setup email (below). Stated plainly, because it
must be stated plainly to users too:

**the PCS holds it, the user holds a copy, Yundera holds nothing and cannot recover
it.**

The consequence is accepted rather than mitigated: a user who loses that mail loses
the backups, and no support path exists. Any future softening has to preserve the
first line — client-side wrapping under a user credential with only ciphertext stored
server-side is the shape that does; server-side derivation is the shape that does not.

---

## Notifications

Maison gets an outbound SMTP client. Two jobs, and only two.

**1. Alert on backup failure.** What makes backups worthless is silent failure, and an
unattended nightly job with no channel out is exactly that. **One mail on transition
into failure, one on recovery** — not one per failed run, which becomes noise and then
a filter rule. The sticky-error state already tracked per tile is the right trigger
source.

**2. Handing the user their encryption key — once, at setup, user-initiated.** This is the
only reason a copy of the password exists anywhere but the box.

> **This one cannot go through the PCS's `smtp` container.** Every PCS runs
> `ghcr.io/yundera/mail-gateway`, which parses the message and forwards it over HTTPS to
> `mesh-router-backend`, which relays via SendGrid. Mail sent that way traverses **Yundera
> infrastructure in plaintext** — which contradicts the claim above that Yundera holds nothing
> and cannot recover the key.

So the two jobs use different transports:

| Job | Transport |
|---|---|
| Failure alerts | The built-in `smtp` container. Zero config, already working, nothing secret in the body. |
| **The key** | **Display in the UI once, with a download.** No mail by default. Optionally, user-supplied SMTP credentials — direct from the PCS, bypassing the relay. |

Display-and-download is not a workaround, it is the better default: it preserves the security
property exactly, removes a dependency, and avoids parking a plaintext secret in an inbox where it
is indexed and retained indefinitely. Whatever the user does with it afterwards is their choice
to make with full knowledge.

Whichever is chosen, it is **once, user-initiated, and clearly labelled as unrecoverable**. It must
never be recurring or automatic.

---

## Restore

### One app

Three paths, chosen by **where the backup is** and whether there is room — never by
which engine is currently selected, which is the same rule that governs listing.

```
on disk           rename the live folder aside, rename the archive in.
                  Instant, atomic, and the displaced state becomes an archive of
                  its own, so the restore is itself undoable.
remote, room      the engine materialises it to .backups/<app>/<stamp>, then
                  exactly the above. RestoreBackup runs unmodified.
remote, no room   the engine writes over the live folder. ~1x space. NOT atomic.
```

`EstimateRestore` chooses between the last two — the restore-side sibling of the
backup guard, and for the same reason: materialising needs room for a full second
copy, which is exactly what an app large enough to need this does not have.

**The in-place path is the one that gives something up.** It is not atomic: an
interruption leaves the folder holding neither the old state nor the new one, and
because there is no local copy the only way back is a remote snapshot — so the
restore is reversible only while the repository is reachable. That is the trade for
being able to restore an app too large to fit twice on its own disk, and it is why
three guards are mandatory rather than best-effort:

1. **An undo snapshot is taken first, and if it fails the restore is refused.** The
   app is already stopped, so it is consistent; it is incremental, so it costs about
   the delta. An unrecoverable overwrite is worse than a restore that did not happen.
2. **A `.restoring` marker**, written *outside* the folder being replaced — a restore
   that deletes files absent from the backup would otherwise delete the marker too.
   Its name cannot parse as a stamp, so no lister mistakes it for an archive.
3. **The marker gates `EnsureStarted`.** An app whose restore was cut short stays
   down. Starting it would initialise over the gap — fresh database, default config —
   and that invented state would become the next night's backup.

Two knock-on changes:

- `ListBackups` / `ListAll` become a union of local and remote-only entries, deduped
  by `(app, stamp)`, with a tier badge per row. Remote listing is a subprocess call
  and **must be cached** — the global backups page is already the expensive read.
- Install-from-backup still goes through `RestoreBackup`, so it inherits the first
  two paths unchanged. It does **not** get the in-place path, and does not need it:
  a fresh install has no live folder to write over.

### Disaster recovery

The scenario is a user who has lost the box entirely.

1. A fresh PCS is provisioned. Identity returns the normal way — JWT, domain,
   mesh-router registration — none of which depends on the backup.
2. The box boots into **recovery mode**: admin stack and domain up, nothing else.
3. The recovery UI asks for the provider and the master password.
4. It connects, lists snapshots, restores.

> **The recovery form has one field.** The repository location is derivable from
> identity — the fresh box's credentials resolve endpoint, bucket and prefix — so the
> only thing a user must supply is the one secret Yundera never had. Treat that as an
> invariant; it erodes the moment someone proposes asking the user to pick a bucket.

Restoring onto a *different* box needs no new mechanism. Storage is modelled as one
space per user with one key per attached device, so a fresh PCS is simply a new device
attaching to the same space.

**Recovery mode is a state of Maison, not a separate program.** Maison already has the
provider layer, `Materialize`, the app registry, per-app progress events, and the
requirement to tolerate absent configuration by degrading to "not configured".
Recovery mode is that same state plus one question: *there is no config — but is there
a repository?* A separate recovery service would duplicate all of it and, being used
once per user per lifetime, would be the least-tested code in the product at the exact
moment it matters most. Sharing the path with ordinary single-app restore means it is
exercised continuously.

#### Four things that go wrong if they are not designed in

| | |
|---|---|
| **Verify before destroying** | Connect and list snapshots *first*, so a mistyped password is caught before the box has been reprovisioned or wiped. |
| **The scheduler stays off until recovery is explicitly confirmed complete** | A nightly run firing mid-restore snapshots a half-restored PCS, and under keep-7-daily a week of that evicts every good daily. Weeklies and monthlies survive, so it is recoverable — but it must be impossible. Require an explicit action; **inferring completion is how this happens.** |
| **No app starts before its data has landed** | An app brought up against an empty `AppData/<app>` initialises fresh — new database, default config — and then either the restore writes over a running app, or that fresh state becomes the next night's backup. Gate per app: restore → verify → start. |
| **Restore is resumable** | A few hundred gigabytes over a home uplink is days, and the box will reboot inside that window. Persist per-source progress; resume, never restart. |

#### Restore order is a feature

```
1. engine config          near-instant; on a multi-engine box this returns engine #2
2. small apps, one by one dashboard, notes, passwords — a working PCS in minutes
3. user data / media      streams in the background for hours or days
```

Restores are per-source anyway, so ordering costs nothing to implement and changes the
experience from "unusable for three days" to "usable in ten minutes, complete in
three days".

Recovery is a **one-way restore, not a merge.** Restoring an older snapshot and then
letting the nightly run creates a rollback point in the chain — correct under GFS
retention, but a deliberate choice rather than a surprise.

---

## What Maison consumes from the PCS

Maison **never fetches credentials.** A self-check script on the host reads the
storage credentials out of the PCS secret env and renders them into
`AppDataShared/backup/<engine>/repository.config`. Maison reads that file and shells
out to the engine.

This keeps credential fetch, key rotation and suspended-space handling in the host
path that already does that work, and keeps Maison's blast radius at *reads a config
file it does not own*.

Three requirements follow:

- **Tolerate the file being absent.** On a box where the host side has not run, the
  backup UI degrades to "not configured" — it does not error. This is the same state
  recovery mode builds on.
- **Tolerate the file being stale.** A rotated credential means a repository
  re-connect, not a failure.
- **Never branch on who wrote it.** A hand-written `repository.config` pointing at a
  local MinIO or a filesystem repository must exercise every code path — that is how
  this is developed and tested before the host side exists.

A credential may also arrive marked **not writable** (a storage space suspended for
quota abuse). That path must degrade correctly rather than error: **restores and
prunes work, writes fail.** It is worth building from the start, because the graceful
handling lives on this side and retrofitting it during an incident is expensive.

---

## Not yet decided

- **Which SMTP transport** Maison uses.
- **Progress reporting granularity.** Engines emit clean JSON *results* but no clean
  JSON *progress* stream. Either stderr is parsed or the bar is indeterminate for v1 —
  which argues for making upload progress **optional** in the provider interface
  rather than required, so an engine that can only report start and end is still
  shippable.
- **What "custom" means** in the engine picker. *User supplies a command* means Maison
  executes an arbitrary binary as root with app data and storage credentials in
  scope — a deliberate remote-execution surface that deserves an explicit decision.
  *User supplies a repository target for an engine Maison already ships* is ordinary.
  These are very different features wearing one word.
- **Scheduling surface** — how much of the schedule a user can change (time, tiers,
  per-app opt-out).
- **Where recovery mode sits in the boot sequence**, and how a fresh box knows to enter
  it rather than come up empty.

---

## Provenance

Derived from `BACKUP-STORAGE-PLAN.md` at the repo root, which remains the working
document and additionally carries the server-side storage design (per-user space
provisioning, scoped credential issuance, usage metering and reconciliation) that this
document treats as an external contract.
