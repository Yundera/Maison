# Maison

**Maison** is a lightweight self-hosted dashboard for managing Docker apps on a
single host. It is a *dashboard-only* reimagining of [CasaOS](https://casaos.io):
same look, same feel, same app store — but stripped down to the one thing most
people actually use it for: **a home screen for your apps + a store to install more.**

Maison is **not** CasaOS and shares none of its code at runtime. It *mirrors* CasaOS's
UX and is **100% compatible with the CasaOS App Store format**, so the exact same store
used by [`casa-img`](https://github.com/Yundera/casa-img) works here unchanged.

It ships as **one container**: a Go binary with the Svelte UI embedded in it, driving the
host Docker engine over the bind-mounted socket. No database, no file manager, no login.

---

## Quick start

```bash
cp .env.example .env          # set DATA_HOST_PATH + DOCKER_GID for your host
docker compose up -d --build  # dashboard on http://localhost:8080
```

> **No auth, by design.** Maison has no login screen and assumes it sits behind a
> trusted boundary (LAN, VPN, or an authenticating proxy). Never expose `:8080` directly
> to the internet.

### The data folder and the host path (read this once)

Maison runs *in* a container, but the apps it installs come up on the **host** Docker
daemon — so their bind-mount sources are resolved **on the host, not inside this
container**. Two env vars keep that straight:

| Env | Meaning | Default |
|-----|---------|---------|
| `DATA_ROOT` | Where the data folder is mounted **inside** the container. Maison reads and writes its own files here. | `/DATA` |
| `DATA_HOST_PATH` | The **host** path of that same folder. Written into every installed app's compose, so the host daemon binds the right directory. | = `DATA_ROOT` |
| `STATE_DIR` | Where everything Maison owns lives — settings, store cache, and the deployment's [`.env.app`](./docs/app-env.md). An in-container path, like `DATA_ROOT`. Move it to keep Maison's state out of a folder that is also an app folder, or onto another volume. | `${DATA_ROOT}/AppData/maison` |

Set `DATA_HOST_PATH` to wherever the data folder really lives on the host, and bind that
same host path to `/DATA`:

```yaml
environment:
  DATA_ROOT: "/DATA"
  DATA_HOST_PATH: "/opt/maison/DATA"   # <- the host directory
volumes:
  - type: bind
    source: /opt/maison/DATA           # <- MUST equal DATA_HOST_PATH
    target: /DATA
```

Use a **host bind**, not a named volume — a bind mount can't point inside another
container's named volume.

---

## Documentation

The README is the overview. These are **authoritative** and win on any conflict:

| Doc | What it specifies |
|-----|-------------------|
| [`docs/app-model.md`](./docs/app-model.md) | Where an app **lives** on disk and how its tile state is derived. Start here. |
| [`docs/lifecycle.md`](./docs/lifecycle.md) | What install / start / update / save / uninstall / backup / restore actually **do**, in order, and their failure semantics. |
| [`docs/x-compose-app.md`](./docs/x-compose-app.md) | Maison's own compose extension: `view`, `folders`, `hooks`, and the resolved web-UI URL. |
| [`docs/domains.md`](./docs/domains.md) | The **additional domains** apps are published on (`sslip.io` / `nip.io` / your own), and the Caddy routes Maison generates for them. |
| [`docs/FEATURE-COMPARISON.md`](./docs/FEATURE-COMPARISON.md) | Row-by-row scope table vs `casa-img`. |
| [`docs/backup.md`](./docs/backup.md) | **Design, not yet implemented.** Offsite backup and disaster recovery: the two backup sets, the pluggable engine, scheduling, retention, encryption, and recovery mode. |

> **Reference implementation:** the full CasaOS bundle Maison slims down lives at
> `D:\workspace\yundera\yundera-root\packages\casa-img` (container path
> `/d/workspace/yundera/yundera-root/packages/casa-img`). When a *CasaOS* behaviour is
> ambiguous, that package is the source of truth. When a *Maison* behaviour is
> ambiguous, `docs/` is.

---

## 1. Goals

- **Dashboard-first.** The app grid + system widgets are the product. Everything else is
  trimmed.
- **Pixel-for-pixel CasaOS UX.** Layout, widgets, interactions, and visual design match the
  CasaOS dashboard (see [§4](#4-ux)). A CasaOS user should feel no difference on the home
  screen.
- **CasaOS App Store, unchanged.** Consume the identical app-store zip `casa-img` uses. No
  new store format, no forked catalog. **Multi-store**, with auto-update.
- **Unmanaged-app discovery.** Any Compose stack on the host carrying `x-casaos` metadata —
  even one Maison didn't install — surfaces as a tile. This replaces CasaOS's manual
  "install a customized app" form.
- **Single lightweight container.** One image, one port, the host Docker socket.
- **Zero-config.** Boots to a usable dashboard: no setup wizard, no login.

## 2. Non-goals (explicit exclusions)

| Excluded | Why |
|----------|-----|
| **File manager / Files app** | Maison is dashboard-only. No `/DATA` browser, no upload/download UI. |
| **Authentication / users / login** | No login screen, no sessions. Maison assumes a trusted network boundary; the operator owns access control. |
| **Multi-user / RBAC** | Single implicit operator. No accounts. |
| **Global search bar** | The dashboard has no search box. |
| **Promo / onboarding cards** | No getting-started cards on the home screen. |
| **Manual "install a customized app" form** | Replaced by unmanaged-app auto-discovery ([§4.3](#43-main-column)). |
| **Terminal / SSH / hardware tools** | No disk/RAID management, network shares, or remote-storage tooling. |
| **Being CasaOS** | Not a fork you can drop CasaOS plugins into. Compatibility is *store format only*. |

## 3. Positioning vs CasaOS / casa-img

`casa-img` is a full CasaOS bundle: a dashboard **plus** a file manager, user accounts,
disk/storage management, network shares, and remote-storage tooling. Maison keeps only
the dashboard + app-management surface.

| Feature area | casa-img | Maison |
|--------------|----------|----------|
| App-grid dashboard (same UX) | ✅ | ✅ |
| App store: browse / install / uninstall / auto-update | ✅ | ✅ |
| Multi-store | ✅ | ✅ |
| Multi-service Compose apps | ✅ | ✅ |
| System widgets (CPU / RAM / storage) | ✅ | ✅ |
| Per-app logs, container stats, live status | ✅ | ✅ |
| i18n / multi-language | ✅ | ✅ |
| Authentication / users | ✅ | ❌ |
| File manager / Files | ✅ | ❌ |
| Global search bar · promo cards | ✅ | ❌ |
| Manual "install customized app" form | ✅ | ❌ → unmanaged-app discovery |
| On-disk app layout | CasaOS nesting | 🔶 **flat `AppData/<app>`** ([`app-model.md`](./docs/app-model.md)) |
| Per-app config edits | rewrites the compose | 🔶 **override compose file** ([§5.1](#51-per-app-configuration)) |
| Disk/RAID mgmt, Samba shares, remote storage | ✅ | ❌ |

Full table: [`docs/FEATURE-COMPARISON.md`](./docs/FEATURE-COMPARISON.md).

---

## 4. UX

The dashboard is a 1:1 copy of the CasaOS home screen. Live reference:
<https://demo.nsl.sh/#/> (the casa-img demo — minus the login page and Files tile, which
Maison does not have).

Theming is **wallpaper-only** (CasaOS has no light/dark toggle); the App Store is a
**panel**, not a page you navigate away to; settings live in a **top-bar dropdown**.

### 4.1 Layout

```
┌──────────────────────────────────────────────────────────────────────────┐
│  [top bar: brand · social links · settings dropdown]                      │  ← top toolbar
├───────────────────────┬──────────────────────────────────────────────────┤
│  ┌─────────────────┐  │                                                    │
│  │  12:28          │  │   App                                        [ + ]│  ← app grid header
│  │  Thu, 9 Jul     │  │  ┌──────┐  ┌──────┐  ┌──────┐  ┌──────┐           │
│  └─────────────────┘  │  │ App  │  │ App  │  │ App  │  │ App  │  ...       │  ← installed apps
│  ┌─────────────────┐  │  │Store │  │  A   │  │  B   │  │  C   │            │
│  │  System status  │  │  └──────┘  └──────┘  └──────┘  └──────┘           │
│  │   12% CPU  0°C  │  │  ┌──────┐  ┌──────┐                                │
│  │    9% RAM       │  │  │ App  │  │ App* │   * = unmanaged (auto-detected)│
│  └─────────────────┘  │  │  D   │  │  E   │                                │
│  ┌─────────────────┐  │  └──────┘  └──────┘                                │
│  │  Storage        │  │                                                    │
│  │  Healthy        │  │   (no global search bar, no promo cards)           │
│  │  12.95/386 GB   │  │                                                    │
│  │  ▓░░░░░░░░       │  │                                                    │
│  └─────────────────┘  │                                                    │
│  [ Widget settings › ]│                                                    │
└───────────────────────┴──────────────────────────────────────────────────┘
```

### 4.2 Left column — widgets

- **Clock/date** — large local time + full date.
- **System status** — two radial gauges: CPU % (with temperature, `0°C` when unavailable)
  and RAM % (with absolute value). Live-updating.
- **Storage** — disk health badge, `Used / Total`, usage bar.
- **Widget settings** — toggle which widgets show.

### 4.3 Main column

Just the app grid — no global search bar, no promo cards.

- Each **tile** = icon + name, with a status dot driven by the container health check.
  Hover reveals **Open** and a burger (⋯) menu: **Open, Settings, Restart, Stop, Start,
  Uninstall**. Tiles are **drag-to-reorder**.
- **App / System.** The section heading is a switch when the box has system apps —
  the platform's own pieces, which declare `view: system` in their compose
  ([`docs/x-compose-app.md`](./docs/x-compose-app.md)). They get their own grid, and
  their menu withholds **Stop** and **Uninstall** (the API refuses both); **Restart**
  stays. `view: hidden` keeps an app off the dashboard entirely. With no system app
  declared, the heading is a plain "App" and nothing else changes.
- A tile is **greyed** when stopped, shows a **`…` overlay** while a lifecycle op is in
  flight, and shows **two progress bars** (image download + stack start) while installing.
- **Unmanaged apps** — any host Compose stack carrying `x-casaos` that Maison didn't
  install appears automatically, badged **unmanaged**. Maison reads its `x-casaos` for
  icon/name/link and can start/stop/restart it, but its config is owned by whatever
  created it.
- **`+` menu** → **add an external link** (pin a bookmark tile to any URL).
- The **App Store** tile opens the store panel ([§5](#5-app-store-casaos-compatible)).

### 4.4 Settings (top-bar dropdown)

Wallpaper, **language** (`en_us`, `fr_fr`, `de_de`, `zh_cn` — one JSON per language),
widget visibility, and the **app-store source URLs** (multi-store).

---

## 5. App Store (CasaOS-compatible)

- **Source:** a GitHub zip of Compose listings, identical to `casa-img`'s:
  `https://github.com/Yundera/AppStore/archive/refs/heads/main.zip`. Set `APPSTORE_URL` to
  a **comma-separated** list for multiple stores. Catalogs are cached under
  `${DATA_ROOT}/AppData/maison/appstore` and refreshed hourly.
- **App format:** standard `docker-compose.yml` + the CasaOS **`x-casaos`** block (title,
  icon, tagline, category, screenshots, main port/scheme/path). Read and honoured
  unchanged.
- **Store UI:** category browse, featured/most-popular, in-store search, and an app detail
  page (screenshots, description, developer, install). Deep-linkable at `/store/<app>`.
- **Install:** writes the app's project to `${DATA_ROOT}/AppData/<app>/` and brings it up
  through the lifecycle pipeline ([§6.2](#62-the-one-rule)). The store's
  `docker-compose.yml` is copied **byte-for-byte** and never modified — customization goes
  in the override file, which is what lets updates stay clean.
- **Env / template injection:** the CasaOS/`casa-img` variables resolve as they do
  upstream — `DATA_ROOT`, `REF_NET`, `REF_PORT`, `REF_DOMAIN`, `REF_SCHEME`,
  `REF_SEPARATOR`, plus base vars (`PUID`, `PGID`, `TZ`, `AppID`) and the app's own `.env`.
  Exact rules: `casa-img/docs/environment-variable-injection.md`.

### 5.1 Per-app configuration

Maison keeps a CasaOS-style config window (ports, env, volumes) but **diverges on how
edits persist**:

- The store's `docker-compose.yml` is **never modified**.
- Edits are written to a separate **`docker-compose.override.yml`**, layered on via Compose
  override semantics. The running app = base + override.
- The override also carries the **update reference** (which store, which catalog id) — so
  it survives a base re-copy on update.

---

## 6. Architecture

One Go binary with the Svelte SPA embedded (`//go:embed`), talking to the host Docker
daemon. No database: **the filesystem and the Docker daemon are the state.**

```
┌──────────────────────── Maison container ─────────────────────────┐
│  Svelte 5 SPA (embedded)  ──REST /api + WebSocket /ws──►  Go server  │
│                                      ├── system   CPU/RAM/disk       │
│                                      ├── apps     registry (managed  │
│                                      │            + unmanaged)       │
│                                      ├── appstore fetch/cache/merge  │
│                                      ├── installer install + update  │
│                                      └── stackup ──► docker compose ─┼──► /var/run/docker.sock
│  Serves on one HTTP port (:8080)                                     │        (HOST engine)
└──────────────────────────────────────────────────────────────────────┘
```

### 6.1 Packages

| Package | Role |
|---|---|
| `internal/server` | chi router: REST `/api`, WebSocket `/ws`, SPA catch-all. Also dispatches by `Host:` — a request for an app's gateway host, while that app is down, gets the **launch gate** instead of the dashboard. |
| `internal/apps` | The tile list: reconciles on-disk projects with live Docker state; surfaces unmanaged `x-casaos` stacks. |
| `internal/appstore` | Fetches, caches, and merges CasaOS store zips (multi-store). |
| `internal/installer` | Store install + update, with two-track progress (image pull, stack start). |
| `internal/stackup` | **Every** `docker compose up` goes through here (see below). |
| `internal/composecmd` | Shells out to the `docker compose` plugin. |
| `internal/dockerx` | Docker Engine API: discovery, lifecycle, container event stream. |
| `internal/envinject` | CasaOS env/template rules + the host-path rewrites. |
| `internal/xcasaos` · `internal/xcomposeapp` | The two compose extensions. |
| `internal/live` | WebSocket hub, channel-multiplexed; sampling only runs while a client is subscribed. |
| `web/` | Svelte 5 + Vite. Vite builds straight into `internal/ui/dist`, which the Go binary embeds. |

### 6.2 The one rule

Every `docker compose up` goes through `internal/stackup`:

```
ensure folders  →  pre_up hook  →  docker compose up -d  →  post_up hook
```

Install, start, update, save-config and save-web-UI all land there. **Never call
`composecmd.Up` directly** — an app started that way comes up without its declared
directories, and the bug only surfaces on someone's *second* boot. Full sequences and
failure semantics: [`docs/lifecycle.md`](./docs/lifecycle.md).

### 6.3 Live updates

One multiplexed WebSocket carries system stats, app status, and per-app logs/stats.
Container events, lifecycle transitions, and install progress all rebroadcast the app
list, so tiles move on their own. Channels are subscribe-gated: nobody watching, nothing
sampled.

---

## 7. Development

```bash
# Backend (serves the embedded UI on :8080)
go run ./cmd/maison
go test ./...

# Frontend (Vite on :5173, proxies /api + /ping + /ws to :8080)
npm --prefix web install
npm --prefix web run dev
npm --prefix web run build     # → internal/ui/dist (the Go embed dir)
npm --prefix web run check     # svelte-check
```

`go build` needs `internal/ui/dist` to exist or the `//go:embed` fails — a placeholder
`index.html` is committed there for exactly that reason. Run the Vite build for a real UI.

### The `dev/` stack

The root `docker-compose.yml` runs Maison alone, which is enough to browse the store and
install ordinary apps. It **cannot** test SSO-enabled store apps: those need a Caddy router
(to route them by their `caddy_*` labels) and a Dex OIDC provider. `dev/` adds exactly
those, wired the way a production PCS is:

```bash
cd dev
cp .env.dev.example .env.dev   # set DOCKER_GID + DATA_HOST_PATH
./gen-certs.sh                 # once
docker compose --env-file .env.dev up -d --build
```

See [`dev/README.md`](./dev/README.md) — including the TLS-trust caveat that bites
back-channel OIDC calls.

---

## 8. Deployment

One container, host Docker socket bind-mounted, one published port, apps on a shared
bridge network. This is the shipped `docker-compose.yml`:

```yaml
services:
  maison:
    image: maison:latest
    build: .
    container_name: maison
    hostname: maison
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      DATA_ROOT: "/DATA"                      # in-container mount target
      DATA_HOST_PATH: "${DATA_HOST_PATH:-/DATA}"  # the SAME folder, host-side
      PUID: "1000"
      PGID: "1000"
      TZ: "${TZ:-UTC}"
      # App-store routing template (CasaOS/casa-img compatible). Optional.
      REF_NET: "mesh"
      REF_PORT: "80"
      REF_SCHEME: "http"
      REF_DOMAIN: "${DOMAIN:-}"
      APPSTORE_URL: "https://github.com/Yundera/AppStore/archive/refs/heads/main.zip"
    group_add:
      - "${DOCKER_GID:-999}"                  # the docker socket's group on the host
    networks:
      - mesh
    volumes:
      - type: bind                            # source MUST equal DATA_HOST_PATH
        source: ${DATA_HOST_PATH:-/DATA}
        target: /DATA
        bind:
          propagation: rshared
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock

networks:
  mesh:
    driver: bridge
    name: mesh
```

### Environment

| Var | Default | Purpose |
|-----|---------|---------|
| `DATA_ROOT` | `/DATA` | Data folder inside the container. |
| `DATA_HOST_PATH` | = `DATA_ROOT` | That folder's path on the host. |
| `APPSTORE_URL` | Yundera AppStore zip | Store source(s), comma-separated. |
| `PUID` / `PGID` | `1000` | Ownership applied to app folders. |
| `TZ` | — | Timezone for the dashboard and installed apps. |
| `REF_NET`, `REF_PORT`, `REF_SCHEME`, `REF_DOMAIN`, `REF_SEPARATOR` | — / `-` | Store templating: app network + `{app}{sep}{domain}` hostnames. |
| `HTTP_ADDR` | `:8080` | Listen address. |

### Notes

- **`/var/run/docker.sock`** is the only privilege Maison needs — it is how apps get
  installed and run. Mounting it is equivalent to root on the host; run Maison only on
  hosts you trust it on. `DOCKER_GID` must match `stat -c '%g' /var/run/docker.sock`.
- **`/DATA`** (bind, `rshared`) holds app data and the compose projects Maison writes.
- **State** lives at `${DATA_ROOT}/AppData/maison/` — settings, store cache, and the
  deployment's [`.env.app`](./docs/app-env.md). It is Maison's own app directory, so
  everything Maison owns is in one place. It holds no `docker-compose.yml` on a
  standalone install and therefore renders no tile; a deployment that installs the
  dashboard's own compose stack here gets a Maison tile, which is intended.
- **Backups** live at `${DATA_ROOT}/AppData/.backups/<app>/<stamp>` — one archive per
  app per moment, holding the whole app folder (compose + override + `.env` + data).
  Make one from an app's **Backups** tab; browse and restore every archive on the box,
  including those of uninstalled apps, from **Settings → Backups**. These are on the
  data disk, so on their own they are a rollback mechanism, not disaster recovery.
- **Cloud backup** (**Settings → Cloud backup**) adds a remote engine — kopia today —
  that sends apps *and* your Documents / Downloads / Media to a repository off the
  box, nightly, with tiered retention. It streams straight to the repository rather
  than staging a copy, so an app occupying most of its own disk can still be backed
  up. The engine runs as a container; Maison ships no binary and installs nothing on
  the host. Credentials are rendered onto the box out of band — Maison only reads
  them. See [`docs/backup.md`](docs/backup.md).
- **Uninstall never deletes.** The app folder is **moved** into `.backups/` (or zipped,
  on request); the data stays put and is one restore away.
- **Health:** `GET /ping` → 200.
- **TLS / public routing** is out of scope — front Maison with a reverse proxy
  (Caddy/Traefik/mesh-router), exactly as `casa-img` is fronted.

---

## 9. Reference material

- **Slim-from source of truth:** `/d/workspace/yundera/yundera-root/packages/casa-img`
  — `Dockerfile` (the 8-service bundle), `dev/docker-compose.yml` (deployment model),
  `conf/app-management/app-management.conf` (store URL + paths),
  `s6-overlay/scripts/casa-init.sh` (socket-GID trick),
  `docs/environment-variable-injection.md` (template rules), `CasaOS-UI/` (the Vue 2.7
  dashboard Maison matches).
- **Live UX reference:** <https://demo.nsl.sh/#/>
- **CasaOS App Store format:** <https://github.com/Yundera/AppStore> · upstream
  <https://github.com/IceWhaleTech/CasaOS-AppStore>
