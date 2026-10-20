# App data migrations

> **Status: implemented in v1.1.58, not yet on the fleet** — see
> [Where it stands](#where-it-stands) for what shipped, what was tested and what is
> left; the [open questions](#open-questions) are deliberately deferred.
> Code: `internal/xcomposeapp/migrations.go` (manifest), `internal/stackup/dataversion.go`
> (marker, start check), `internal/stackup/migrate.go` (runner), `internal/installer/update.go`
> (`PlanData`, the update sequence). The update sequence this extends is the one in
> [`lifecycle.md`](./lifecycle.md#update), authoritative for what Maison does today.

How a store app carries its users' data from one version to the next when an image
bump alone is not enough: a PostgreSQL major upgrade, an app that must boot every
intermediate major (Nextcloud), a config file that moved, a schema the app will not
convert by itself.

Its companions:

- [`lifecycle.md`](./lifecycle.md) — the install and update sequences this plugs into.
- [`x-compose-app.md`](./x-compose-app.md) — the manifest the two new fields live in,
  and the [script entry](./x-compose-app.md#the-script-entry) a migration step is.
- [`app-model.md`](./app-model.md) — the app folder, where the data-version marker
  lives beside `.init/`.

---

## Why

An update today is a byte comparison: the store's compose differs from the one on
disk, so Maison backs the app up, swaps the file and brings the stack up. There
is no notion of *which* version an install is on, so there is nowhere to hang "and
before the new version starts, convert the data".

The cost of not having it is real. On yunderateam (2026-10-07) a store relink put
`odoo:19.0` over an Odoo 18 database: every page answered 500 while the container
reported healthy. Odoo 18 → 19 cannot be done by the image; it needs OpenUpgrade.
Nothing in the store could have said so, and nothing in Maison could have refused.

## The split of responsibility

> **Maison provides the runner, the ordering, the marker and the rollback. What a
> migration does — and whether it is correct — is the app maintainer's.**

This is the same line as everywhere else in the app model: Maison does not grow
mechanisms to compensate for an app's mistakes. A migration is the maintainer's
statement that "data in shape N-1 becomes data in shape N by running this"; Maison
guarantees it runs exactly once, in order, at the right moment, with a way back.

## The model

### Two fields, two jobs

```yaml
x-compose-app:
  version: "2.4.0"        # what the user is told — display only
  data_version: 3         # what the data is converted to — the migration counter
  migrations:
    - id: 2
      ...
    - id: 3
      ...
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | string | — | The app's release as humans read it: shown on the tile, the Update tab (`2.3.1 → 2.4.0`), the store card. **Nothing compares it.** Free-form, because upstream tags are (`2024.10.1-ls45`). |
| `data_version` | int ≥ 1 | `1` | The shape of the app's **data**. Raised by exactly one per migration. An app that has never needed a migration does not declare it. |
| `migrations` | object[] | `[]` | One entry per `data_version` step above 1. See [Migration steps](#migration-steps). |

They are separate because they change for different reasons. Most releases bump
`version` and leave the data alone; a migration is rare and must be totally ordered,
which a free-form upstream tag cannot be. An ordinal never needs parsing, never
collides with a tag scheme, and makes "which migrations are pending" a range check.

`data_version` and `migrations` are read from the **base compose only** — the store's
file. The override cannot declare, skip or reorder a migration: the data's history
belongs to the app, not to a box.

### The marker: where the data *is*

```
/DATA/AppData/<app>/
├── docker-compose.yml        # says where the data is going (data_version)
├── .data-version             # says where the data is — one integer
├── .scripts/                 # the store's scripts/, refreshed on install and update
├── .init/
└── …
```

The marker lives **in the app folder**, for the same reason `.init/` does: it must
travel with the data it describes. A rollback restores the folder, so the marker goes
back with the data. A restore from an archive brings back a compose, a marker and data
that agree. A marker in Maison's own state would be wrong after either.

The compose declares the target; the marker records the present. Neither alone is
enough: the compose on disk is replaced at every update, and a marker without a target
cannot say what is pending.

**Reading the marker** (`from`):

1. `.data-version` exists → its value.
2. Otherwise → the `data_version` of the compose **on disk, before the update
   overwrites it**, which is `1` when that compose does not declare one.

Rule 2 is the baseline for every install that predates this feature — which is all of
them. It is also true: no app has ever had a migration, so every existing install's
data is at version 1.

The marker is written atomically (temp file + rename) after each successful step, so
a failed update's log says exactly which step it reached — though the rollback puts
the earlier value back regardless.

## Migration steps

A step is a [script entry](./x-compose-app.md#the-script-entry), or a refusal.

```yaml
x-compose-app:
  data_version: 4
  migrations:
    # 1. Inline — the common case: one tool, in a pinned image.
    - id: 2
      name: { en_us: pg-14-to-16 }
      images: [pgautoupgrade/pgautoupgrade:16-alpine]   # pulled before the stop
      script: |
        docker run --rm -e PGAUTO_ONESHOT=yes \
          -v "$APP_DIR/db:/var/lib/postgresql/data" pgautoupgrade/pgautoupgrade:16-alpine

    # 2. A file — for a migration that is genuinely a sequence.
    - id: 3
      name: { en_us: move-config }
      file: scripts/0003-move-config.sh
      images: [nextcloud:28.0.14-apache, nextcloud:29.0.16-apache]

    # 3. A refusal — "this cannot be automated".
    - id: 4
      blocked:
        en_us: |
          Odoo 18 → 19 changes the database schema and needs OpenUpgrade.
          Follow https://… on a copy, then reinstall from that copy.
        fr_fr: |
          Odoo 18 → 19 change le schéma de la base et nécessite OpenUpgrade.
```

| Key | Form | Meaning |
|---|---|---|
| `id` | all | **Required.** The `data_version` this step produces. Steps are `2 … data_version`, each present exactly once. |
| `name` | all | [Localized](./x-compose-app.md#localized-strings). Shown in the update progress (operator's locale) and the logs (`en_us`). |
| `script` / `file` | script entry | Inline bash, or a `.sh` under the store app's `scripts/` folder — nothing else travels with an update. The folder is copied to `.scripts/` from the same store sync as the compose. |
| `images` | script entry | Images the step runs, pulled with the app's own images **while the old version still serves**. An unlisted image is pulled when the step runs — after the stop, so on the outage's clock. |
| `timeout` | script entry | Go duration. Default `2h` — a dump/restore of a large database is legitimately long, a hang is not. |
| `blocked` | refusal | [Localized](./x-compose-app.md#localized-strings) Markdown shown to the operator in their locale. An update that would cross this step is **refused before anything is pulled.** |

These rules are checked on the store's compose, at install and when an update is planned.
The compose already installed is read for its `data_version` only: one installed under
older rules (a plain-string `name`, say) still starts, and still updates to the store's
current file.

A step runs exactly like a [step](./x-compose-app.md#steps) or a credential change:
`bash -euo pipefail` inside the Maison container, working directory the app folder,
the host Docker daemon, `compose`, the sanctioned command set, `$APP_DIR` as the
host spells it. It additionally gets `MIGRATION_ID`, `DATA_VERSION_FROM` (the marker
when the update started) and `DATA_VERSION_TO` (the compose's target). What it
writes to `$MAISON_OUTPUT` reaches the setup steps, seed tree and `files` of the
same up; what it writes to `$MAISON_ENV` is added to the `.env` where missing —
how a migration gives an existing install a key a fresh install gets another way.

**Exit 0 means applied. Anything else means the update failed.** There is no
"skipped" and no "try again next time": a step that has nothing to do (the state is
already converted) checks that and exits 0. That is the whole idempotence contract,
and it is the one the template's `run-migrations.sh` got wrong — a "defer to next
cycle" exit 0 recorded the migration as done and the fleet carries the false markers.

**Write steps that check state.** The counter says which steps to run; the step
should still confirm the data is in the shape it expects — read `PG_VERSION` before
running `pg_upgrade`. On an install whose marker came from the baseline rule, that
check is what keeps a step safe.

### Intermediate versions belong to the step, not to the store

The store serves only its current tip, so a user on data version 2 updating to 6 gets
steps 3, 4, 5, 6 in a row with only the version-6 compose on hand. A migration that
needs an intermediate *app* running brings it itself, as a pinned image in its own
step. Maison does not replay historical composes: fetching them by commit depends on
store history, their assets may be gone, and every replayed update would need its own
backup.

## When each step runs

This is the table to read before choosing between a step and a migration.

| Step | Fresh install | Update that raises `data_version` | Update that doesn't | Start / config save | Restart | Rollback / restore |
|---|---|---|---|---|---|---|
| migrations | **never** — the marker is set to the target | **each pending step, once, in order** | — | never — refuses if any are pending | never | never |
| a step, `run: once` (and an `x-casaos` `pre-install-cmd`) | yes | yes, for a step this version adds | yes, for a step this version adds | yes, if its marker is missing | no | no — its marker is restored |
| a step, `run: always` | yes | yes | yes | yes | no | yes |

A fresh install **never replays history**. It runs the current steps against an
empty folder and writes `.data-version` equal to the compose's `data_version`.

**Step or migration?** A step is safe to run against any install at any time —
idempotent, no backup needed. A migration happens exactly once at a version
boundary, transforms data, and has a backup behind it.

### The maintainer's invariant

> **A fresh install of data version N must produce the same state as an install of
> any earlier version followed by every migration up to N.**

The declarations (`folders`, the seed tree, `files`) and the `once` steps build the version-N
workspace from nothing. Migration N turns the version-(N-1) workspace into the
version-N one. They are two routes to the same place.

The consequence that matters: **a change to what a create-if-absent declaration
(the seed tree, `ensure: once` files) or an existing `once` step produces on disk is
a change that existing installs will never see** — unless a migration, or a new step,
carries it. Before this feature that gap was silent. Now it has a name, and it is what
to check when reviewing a store change that touches steps, `files` or the seed tree.

It is also checkable without a human: install the store's previous commit of the app,
update to the new one, and compare with a fresh install of the new one. That is a
natural Touchstone audit.

## The update sequence, extended

Changes against [`lifecycle.md`](./lifecycle.md#update) are marked `◆`.

```
1. fetch the store's current compose for the app store-ref names
2. equal to what's on disk, byte for byte? → nothing to do
◆  plan: from = marker (or the on-disk compose's data_version), to = new compose's
◆     to < from              → REFUSE  "the data is newer than this version"
◆     a step in from+1..to missing or malformed → REFUSE  (store authoring error)
◆     a step in from+1..to blocked → REFUSE with its message, nothing pulled
◆     steps pending and NoBackup → REFUSE  (see below)
   can an enabled destination take the backup? no → REFUSE
3. pull the new version's images ◆ and every pending step's image(s)
4. back up to every enabled destination
5. stop the old version
6. overwrite docker-compose.yml
7. refresh .seed ◆ and .scripts from the same store sync
8. stackup.Up in update mode:
     folders → secrets → variables
◆    → migrations from+1 … to, in order; .data-version written after each
     → setup steps → seed → files → compose up → started steps
9. any failure → restore the backup over the app (compose, data and marker together)
```

**Why after `variables`**: a migration may need what the new version introduces — a
new database password generated by `secrets`, a new folder from `folders`. Both are
idempotent and additive, so running them first costs nothing on the old data.

**Why before the setup steps, the seed tree and `files`**: those write create-if-absent defaults
for the *new* layout. Run first, they would put a fresh default config at the path a
migration is about to move the user's real config into — and the migration would then
find its target taken, or worse, `files` would have rendered over it.

**Why the plan is checked at step 2**: every refusal there costs nothing — no pull, no
backup, no stop. A blocked step discovered after the stop would cost the user an
outage for a refusal.

**No migration without a backup.** *Update without backup* exists so a large
app is not pinned on an old version; with migrations pending it would make an
irreversible data conversion with no undo. The update is refused, and the refusal says
so. This is the one place `NoBackup` is not honoured — and the same refusal applies to an
app with `backup.skip` or a box with no backup destination enabled.

**An app that is not stoppable** (`lifecycle.stoppable: false`) is not stopped at step 5,
so its migrations run against a live old version. Its maintainer writes them for that.

**Time.** An ordinary update gets five minutes from the stop onward (the backup before
it is not counted); one with pending steps gets that plus every step's `timeout`. The
Update tab hands every update to a one-app background run (`202 {"started": true}`), so
it outlives the browser tab, and the tile's progress bar
names the step (`Migrating data (2/3): pg-14-to-16`).

**Update all** is unchanged — it is already strictly sequential, one backup at a time. A blocked app appears on Settings → Updates as *blocked*, with its message,
rather than as *update available*; "Update all" leaves it out.

## Start refuses pending migrations

Only an update takes a backup to undo them with, so only an update runs migrations. Every other
path into `stackup.Up` — Start, a config save, an added domain — checks the marker
against the compose first:

- marker < compose `data_version` → **refuse to start**, raise `app.<app>` pointing at
  the Update tab.
- marker > compose `data_version` → **refuse to start**: the data was written by a newer
  version than this compose. Starting old code on it is the Odoo incident in reverse.

Through Maison's own flows this never fires: install writes the marker, update writes
it, and a restore brings back a marker and a compose that agree. It fires when the
folder was edited by hand, or a compose was swapped outside Maison — exactly the cases
where starting anyway would run code against data it was not written for.

With no `.data-version` file the check cannot fail (the baseline is the compose on
disk), so the whole existing fleet starts exactly as before.

## The store side

### Layout

```
Apps/<App>/
├── docker-compose.yml       # x-compose-app.data_version / migrations
├── scripts/                 # files a script entry names with `file:`
│   └── 0003-move-config.sh
└── …
```

`scripts/` arrives with the same store sync as the compose (the store archive), so
a compose and the files it names can never be from different commits. It is shared
by every script entry — steps and credential changes name their files there too.

The file name is not significant — the compose names each script — but
`<id, zero-padded>-<what>.sh` keeps the folder readable in order.

### The sweep

A first, mechanical pass adds `version` to every store app, read from the main
service's image tag and corrected by hand where that tag is not a release. It does not
add `data_version`: absent means `1`, and only an app that gains its first migration
writes `data_version: 2`. Keeping the field off the other ~220 files keeps every later
diff that touches it meaningful.

## Rollout order

`version`, `data_version` and `migrations` are additive: a Maison that predates them
**ignores them**. For `version` that is harmless. For `migrations` it is not — an old
Maison would update straight past the step, putting the new image on unconverted data,
and write no marker either.

So the order is fixed, the same shape as the store's switch to relative asset paths:

1. Ship a Maison that understands migrations, and let it reach the fleet.
2. Sweep `version` into the store (harmless on any Maison).
3. Only then merge the first store app that declares a migration.

Should an old Maison ever need to be locked out by the manifest itself, the lever would
be a `requires_maison:` field that refuses the update — not raising `schema_version`,
which [degrades rather than gates](./x-compose-app.md#fields). Rollout order should make
it unnecessary.

## Implementation sketch

- `internal/xcomposeapp` — parse `version`, `data_version`, `migrations`; validate the
  step forms and contiguity of ids. Base-only, like the store-owned keys.
- `internal/installer/update.go` — compute the plan before the room check; extend the
  image pull; copy `scripts/` to `.scripts/` beside `writeSeed`; refuse `NoBackup`
  with steps pending.
- `internal/stackup` — an update mode on `Up` (an option, not a second function: the
  one-rule "every up goes through `stackup.Up`" stays true) that runs the pending steps
  between `EnsureVariables` and the setup steps; the marker check for every other mode.
  A step is run by `RunScript`, the runner every script entry shares.
- `internal/installer` install path — write `.data-version` before the first up.
  Over a restored folder the data already has a version (its marker, else the restored
  compose's); an install runs no migrations, so one that disagrees with the store's
  compose is refused rather than started on data of another shape.
- Update progress — a `migrate` phase naming the step (`Migrating data (2/3):
  pg-14-to-16`), with the step's output in the run log.
- Incidents — a blocked update raises `app.update:<app>` as a warning carrying the
  maintainer's message; a failed step reports like any failed update.

## Where it stands

*As of 2026-10-13.*

### Shipped

| Release | Commit | What |
|---|---|---|
| v1.1.56 | `63e067c` | The manifest (`data_version`, `migrations`, validated, base compose only; `version` display-only), the `.data-version` marker and its baseline rule, install writing it, every start refusing a marker that disagrees with the compose, install-from-backup refusing data of another version, and an update **refusing any pending step** (no runner yet). |
| v1.1.58 | `9e321be` | The runner: container and script steps between `variables` and `init`, marker after each step, per-step timeout, rollback on failure; `data_no_backup` refusal; step images pulled with the app's; `.migrate/migrations/` refreshed per update; a `migrate` progress phase; the `blocked` update state, left out of "Update all"; the Update tab running a migrating update as a background one-app run with the steps' time budget; UI for all of it (en/fr). |

v1.1.57 (`6e6c049`, backup-engine approval) is unrelated and sits between the two.

**Unreleased (2026-10-20):** a step became a [script entry](./x-compose-app.md#the-script-entry),
shared with steps and credential changes. The container form (`image`, `command`,
`volumes`, …) is gone — it is a `docker run` in `script`, with the image under
`images`; `script:` is now inline bash and a file is `file: scripts/…`; the store's
`migrations/` folder became `scripts/`, copied to `.scripts/` on install and update
(`.migrate/` is removed). The history below describes the released forms.

The store side (Yundera/AppStore): `beef35f` swept an optional `version` into 79 apps,
and `6bbd981` documents `version` (optional, display-only) and `data_version` /
`migrations` in CONTRIBUTING.md. No store app declares a migration yet.

### Tested on a box

holyhorse, 2026-10-13, with a throwaway store app (`migtest`, Postgres) served from an
nginx container on the box and Maison built from the working tree:

| Case | Result |
|---|---|
| install v1 (`postgres:14`) | marker written as 1 |
| v2 (`postgres:16` + `pgautoupgrade` container step) with *Update without backup* | refused `data_no_backup` before anything ran |
| v2, normal update | background run; phases pull → backup → stop → `Migrating data (1/1): pg-14-to-16` (30 s) → start; `PG_VERSION` 16, marker 2, rows intact |
| v3 (+ a script step that exits 1) | step ran and failed; rolled back to v2 — compose, data (3 rows) and marker 2 |
| v4 (+ a `blocked` step) | check reports *blocked* with the message; apply returns `409 data_blocked` before any pull; "Update all" skips it |

The test found one bug, fixed before the release: the store's `migrations/` folder was
copied flat into `.migrate/`, so a step's `script: migrations/…` did not resolve. It is
now copied to `.migrate/migrations/`, and a script path must start with `migrations/`.

Not tested: the UI in a browser (Update tab wording, the *blocked* row on Settings →
Updates, the tile's "Migrating data…" label) — only its API.

### Remaining

1. **Rollout.** The template's `main` channel pins maison **1.1.57** (staging: phase 1,
   refusals only) and `stable` pins **1.1.55** (no migration support at all). Pin
   **1.1.58** in `mesh-router-template-root/stacks/maison/docker-compose.yml` on `main`,
   let it run on the staging boxes, then promote to `stable`; bump the `maison` and
   template pointers in `mesh-router-root`, then `yundera-root`.
2. **No store migration before step 1 reaches the fleet.** A Maison older than v1.1.56
   ignores the fields and updates straight past a step onto unconverted data.
3. **Odoo 18 → 19** is the first real one: a `blocked` step with the OpenUpgrade
   instructions. Check the installed Odoo versions first — an install that predates
   the marker is read as data version 1 from its compose, so one already on 19.0 would
   be refused as blocked although its data is fine. yunderateam is pinned to 18.0 and
   unlinked from the store.
4. **A browser pass** over the UI above.
5. **holyhorse cleanup**: the `migtest` app, the `migtest-store` container and
   `/DATA/AppData/.migtest-store/`. The nightly self-check restores the pinned Maison
   image but not these.
6. **de/zh strings** for the new UI text (they fall back to English).

## Open questions

- **Disk room.** `pg_upgrade` in copy mode, or a dump/restore, needs space beyond the
  app's data, on the box's own disk whatever the backup destinations are. A per-step `space:` hint (a multiple of a folder's size) could join
  the existing room check. Not designed until a real migration needs it.
- **Getting past `blocked`.** As written, a blocked app stays on its version until the
  maintainer replaces the step with a real one, while fresh installs get the new
  version. Whether an operator may declare "I did it by hand" — set the marker from the
  UI — is open; it is a promise Maison cannot verify.
- **Post-start steps.** Some conversions need the new version running
  (`occ db:add-missing-indices`). An `at: started` on a migration step, as steps have, would
  cover it; left out until an app needs it, and its failure semantics (the stack is up —
  roll back or not?) need deciding then.
- **"Healthy but broken".** `installer.Steady` looks at container restarts, which the
  Odoo incident passed. A declared health URL checked after the up would catch a
  migration that ran green and left the app serving 500s. Useful beyond migrations.
