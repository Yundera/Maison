# `x-compose-app` — Maison's compose extension

`x-compose-app` is a Compose top-level extension that Maison reads to render an
app tile, open its web UI, and populate the store. It exists **alongside**
`x-casaos`, not instead of it:

- Maison still consumes the **unmodified CasaOS App Store** (`x-casaos`). Nothing
  here requires changing existing store apps.
- When an app *also* carries `x-compose-app`, Maison **prefers** it for every
  field it defines, and falls back to `x-casaos` (then to derivation) for anything
  it omits.
- An app may ship `x-compose-app` **alone** — Maison renders it fully without an
  `x-casaos` block.

The design goal is a **click URL that mirrors the reverse-proxy route**. Instead of
CasaOS's approach — asking for a container port and *deriving* a hostname at
install time — `x-compose-app` lets the author declare the **final web-UI URL**
directly, the same way they already declare the app's Caddy route. The
`webui-host` value *is* the Caddy label's host:

```yaml
services:
  jellyfin:
    labels:
      caddy_0: jellyfin-${DOMAIN}            # the route
x-compose-app:
  webui-host: jellyfin-${domain}             # the click URL host — same string
```

> Scope: this document specifies **only what Maison consumes**. Unknown keys are
> tolerated and skipped.

---

## Top-level shape

```yaml
# docker-compose.yml
name: jellyfin
services:
  jellyfin:
    image: jellyfin/jellyfin:10.9.11
    expose: ["8096"]
    labels:
      caddy_0: jellyfin-${DOMAIN}
      caddy_0.reverse_proxy: "{{upstreams 8096}}"

x-compose-app:
  schema_version: 1
  id: jellyfin
  title: Jellyfin
  icon: https://cdn.example.com/jellyfin.svg
  category: Media
  tagline: Free software media system
  description: |
    Jellyfin is a media server for organizing and streaming your collection.
  developer: Jellyfin
  screenshots:
    - https://cdn.example.com/jellyfin/1.png

  # --- the click URL ---
  webui-host: jellyfin-${domain}
  webui-path: /web/
```

### Fields

| Field | Type | Required | Meaning / Maison use | `x-casaos` fallback |
|---|---|---|---|---|
| `schema_version` | int | no (default `1`) | Spec version (currently **2**). Maison refuses versions it doesn't understand and falls back to `x-casaos`. v1 files keep working; declare `2` if the app *needs* `folders`/`hooks` to be honoured, so an older Maison refuses it instead of silently starting it without its directories. | — |
| `id` | string | no | Stable app identifier (should equal the Compose project `name`). Defaults to the project name. | `store_app_id` |
| `title` | string \| localized | no | Tile + store display name. | `title` |
| `icon` | url | no | Tile icon. | `icon` |
| `category` | string | no | Store grouping. | `category` |
| `tagline` | string \| localized | no | One-line store summary. | `tagline` |
| `description` | string \| localized | no | Store long description (Markdown). | `description` |
| `developer` | string | no | Store attribution. | `developer` / `author` |
| `screenshots` | url[] | no | Store gallery. | `screenshot_link` |
| `thumbnail` | url | no | Store card image. | `thumbnail` |
| `architectures` | string[] | no | e.g. `[amd64, arm64]`. Advisory. | `architectures` |
| **`webui-host`** | string | **yes\*** | The web UI's **host** — the final URL host, templated exactly like the app's Caddy route (e.g. `jellyfin-${domain}`). Omit for headless apps. | derived from `hostname` |
| `webui-port` | string | no (default `""`) | The **URL** port, appended as `:<port>`. **Not** the container port — it exists only to build the URL and is empty in the common gateway case (standard 443). | derived from `port_map` |
| `webui-scheme` | `http` \| `https` | no (default `https`) | The URL scheme the **browser** uses. | `scheme` |
| `webui-path` | string | no (default `/`) | Path appended to the host. May include a query string (e.g. `/?hash=$AUTH_HASH`). | `index` |
| `links` | object[] | no | Extra buttons on the detail view: `{ name, url, icon? }` with an **absolute** `url`. Never the tile's default action. | — |
| `tips` | string \| localized | no | Guidance note (Markdown, `${VAR}` references resolved from the app's `.env`) shown from the tile menu. This is where Maison writes tips edited in **App settings** — into the **override's** `x-compose-app.tips`, never the store-provided base compose. When set, it replaces the `x-casaos` tips; clearing it falls back to them. | `tips.before_install` + `tips.custom` |
| **`folders`** | object[] | no | Directories **created and owned before every `up`**. See [Folders](#folders). | — |
| **`hooks`** | object | no | `{ pre_install, post_install, pre_up, post_up }` — host shell around the app's lifecycle. See [Hooks](#hooks). | `pre-install-cmd` / `post-install-cmd` |

\* `webui-host` is required only to have a **clickable app**. An app with no
`webui-host` (and no `x-casaos` fallback) is headless — its tile has no "open"
action.

**Localized** means either a plain string (`title: Jellyfin`) or a locale map
(`title: { en_us: Jellyfin, fr_fr: Jellyfin }`). Maison prefers `en_us`.

---

## The web-UI URL

Maison builds the click URL by **direct string construction** — no container
ports, no reading routes back, no baked-in state:

```
<webui-scheme>://<resolved webui-host><:webui-port if set><webui-path>
```

| Part | Source | Default |
|---|---|---|
| scheme | `webui-scheme` | `https` |
| host | `webui-host`, after placeholder resolution | — (required) |
| port | `webui-port` | `""` → omitted |
| path | `webui-path` | `/` |

### Host placeholders

`webui-host` may contain deployment placeholders, resolved by Maison from its
own configuration (so the value can be shared verbatim with the Caddy label):

| Placeholder | Resolves to | Source |
|---|---|---|
| `${domain}` / `${DOMAIN}` | the deployment's base domain | Maison `REF_DOMAIN` |

- Resolution happens on **every render**, so the URL tracks a domain change and
  works for **unmanaged/discovered** apps Maison never installed — nothing is
  stored.
- If `webui-host` references `${domain}` but the deployment has no domain
  configured (`REF_DOMAIN` empty), the URL is treated as **unresolvable**: the tile
  shows the "no reachable address" hint rather than a broken link.
- A literal `webui-host` (no placeholder, e.g. `nas.example.com`) is used verbatim.

### Examples

**Gateway app** (behind Caddy — the common case):

```yaml
x-compose-app:
  webui-host: jellyfin-${domain}
  webui-path: /web/
# REF_DOMAIN=app.localhost  →  https://jellyfin-app.localhost/web/
```

**Direct port access** (no gateway) — set a literal host and a URL port:

```yaml
x-compose-app:
  webui-host: nas.example.com
  webui-scheme: http
  webui-port: "8096"
# → http://nas.example.com:8096/
```

**Headless app**: omit `webui-host` → the tile has no "open" action.

**Extra buttons**:

```yaml
x-compose-app:
  webui-host: photoprism-${domain}
  links:
    - name: Docs
      url: https://docs.photoprism.app
```

---

## Reserved keys Maison writes

Two keys in an *installed* app's **override** are Maison's own bookkeeping rather
than author fields. They are listed here so a store author doesn't reuse the names,
and so an operator reading the file knows what they are:

| Key | Written by |
|---|---|
| `store` / `store-app-id` | The install, recording where the app came from so it can be updated later. |
| `generated-routes` | Route generation: the Caddy label keys Maison added to publish the app on the deployment's **additional domains**, and will delete before rewriting them. See [`domains.md`](./domains.md). |

Route generation is the other reason to keep `webui-host` and the app's `caddy_N`
label the same string. Maison clones the app's **Caddy route group** onto every
additional domain the deployment answers on (`sslip.io`, `nip.io`, …), so an app
that declares its route in labels is reachable at all of them with no extra field
here — and its click URL keeps mirroring the route it was cloned from.

---

## The stack-up sequence

`folders` and `hooks` hang off one sequence, which **every** `docker compose up`
Maison runs goes through — install, start from the tile, store update, and
saving the app's config all take the same path:

```
ensure folders  →  pre_up  →  docker compose up -d  →  post_up
```

`pre_install` / `post_install` bracket that sequence, but only the **first** time —
during the install itself:

```
write compose + .env  →  ensure folders  →  pull images
                      →  pre_install  →  [ the up sequence ]  →  post_install
```

So a directory declared under `folders` is guaranteed to exist before an image is
pulled, before any hook runs, and before the containers start — on the first boot
*and* on every boot after it.

[`lifecycle.md`](./lifecycle.md) is authoritative for these sequences, and covers
what the other operations (start, restart, update, save, uninstall) do with them.

---

## Folders

Compose creates a missing bind-mount source as an empty **root-owned** directory.
An app that drops privileges to `PUID:PGID` then can't write to its own config
volume — the classic "permission denied on first start". `folders` fixes that
declaratively: Maison creates each one and takes ownership of it *before* the
stack comes up.

**`folders` is the only mechanism.** Maison does not read `volumes:` and guess
which bind sources are directories — a compose file says nothing about whether a
source is a directory or a config file, and every heuristic for it (a trailing
`/`, a dot in the last segment) is wrong in one direction or the other: a
directory created where the app wanted a file, or a silently root-owned data dir.
So a directory an app needs is a directory the app **declares**. Anything not
declared is left to Docker, exactly as it would be outside Maison.

That is a porting step for an app coming from a CasaOS store: its bind mounts
work, but any directory that needs `PUID:PGID` ownership before first start has
to be listed here.

```yaml
x-compose-app:
  schema_version: 2
  folders:
    - /DATA/AppData/${AppID}/config          # shorthand: just a path
    - path: /DATA/AppData/${AppID}/data      # full form
      user: "${PUID}"
      group: "${PGID}"
      mode: "0750"
    - path: /DATA/Media
      group: media
      recursive: true                        # reclaim what's already in there
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | — (required) | Absolute path of the directory, under the data root. Interpolated (see below). |
| `user` | uid \| name | `${PUID}` | Owning user. |
| `group` | gid \| name | `${PGID}` | Owning group. |
| `mode` | octal string | `"0755"` | Permissions of `path` itself. **Must be quoted** — see the trap below. |
| `recursive` | bool | `false` | Apply `user`/`group` to **everything already inside** `path`, not just `path` itself. |

A list entry may be a bare string (`- /DATA/AppData/app/config`), which means that
path with every default.

### Interpolation and path resolution

`path`, `user`, `group` and `mode` are interpolated with the same variables the
app's own compose sees: the base variables (`${DATA_ROOT}`, `${AppID}`, `${PUID}`,
`${PGID}`, `${REF_*}`, …) overlaid with the app's persisted `.env` — so a folder can
follow a path the operator configured there.

The path names the **host** location, exactly as a bind-mount source does
(`/DATA/...`, `${DATA_ROOT}/...`, or the literal host path). Maison maps it back
into its own data mount to create it, so it is correct on both sides of the socket.

Three things make a folder a **declaration error** and fail the up, rather than
being silently skipped:

- a variable that resolves to nothing (`${NOPE}` left in the path),
- a relative path,
- a path outside the data root (`/etc/cron.d`, or `/DATA/../etc`) — the data root is
  the only host directory Maison has mounted, so anything else would quietly
  create a directory *inside the Maison container* and mount an empty one into the
  app.

Ownership and mode are applied **best-effort**: a filesystem that can't `chown`
logs a warning rather than blocking an otherwise healthy start.

### `mode` must be quoted

```yaml
mode: "0755"   # ✅
mode: 0755     # ❌ YAML types this as an octal *int* — the leading zero is gone
               #    by the time Maison sees it, and the app fails to install.
```

Maison rejects the unquoted form with an error naming the fix rather than
guessing what `493` was supposed to mean.

### `recursive`

`recursive: true` walks the existing tree and applies `user`/`group` to every entry
below `path`. Use it when an app must reclaim a directory it didn't create — a
restored backup, a media library written by another app, a tree an earlier
root-running version of the app left behind.

It rewrites **ownership only**. `mode` still applies to `path` itself and nothing
else: rewriting the mode of every file below would flip executable bits the app
deliberately set for itself.

It is not free — the walk is proportional to the size of the tree, so don't put it
on a multi-terabyte media folder that is already correct.

---

## Hooks

Shell snippets around the app's lifecycle. Two pairs, differing in **when** they
fire:

| Hook | Runs |
|---|---|
| `pre_install` | Once, when Maison installs the app — after the images are pulled, before the first up. |
| `post_install` | Once, right after that first up succeeds. |
| `pre_up` | Before **every** `docker compose up` — first install, every later start, update, and config save. |
| `post_up` | After every `docker compose up`. |

```yaml
x-compose-app:
  schema_version: 2
  hooks:
    pre_install: |
      openssl rand -hex 32 > ${DATA_ROOT}/AppData/${AppID}/secrets/key
    pre_up: |
      docker pull ghcr.io/example/sidecar:latest
    post_up: |
      echo "$AppID up at $(date)" >> /var/log/maison-apps.log
```

`pre_install` / `post_install` generalise the CasaOS `pre-install-cmd` /
`post-install-cmd`, and **win over them** when both are present. A store app that
carries only `x-casaos` keeps working with no change.

### Failure semantics

- **`pre_install` and `pre_up` are fatal.** A pre-hook is the app's precondition; if
  it doesn't hold, the stack must not start. A failing `pre_up` blocks the app on
  *every* start, which is the point — don't put anything flaky in one.
- **`post_install` and `post_up` are logged and swallowed.** The stack is already
  running by then, and tearing a healthy app back down over a failed after-the-fact
  tweak would be worse than the failed tweak.

### Execution environment

Hooks run through `/bin/bash -c` **inside the Maison container**, with the working
directory set to the app's folder, but they talk to the **host** Docker daemon
(`DOCKER_HOST=unix:///var/run/docker.sock`). They get the app's interpolation
variables plus its `.env`, `AppID`, and `APP_DIR`.

Because they're aimed at the host daemon, `/DATA` and `${DATA_ROOT}` inside a hook's
script are rewritten to **host** paths — a `docker run -v` in a hook must name a
path the host daemon can resolve. The consequence is the one trap worth knowing:

> A hook that just wants a directory to exist should **not** `mkdir` it. Written in a
> hook, that path is a host path, and the `mkdir` would run in the Maison
> container — creating the wrong directory in the wrong place. Declare it under
> `folders` instead: those are created through Maison's data mount and are correct
> on both sides.

Hooks are for **Docker-level** work (pulling a sidecar image, priming a volume with
`docker run`, poking another stack). Directories are what `folders` is for.

---

## Precedence

For any concern, Maison reads in this order and stops at the first hit:

```
x-compose-app  →  x-casaos  →  runtime derivation (published host port)
```

So `webui-host` wins over the `x-casaos` `hostname`/`port_map` derivation, which in
turn wins over "guess a published host port". An author adds `webui-host` to pin
the click URL and keeps `x-casaos` for CasaOS-store compatibility.

## Minimum viable block

The smallest `x-compose-app` that changes Maison's behavior is just the host:

```yaml
x-compose-app:
  webui-host: myapp-${domain}
```

Everything else falls back to `x-casaos` or Compose metadata.
