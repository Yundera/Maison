# App lifecycle

What CasaDash actually *does* to an app — install, start, update, save, uninstall —
and in what order. This document is authoritative for the sequences and their
failure semantics.

Its two companions:

- [`app-model.md`](./app-model.md) — where an app **lives** on disk and how its tile
  state is derived. Read that first; this document is what happens *to* that layout.
- [`x-compose-app.md`](./x-compose-app.md) — the **declaration** of `folders` and
  `hooks`. This document is what CasaDash does with them.

---

## The one rule

> **Every `docker compose up` CasaDash runs goes through `internal/stackup`.**

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
| **2. Ensure folders** | Create every declared folder; apply user/group/mode; walk the tree when `recursive`. Then create the bind-mount sources inferred from the compose files themselves. | **Fatal** for a *declared* folder (it is the author's contract). Logged for an *inferred* one. |
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

| Operation | Managed app (CasaDash wrote its folder) | Unmanaged app (a stack CasaDash merely discovered) |
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
override that adds a new bind mount gets its directory created on save, not on the
next restart.

`SetTips` is the exception: tips never affect the running container, so saving them
writes the override and stops there. No Docker call at all.

---

## Uninstall

Stop and remove the containers, then **rename** the app folder to
`<app>.<date>.archive` (or zip it). Nothing is deleted, and no hooks run — CasaDash
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
clears a failed install). Every failure path leaves the app folder in place, so
there is always a tile for the error to land on.

---

## What a hook sees

Hooks run through `/bin/bash -c` **inside the CasaDash container**, with the working
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
> writes is a *host* path, but the `mkdir` runs *in the CasaDash container* —
> creating the wrong directory in the wrong place, and leaving the app with an empty
> mount. **Declare it under `folders`** instead: those are created through CasaDash's
> data mount and are correct on both sides of the socket.

Hooks are for Docker-level work (pulling a sidecar image, priming a volume with
`docker run`, poking another stack). Directories are what `folders` is for.

### Paths, in code

Two mappings, easy to confuse, both in `internal/envinject`:

| Function | Use on | Does |
|---|---|---|
| `ContainerPath` | a **real path** | host spelling (`/DATA/…`, `${DATA_ROOT}/…`, the literal host path) → this container's data mount. This is how folders get created. |
| `HostPath` | a **real path** | the inverse: container path → host path. This is how `APP_DIR` is built. |
| `RewriteToHostPath` | **script text** | rewrites the `/DATA` / `${DATA_ROOT}` *spellings* a hook author wrote. Single-pass — a host path normally ends in `/DATA`, so rewriting twice would yield `/opt/casadash/opt/casadash/DATA`. |

---

## Failure semantics, in one table

| Step | Fails the operation? | Why |
|---|---|---|
| Declared `folders` entry (bad path, unknown user, unquoted mode) | **Yes** | It is the author's explicit contract, and starting without it gives the app an unwritable mount — a confusing "permission denied" instead of a clear error. |
| Inferred bind directory | No (logged) | CasaDash guessed it from a volume; a guess should not block a valid start. |
| `pre_install`, `pre_up` | **Yes** | Preconditions. |
| `post_install`, `post_up` | No (logged) | The stack is already up. |
| `docker compose up` | **Yes** | Obviously. |
| Ownership / mode (`chown`, `chmod`) on a folder | No (logged) | Not every filesystem supports it, and that should not block an otherwise healthy start. |

An install that fails leaves the app's folder **in place**, half-configured — which
is correct: the folder is the tile, the failure is visible on it, and a retry is a
plain re-install over what is already there.
