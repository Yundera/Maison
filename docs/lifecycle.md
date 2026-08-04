# App lifecycle

What Maison actually *does* to an app — install, start, update, save, uninstall —
and in what order. This document is authoritative for the sequences and their
failure semantics.

Its two companions:

- [`app-model.md`](./app-model.md) — where an app **lives** on disk and how its tile
  state is derived. Read that first; this document is what happens *to* that layout.
- [`x-compose-app.md`](./x-compose-app.md) — the **declaration** of `folders` and
  `hooks`. This document is what Maison does with them.

---

## The one rule

> **Every `docker compose up` Maison runs goes through `internal/stackup`.**

There is no other place in the codebase that starts an app's stack. This is the
whole reason the package exists: "create this folder before the stack comes up" has
to be true when the app is installed, when it is started from the tile a month
later, when a store update recreates it, and when the operator saves a config
change. Five call sites, one guarantee.

```
                       ┌─────────────────────────────────────────┐
  install ─────────────┤                                         │
  start (tile)  ───────┤          stackup.Up(project, files)     │
  store update ────────┤                                         │
  save config  ────────┤   ensure folders                        │
  save web-UI  ────────┤     → pre_up                            │
                       │       → docker compose up -d            │
                       │         → post_up                       │
                       └─────────────────────────────────────────┘
```

**If you add a sixth thing that starts a stack, route it through `stackup.Up`.**
Calling `composecmd.Up` directly means the app starts without its directories, and
the bug will only show up on someone's second boot.

---

## The up sequence

`stackup.Up` is the primitive. It is **idempotent** — it starts a stopped stack,
recreates a removed one, and re-applies a changed compose, all with the same call.

| Step | What happens | On failure |
|---|---|---|
| **1. Resolve the spec** | Read `x-compose-app` (`folders`, `hooks`) from base + override, with `x-casaos` `pre-install-cmd` / `post-install-cmd` as the fallback for the install hooks. Later files win, key by key. | — |
| **2. Ensure folders** | Create every folder declared under `folders`; apply user/group/mode; walk the tree when `recursive`. Declared folders are the *only* directories Maison creates — it never infers them from `volumes:`. | **Fatal** — a declared folder is the author's contract. |
| **3. `pre_up`** | Run the hook. | **Fatal** — a precondition that doesn't hold must not start the stack. |
| **4. `docker compose up -d`** | Base + override, with the app's `.env` and interpolation variables. | **Fatal.** |
| **5. `post_up`** | Run the hook. | **Logged and swallowed** — the stack is already running; tearing a healthy app back down over a failed after-the-fact tweak is worse than the failed tweak. |

The asymmetry in 3 vs 5 is deliberate and worth internalising: **pre-hooks gate,
post-hooks garnish.** Anything flaky in a `pre_up` blocks the app on *every* start.

---

## Install

`Installer.Install` — the only operation that is not just "an up". It runs the
install-only hooks around the ordinary up sequence, because it is the only caller
that knows the app is being installed for the *first* time.

```
 1. fetch the app's compose from the store
 2. transform it            (PCS rewrites: /DATA → host path, attach REF_NET)
 3. restore backup          (only when installing from an archive — see app-model.md)
 4. write docker-compose.yml   (strict base — overwritten on every install/update)
 5. write .env                 (prefilled, and NEVER clobbered if one already exists)
 6. write the update reference into the override's x-compose-app
 7. ensure folders             ← early, because pre_install seeds files into them
 8. pull images                (Download progress bar, 0 → 100)
 9. pre_install hook           ← fatal on failure
10. ┌ stackup.Up ─────────────────────────────────────┐
    │ ensure folders (again, idempotent)              │   (Start progress bar)
    │ pre_up → compose up -d → post_up                │
    └─────────────────────────────────────────────────┘
11. post_install hook          ← logged, not fatal
12. await readiness            (poll Docker until running + healthy, ~30s)
```

Folders are ensured **twice** — at step 7 and again inside step 10. That is not
redundancy to clean up: step 7 is what makes them exist before the `pre_install`
hook and the image pull, and step 10 is what makes them exist for every *later*
start, when there is no installer in the picture at all.

Steps 4–5 are the non-destructive contract that makes "install from backup" work
without special-casing: the strict base is meant to be replaceable, an existing
`.env` is meant to be kept, and app data is never touched.

### Progress

The install emits `Event`s on two independent tracks, which the UI shows one at a
time on a **single bar**: **Download** (image pull, real per-layer progress, blue)
and then **Start** (bringing the stack up, driven by Docker's live running/healthy
fractions rather than a guess, green). The bar's colour is what says which step is
running — see `appProgress()` in `web/src/lib/stores/apps.ts`, the one place that
turns these fields into a bar, for both the tile and the store's install pill.
Progress rides the live app list, so the tile keeps advancing after the store panel
is closed. A failed install **stays visible** on the tile until it is retried or
dismissed.

---

## Start · Stop · Restart

| Operation | Managed app (Maison wrote its folder) | Unmanaged app (a stack Maison merely discovered) |
|---|---|---|
| **Start** | `stackup.Up` — so a fully-down stack whose containers were removed is *recreated*, folders and hooks included. | `docker start` on the existing containers. There are no compose files, so there is nothing to declare. |
| **Stop** | `docker stop` on the project's containers. No hooks. The folder stays. | Same. |
| **Restart** | `docker restart`. **No hooks, no folders, no compose** — it is a container-level bounce, not an up. | Same. |

Restart deliberately does *not* run the up sequence. If you want folders and
`pre_up` re-applied, that is a **Start** (or a config save), not a restart.

While any of these run the tile is **busy**: greyed with a `…` overlay and no burger
menu (see `app-model.md`).

---

## Update

`Installer.ApplyUpdate`, driven by the update reference recorded in the override at
install time (`store` + `store-app-id`).

```
1. fetch the store's current compose for store-app-id
2. apply the same transform used at install → byte-comparable with what's on disk
3. equal? → nothing to do, report "up to date"
4. overwrite docker-compose.yml (the strict base only)
5. stackup.Up  → folders (including any the new version introduces) → pre_up → up → post_up
```

The override and `.env` are never touched — that is the entire point of keeping the
base byte-identical to the store. `pre_install` / `post_install` do **not** re-run;
`pre_up` / `post_up` do, because an update is an up.

---

## Save config / Save web UI

`SetConfig` writes the override (after validating that it parses — a typo must not
leave an app whose only repair path is the config window that broke it), then
`stackup.Up`. `SetWebUI` merges the `webui-*` keys into the override and does the
same.

So **saving a config re-runs the up sequence**, hooks and folders included. An
override that adds a `folders` entry gets its directory created on save, not on the
next restart.

`SetTips` is the exception: tips never affect the running container, so saving them
writes the override and stops there. No Docker call at all.

---

## Uninstall

Stop and remove the containers, then **move** the app folder to
`.backups/<app>/<stamp>` (or zip it). Nothing is deleted, and no hooks run — Maison
has no `pre_uninstall` / `post_uninstall`, on purpose: a hook that fires while the
app is being taken away is a hook that can fail and leave the operator unable to
uninstall. The archive is the safety net instead. See `app-model.md` for the archive
format and the restore path.

```
1. remove containers   (Remove progress bar — one tick per container, because a
                        single stop can block for the whole stop-grace period)
2. archive the folder  (Archive progress bar — instant for a rename, metered by
                        bytes for a zip)
```

### Detached, like an install

`DELETE /api/apps/{id}` **starts** the uninstall and returns `202 Accepted`; only an
up-front refusal (a protected app → `403`) is answered synchronously. The work runs
on a background context, so it survives the request, and the confirmation dialog
closes at once instead of blocking the dashboard on a zip that can take minutes.

Progress rides the live app list exactly the way an install's does — the tracker
lives in `apps.Registry` (`StartUninstall` / `Uninstalls` / `ClearUninstall`), and
`server.overlayUninstalls` stamps it onto the app's tile. The tile renders the *same
single bar as an install, in red*: **Remove**, then **Archive**. There is never a
placeholder tile to append (unlike an install): the folder is what makes the tile,
and it only disappears at the last step.

A failed uninstall **stays visible** as a red `!` on the tile, with the error as its
tooltip, until it is retried or dismissed (`POST /api/apps/{id}/dismiss`, which also
clears a failed install or backup). Every failure path leaves the app folder in
place, so there is always a tile for the error to land on.

---

## Backup

Archive the app folder **without** uninstalling. The only operation that stops a
running app on purpose, so the sequence exists to keep that window short:

```
1. copy       full mirror of AppData/<app> into .backups/<app>/.staging-<stamp>
              — the app is still up, so this costs no downtime
2. stop       (skipped when the app was already stopped)
3. sync       mirror again: only files whose size or mtime changed, plus a prune
              of anything deleted — this is the entire downtime
4. start      deferred, so it runs even if a later step fails
5. compress   zip the snapshot and delete it (zip=true only); a folder backup
              just renames .staging-<stamp> → <stamp>, which is the commit point
```

The mirror is plain Go (`apps.mirror`), not rsync: the runtime image carries no
rsync, and the incremental test — same size *and* same mtime — is the same one
rsync makes by default. Irregular files (sockets, fifos) are skipped rather than
opened, so an app that leaves a socket in its folder is still backupable.

`POST /api/apps/{id}/backup?zip=` **starts** it and returns `202 Accepted`. Only
the up-front refusals are synchronous: an unknown app, or not enough free space
(`400`, measured by `EstimateBackup` — the folder's size × 1.1 for a folder
archive, × 2 for a zip). Progress rides the live app list through
`apps.Registry.StartBackup` / `Backups` / `ClearBackup` and
`server.overlayBackups`, on the same single tile bar as an install or an
uninstall, in amber: **Copy**, then **Sync**, then **Compress**.

Nothing is committed until the final rename, so a crash mid-backup leaves only a
`.staging-…` folder or a `.partial` zip — neither of which parses as an archive,
so neither is ever listed or restored.

## Restore

The mirror image, and reversible by construction:

```
1. stop       (if running)
2. archive    rename AppData/<app> → .backups/<app>/<now>   ← instant, free
3. restore    folder archive: renamed back (consumed)
              zip archive:    extracted (survives, restorable again)
4. start      deferred, as above
```

`POST /api/apps/{id}/restore {"name": …}` for a live app, or
`POST /api/backups/{app}/restore` for one that has been uninstalled — the same
detached path, which simply finds nothing to do at steps 1, 2 and 4. Step 2 is why
a restore can never destroy the state it replaces.

---

## What a hook sees

Hooks run through `/bin/bash -c` **inside the Maison container**, with the working
directory set to the app's folder, but they act on the **host** Docker daemon.

| Variable | Value |
|---|---|
| `AppID` | The compose project name. |
| `APP_DIR` | The app's directory, as the **host** sees it. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` — the host daemon. |
| `DATA_ROOT`, `PUID`, `PGID`, `TZ`, `REF_*`, … | The app's base interpolation variables. |
| everything in the app's `.env` | So a hook sees the same values its compose does. |

Because they target the host daemon, `/DATA` and `${DATA_ROOT}` **inside a hook's
script text** are rewritten to host paths — a `docker run -v` in a hook must name a
path the host daemon can resolve.

That rewrite is also the one trap worth knowing:

> A hook that just wants a directory to exist must **not** `mkdir` it. The path it
> writes is a *host* path, but the `mkdir` runs *in the Maison container* —
> creating the wrong directory in the wrong place, and leaving the app with an empty
> mount. **Declare it under `folders`** instead: those are created through Maison's
> data mount and are correct on both sides of the socket.

Hooks are for Docker-level work (pulling a sidecar image, priming a volume with
`docker run`, poking another stack). Directories are what `folders` is for.

### Paths, in code

Two mappings, easy to confuse, both in `internal/envinject`:

| Function | Use on | Does |
|---|---|---|
| `ContainerPath` | a **real path** | host spelling (`/DATA/…`, `${DATA_ROOT}/…`, the literal host path) → this container's data mount. This is how folders get created. |
| `HostPath` | a **real path** | the inverse: container path → host path. This is how `APP_DIR` is built. |
| `RewriteToHostPath` | **script text** | rewrites the `/DATA` / `${DATA_ROOT}` *spellings* a hook author wrote. Single-pass — a host path normally ends in `/DATA`, so rewriting twice would yield `/opt/maison/opt/maison/DATA`. |

---

## Failure semantics, in one table

| Step | Fails the operation? | Why |
|---|---|---|
| Declared `folders` entry (bad path, unknown user, unquoted mode) | **Yes** | It is the author's explicit contract, and starting without it gives the app an unwritable mount — a confusing "permission denied" instead of a clear error. |
| `pre_install`, `pre_up` | **Yes** | Preconditions. |
| `post_install`, `post_up` | No (logged) | The stack is already up. |
| `docker compose up` | **Yes** | Obviously. |
| Ownership / mode (`chown`, `chmod`) on a folder | No (logged) | Not every filesystem supports it, and that should not block an otherwise healthy start. |
| Free-space check before a backup | **Yes**, before anything is copied | Filling the data disk breaks every app on the box, not just this one. |
| Either mirror pass of a backup | **Yes** | A snapshot that is missing files is not a backup, and silently keeping it would be worse than failing. |
| Restarting the app after a backup or restore | No (logged) | The restart is deferred so it runs even when the operation failed — an app left down is a worse outcome than a missing backup. |
| Compressing a backup | **Yes** | The zip is only renamed into place once it is whole, so a failure leaves a `.partial` that is never listed. |

An install that fails leaves the app's folder **in place**, half-configured — which
is correct: the folder is the tile, the failure is visible on it, and a retry is a
plain re-install over what is already there.
