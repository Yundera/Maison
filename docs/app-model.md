# App storage & lifecycle model

How Maison lays apps out on disk and derives their dashboard state. This model
**diverges from CasaOS** on purpose: the on-disk folder is the single source of
truth for *what apps exist*, and the live Docker state is the single source of
truth for *how each one is doing*. Nothing about an app is kept in a database or a
registry file — the filesystem and the Docker daemon **are** the state.

> This document is authoritative for the app layout and supersedes the older
> `AppData/casaos/apps/<app>` nesting described elsewhere. Maison uses a **flat**
> `AppData/<app>` layout.

For what Maison *does* to this layout — the install / start / update / save /
uninstall sequences, and the `folders` and `hooks` that hang off them — see
[`lifecycle.md`](./lifecycle.md).

---

## On-disk layout

Every app is one directory directly under the data root:

```
/DATA/AppData/<app>/
├── docker-compose.yml            # strict copy from the store — never modified
├── docker-compose.override.yml   # user edits from the config window (Compose override)
├── .env                          # prefilled by Maison on create, then user-editable
└── …                             # any other files the app needs (configs, seed data, …)
```

`<app>` is the Compose project name and the tile identity. The directory name is
what the dashboard shows.

### File roles

| File | Origin | Mutated by | Purpose |
|------|--------|-----------|---------|
| `docker-compose.yml` | **Strict copy from the store listing.** | Never — Maison treats it as read-only. | The pristine app definition. Keeping it byte-for-byte identical to the store is what lets updates stay clean (re-copy on update, overrides survive). |
| `docker-compose.override.yml` | Generated from the **per-app config window** (ports, env, volumes, …). | Maison, on every config save — and on every up, for the routes it generates (see [`domains.md`](./domains.md)). | User customizations, layered on top via Compose override semantics. The running stack = `docker-compose.yml` + `docker-compose.override.yml`. |
| `.env` | **Prefilled by Maison on create** (PUID/PGID, TZ, `REF_*`, domain, generated secrets, …), then hand-editable. | Maison on create; user thereafter. | Variable substitution for both compose files. |
| everything else | The app (bind-mount targets, config files, databases, …). | The app at runtime. | User data. **Never** deleted by Maison (see uninstall). |

The stack is always brought up from this directory as
`docker compose -f docker-compose.yml -f docker-compose.override.yml … up`, with
`.env` resolved from the same folder — so what runs is exactly what is on disk.

### Update reference (in the override's `x-compose-app`)

On install, Maison records **where the app came from** so it can later pull a
fresher `docker-compose.yml`. The reference lives in the override's
`x-compose-app` block (so it survives base re-copies, and the strict base stays
byte-identical to the store):

```yaml
# docker-compose.override.yml
x-compose-app:
  store: https://github.com/Yundera/AppStore/archive/refs/heads/main.zip  # reference store
  store-app-id: jellyfin                                                  # catalog id within it
  store-apps-path: catalog/apps        # apps folder, only when it is not the default Apps/
```

`store-apps-path` is written only when the app came from a store that keeps its apps
somewhere other than `Apps/` — an app installed before the field existed reads back with
the default, which is what it was installed from. Together the three fields are the store
reference the store panel uses (`internal/appstore/ref.go`), so an update resolves to the
same store *and* the same folder the install came from.

The same block carries the manifest of the **Caddy routes Maison generates** to
publish the app on the deployment's additional domains (`generated-routes`) — the
record of which label keys are Maison's to rewrite, so regenerating them can
never touch the operator's. See [`docs/domains.md`](./domains.md).

The per-app **Update** tab uses this to:

1. fetch the store's current listing for `store-app-id` from `store` (in
   `store-apps-path`, when set), and
2. compare it byte-for-byte with the installed `docker-compose.yml`.

The comparison is that simple only because the base is the store's bytes and nothing
else. Maison once transformed the store's file on the way in, so both sides of this
comparison had to be transformed identically for it to mean anything.

When they differ, **Update now** overwrites the strict base with the store's
version and runs `docker compose up -d` (base + override). The override and
`.env` are never touched. Apps with no recorded reference (installed before this
feature, or unmanaged stacks) simply report "no update reference".

### Editing the override — form and YAML

The settings window splits the two compose files by what you can *do* to them:

| Tab | View | What it is |
|---|---|---|
| **Override** | **Form** | Field-by-field editor (image, restart, ports, volumes, environment; devices, cap_add, command, privileged, limits under *Advanced*). Each field shows the store's value as a ghost placeholder and is marked when overridden; clearing a field resets it to the store's. |
| **Override** | **YAML** | The `docker-compose.override.yml` itself. Anything the form can't express belongs here. |
| **Compose** | **Store** | The strict `docker-compose.yml` as shipped. Read-only — Maison never modifies it. |
| **Compose** | **Effective** | `docker compose config` over base + override: the merged, interpolated project. Read-only; this is what actually runs, and the view that answers "what did my override actually *do*" — which of the store's values survived, and which yours replaced. |

Form and YAML are two views of the same file and either can save it. The form
**patches the override's YAML node tree** rather than regenerating it, so
comments, key order, and keys it doesn't model (`x-compose-app`, `healthcheck`,
`depends_on`, …) survive a save untouched.

**Compose's merge rules are not uniform, and the form speaks them:**

- **Scalars** (`image`, `restart`, …) — the override replaces the base.
- **Sequences** (`ports`, `volumes`, `devices`, `cap_add`) — the override is
  **appended** to the base, not substituted for it. A form that merely *listed* the
  ports you want would therefore keep publishing the store's as well. So: when the
  form's list only adds to the store's, it writes just the extras; when it **edits
  or removes** one of the store's, it writes the whole list under Compose's
  `!override` tag, which replaces the base's outright.
- **Mappings** (`environment`) — merged key by key. The form writes only the
  variables that differ from the store's. **Removing** one of the store's variables
  can't be expressed by a key merge, so that too falls back to `!override`.

`!override` requires Docker Compose **v2.24.4+**.

A construct the form can't represent faithfully — a long-syntax port, a list-form
`command`, a node tagged by hand — is shown read-only ("edit in the YAML view") and
is **never rewritten** by a form save. Whether a field is editable is recomputed
from the files on every save, never trusted from the client.

Every save, from either view, is **validated first** (`docker compose config` over
base + candidate). An override Compose won't parse is rejected before it is written,
so a typo can't leave an app that no longer comes up.

---

## State model — the folder and Docker together

Maison never invents state. A tile's existence comes from the **folder**; a
tile's appearance comes from **Docker**.

### 1. Existence — driven by the folder

```
folder present at /DATA/AppData/<app>   ≡   an app tile in Maison
no folder                               ≡   no tile
```

Create a folder (even by hand) → the app appears. Remove/rename it → the app
disappears. There is nothing else to register.

### 2. Appearance — driven by the live Docker state

For each existing app, the tile reflects what Docker reports for that project:

| Docker state | Tile appearance | Interaction |
|---|---|---|
| **No live stack** (folder exists, stack not started / fully down) | **Greyed** icon | Burger menu available (Start, Settings, Uninstall, …). Not clickable to "open". |
| **Operation in progress** (up / down / restart / pull mid-flight) | Greyed icon with a **`…` overlay** | **No burger menu** while the operation runs — the tile is busy. |
| **Install / uninstall in progress** | Greyed icon with **one progress bar**, coloured by the step running: blue (downloading), green (starting), red (uninstalling) | Same as busy: no burger menu, not clickable. The `…` overlay gives way to the bar. |
| **Live stack** (running) | **Full-colour, clickable** icon | Click opens the web UI; burger menu available. |

### 3. Health dot — driven by the Docker health check

A small dot in the **top-left** of the tile reflects the container health check:

| Dot | Meaning |
|---|---|
| 🟢 **Green** | Health check passing. |
| 🟠 **Orange** | Health check failing / unhealthy (or still `starting`). |

The dot only appears when Docker reports a health check for the stack; a stack
with no health check has no dot.

### 4. Which grid — driven by the app's `view`

Existence and appearance say *whether* a tile is drawn; the app's `x-compose-app`
`view` says *where*. `system` puts it in the dashboard's System grid and protects
it (no stop, no uninstall, no scheduled backup); `hidden` draws no tile at all;
anything else — including every app that says nothing — lands in the ordinary
grid. [`x-compose-app.md`](./x-compose-app.md) is authoritative for it.

Note the asymmetry with the rule above: a hidden app still *exists*. Its folder is
there, it runs, and Maison manages it — it simply has no tile. Only a `.` in the
name takes an app out of the model entirely (see below).

---

## Backups live in `.backups/`

Every archive of every app sits under one directory, one sub-directory per app:

```
/DATA/AppData/<app>/                          the live app
/DATA/AppData/.backups/<app>/<stamp>/         a folder archive
/DATA/AppData/.backups/<app>/<stamp>.zip      a compressed archive
/DATA/AppData/.backups/<app>/.staging-<stamp>/ a snapshot still being taken
```

`<stamp>` is `YYYY-MM-DD_HHMMSS` — seconds included, so the name is the whole
identity and two archives of one app can never collide. Anything in that directory
that does not parse as a stamp (a staging folder, a `.partial` zip, a stray file)
is **not an archive**: it is never listed and never restorable, which is what makes
an interrupted backup inert rather than dangerous.

Two properties of the location are load-bearing:

- **The leading dot hides it.** `.backups` contains a `.`, so the existing "dot in a
  name = hidden" rule (below) keeps it off the dashboard with no special case.
- **It is inside `AppData`, so archiving is a rename.** Same filesystem means
  `os.Rename`, which is instantaneous no matter how much data the folder holds.
  Moving `.backups` to another volume would silently turn every uninstall into a
  full copy.

An archive carries the **whole app folder** — `docker-compose.yml`, the override,
`.env` and the data — so it restores into a working app on its own, without the
store.

There is deliberately **no distinction between an archive made by an uninstall and
one made on demand.** A backup is a backup, whatever created it; both are listed,
restored and deleted the same way.

---

## Uninstall = archive (never delete)

Maison **never removes user data.** Uninstalling an app **moves** its folder into
the backups tree:

```
/DATA/AppData/<app>
      ↓ uninstall — local engine
/DATA/AppData/.backups/<app>/2026-07-10_153045
      ↓ uninstall — remote engine
a snapshot in the repository; nothing is left on this disk
```

- **An uninstall is a backup through the default engine**, and where it lands follows
  that setting like any other backup (`backup.md` §Uninstalling an app). The stack is
  stopped, backed up, and only then are the containers and the folder removed.
- On the **local** engine the backup is a rename of the directory, exactly as above —
  the bytes on disk are untouched and only the path changes, which is what keeps an
  uninstall instant at any size.
- On a **remote** engine the folder is uploaded and then deleted, so an uninstalled
  app's data survives losing this disk. Nothing is left in `.backups/`.
- **Zip is an option, and a local-engine one.** When enabled, the folder is compressed
  to `<stamp>.zip` instead of a plain move. Default is a plain move (fast, no copy). A
  remote engine ignores it — a zip defeats deduplication — and the dialog hides it.
- **It runs detached, with progress on the tile.** Confirming the dialog only
  *starts* the uninstall; the dialog closes immediately and the tile carries the
  same progress bar an install shows, in red (see `lifecycle.md`). An upload, or a
  zipped archive of a large app, runs for minutes and nothing waits on it.
- The app vanishes from the grid — its folder is gone — but its data is one
  restore away, from the store's install dialog or from Settings → Backups.

---

## Backup = archive without uninstalling

The same archive, made while the app stays installed. It is the only operation that
must stop a running app, and it is built to stop it for as little time as possible:

```
copy      app up      full mirror into .staging-<stamp>   (no downtime)
stop      ─────────────────────────────────────────────── downtime starts
sync      app down    only what changed during the copy
start     ─────────────────────────────────────────────── downtime ends
compress  app up      zip the snapshot, then delete it    (zip only)
```

Downtime is proportional to what the app **wrote during the first pass**, not to how
big it is — a 40 GB app whose data is mostly at rest is down for seconds. The result
is consistent either way: every byte in the snapshot was read either before the app
touched it again, or while the app was down.

A backup while the app is *already* stopped skips the stop and the restart entirely.

Two guards, because a backup is the one feature that can hurt the box it protects:

- **Free space is checked before any bytes move** — the app folder's measured size
  plus headroom (×1.1 for a folder archive, ×2 for a zip, which needs the snapshot
  and the zip on disk at once). Filling the data disk does not just fail the backup;
  every other app starts failing writes.
- **The restart is deferred**, so a failure anywhere after the stop still brings the
  app back up. An app left down is worse than a missing backup.

---

## Restore = the swap

Restoring over a live app **archives what is there first**:

```
1. stop the app (if it was running)
2. rename AppData/<app>  →  .backups/<app>/<now>      ← instant, costs nothing
3. put the chosen archive back as AppData/<app>
4. start the app (if it was running)
```

Step 2 is what makes a mis-clicked restore undoable, and it is free: a rename inside
the same tree. It also balances the books — a *folder* archive is consumed by step 3
(renamed back), and step 2 has just created one in its place, so the archive count
does not drop. A *zip* is extracted rather than moved, so it survives and can be
restored again.

Restoring an app with no live folder — an uninstalled one, reached from
Settings → Backups — is the same path with steps 1, 2 and 4 having nothing to do.
Once the folder lands, the app has a tile again.

> **Scope.** Archives under `.backups/` are on the same disk as the apps. They cover
> a bad update, a broken config or a regretted uninstall — **not** a failed disk.
> On their own they are a rollback mechanism, not disaster recovery.
>
> Surviving the loss of the disk needs a **remote engine** — the engine is a setting on
> Settings → Backups — which sends apps *and* the user-data set to a repository off the
> box. Both tiers
> coexist: a backup is listed once, with a badge saying where it actually is, and a
> restore comes from whichever tier holds it — preferring the local one, because
> that restore is a rename. See [`backup.md`](./backup.md).

---

## Install from backup = uninstall, inverted

Archives are not write-only: the store reads them back. Clicking **Install** on a
store app first asks the server for that app's archives
(`GET /api/store/{id}/backups`). With none — the common case — it installs
straight away, one click as before. With some, it offers them:

```
┌──────────────────────────────────┐
│ ▸ Fresh install                  │
│                                  │
│ RESTORE FROM BACKUP              │
│ ▸ 2026-07-10 15:30       folder  │
│ ▸ 2026-06-02 09:00 zip · 78.7 MB │
└──────────────────────────────────┘
```

Picking one posts `{"from_backup": "<archive name>"}` to the install endpoint,
and the install becomes:

1. **restore** the archive as `AppData/<app>/` — a folder archive is *renamed*
   back (no copy, and the archive is consumed); a zip is *extracted* (a copy, so
   the zip survives and can be restored again),
2. then run the **ordinary install** on top of it.

Nothing about step 2 is special-cased, because the install is already
non-destructive over what it finds: it overwrites `docker-compose.yml` with the
store's current version (the strict base is meant to be replaceable) but
**never clobbers an existing `.env`**, and never touches app data. So the app
comes back with its old data and its old variables, on a fresh app definition.

The project name is resolved **server-side** (`Installer.ProjectFor`): a store id
is `Dufs`, but its compose project — and therefore its backup directory — is
`dufs`. The client cannot derive this, since the project name may come from the
compose file's own `name:`.

This path **refuses to overwrite a live app** (`apps.RestoreBackup` errors when
`AppData/<app>/` is already present), because an install-from-backup is meant to
land on a clean slate. To put an archive back over an app that is still installed,
use the restore in the app's **Backups** tab — which does the swap above, archiving
the current state on the way. Same archives, two entry points, and neither can
destroy the data it is about to replace.

---

## Dot in a name = hidden

`.` is a **reserved character** for Maison. Any entry under `AppData/` whose name
contains a `.` is **not displayed** as an app:

```
AppData/jellyfin        → shown  (tile "jellyfin")
AppData/.backups        → hidden (every archive of every app lives in here)
AppData/.tmp-download   → hidden (scratch / hidden dir)
```

This single rule does double duty:

- It keeps **archives** (which always carry a date-dotted suffix) out of the grid.
- It gives Maison a namespace for **scratch / internal** folders — anything it
  doesn't want to surface, it names with a `.`.

An app that needs to be visible therefore **must not** have a `.` in its directory
name.

---

## Summary

| Concern | Source of truth |
|---|---|
| Which apps exist | Presence of `AppData/<app>/` (dot-free name) |
| App definition | `docker-compose.yml` (strict store copy) + `docker-compose.override.yml` (user edits) |
| Variables | `.env` (prefilled on create) |
| Running / stopped / busy / clickable | Live Docker state |
| Health dot | Docker health check |
| Which grid (app / system / none) | The app's `x-compose-app` `view` |
| Uninstall | Move to `.backups/<app>/<stamp>` (optionally `.zip`) — data never deleted |
| Backup | Two-pass copy into `.backups/<app>/<stamp>`; the app is down only for the delta pass |
| Restore | Archive the current folder, then put the chosen one back — always reversible |
| Install from backup | Restore an archive as `AppData/<app>/`, then install over it (keeps its `.env` + data) |
| Where backups live | `AppData/.backups/<app>/<YYYY-MM-DD_HHMMSS>[.zip]` |
| Hidden entries | Any name containing `.` |
</content>
</invoke>
