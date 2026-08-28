# Note: dockerd container-start latency on holyhorse (and what it breaks)

Written 2026-08-28 while debugging "Install does nothing" on holyhorse.nsl.sh.
Two visible bugs, one shared cause. Not fixed — parked here for a proper look.

## Symptom 1 — maison-app log spam

`maison-app` logs this continuously, several times a minute:

```
maison: apps: docker list failed, showing installed apps as stopped:
  Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json?all=1": context deadline exceeded
```

Source: `internal/apps/apps.go:260`, the best-effort fallback in `Registry.List`.
The call is `internal/dockerx/dockerx.go:84-85`, `ContainerList(ctx, {All: true})`
→ `GET /v1.47/containers/json?all=1`. The deadline is inherited from the caller's
ctx (pollers / request ctx), not a constant in dockerx.

User-visible effect: whenever it fires, installed apps render **greyed as stopped**
even though they are running. The grid flickers between correct and all-stopped.

### Measured on the box (Docker 29.5.2, overlayfs, containerd image store, 12 containers / 63 images)

| call | time |
|---|---|
| `containers/json` (all=0) | 0.96 – 1.75 s |
| `containers/json?all=1` (what maison calls) | **3.10 – 3.12 s** |
| `docker ps -aq` (cold / warm) | 5.39 s / 0.47 s |

`dockerd` journal shows, for every single `?all=1`:

```
level=warning msg="failed to resolve container image" containerID=… error="Canceled: context canceled" image="ghcr.io/yundera/mesh-router-tunnel:1.2.10"
level=warning msg="failed to resolve container image" containerID=… error="Canceled: context canceled" image="ghcr.io/yundera/mail-gateway:1.0.4"
…one per container…
level=info msg="request cancelled by client" error="write unix /run/docker.sock->@: write: broken pipe" status=499 request-url="/v1.47/containers/json?all=1"
```

So dockerd resolves the **image config for every container** to serve `?all=1`, hitting
the containerd content store once per container. maison's deadline expires first, the
client hangs up, dockerd logs 499, maison logs `context deadline exceeded`.

`status=499` is the client giving up — this is a latency problem, not a broken daemon.

## Symptom 2 — Install button appears dead (same cause)

Store → Install first blocks on `GET /api/store/<App>/backups`
(`web/src/lib/components/store/InstallButton.svelte:53-69`). Measured from the live page:
**8.1 – 27.5 s**. During that window the button renders nothing at all — `loading`
(line 28) is set but never referenced in the template.

`internal/server/store.go:240-266` spawns a fresh `docker run --rm kopia/kopia:0.23.1`
per engine for `snapshot list`, plus another for `engineDisplay()` → `repository status`.
Two sequential container starts per click.

### Where the time actually goes

| layer | time |
|---|---|
| remote repo I/O (3× `ListBlobs` to the bucket) | **~0.105 s** |
| kopia process total (start → connect → cache scan → list → exit) | **~1.5 s** |
| `docker run` overhead for a container that does nothing | **~6 – 7.5 s** |

From `/DATA/AppDataShared/backup/kopia/logs/cli-logs/…-snapshot-list.0.log`:
span `12:27:06.549 → 12:27:08.051` = 1.50 s, of which ListBlobs = 19.7 + 16.0 + 68.8 ms.
The matching docker events: `create 1787920022 → die 1787920031` = 9 s wall.

Control — a container that runs nothing at all:

```
docker run --rm --entrypoint /bin/true kopia/kopia:0.23.1   → 6.14 / 6.21 / 7.37 s
docker run --rm alpine:3.20 /bin/true                       → 7.28 / 5.10 s
docker create … ; docker start -a …                         → start alone: 7.07 s
```

Image-independent, and it is **`start`**, not `create` or image pull. A healthy box
does this in ~0.3–0.7 s. Neither kopia nor the remote bucket is slow — the daemon is.

## What to look at

1. **Why is container `start` ~7 s here?** That is the root cause of both symptoms and
   also slows every app install/restart on this box. Suspects: containerd image store on
   Docker 29.x + overlayfs, cgroup v2 / systemd driver setup cost, snapshotter I/O.
   Compare against a box on the older `overlay2` + classic image store.
2. **`?all=1` image resolution.** If maison does not need `Image`/`ImageID` in
   `ListProjectContainers`, avoiding the field (or tolerating it unresolved) may sidestep
   the per-container content-store lookup entirely.
3. **Give `Registry.List` its own bounded, generous deadline** rather than inheriting a
   short one, and/or keep the last good container state so a slow poll greys nothing.
4. **Stop paying container start on a UI click path** (see the separate install-flow
   findings): cache the store backup listing, use kopia's `ListAll()`
   (`internal/backup/kopia/kopia.go:882` — "one query is the point") instead of one
   container per app, and cache `engineDisplay()` instead of spawning
   `repository status` inside the engine loop.

Note: the kopia cache at `/DATA/AppDataShared/backup/kopia/cache` **is** working and
persisted across runs (the log shows 12 metadata blobs, 129975 B, reused on each run).
It is only 372 K because the repository is small. It is not the bottleneck.
