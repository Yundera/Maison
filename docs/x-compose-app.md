# `x-compose-app` — Maison's compose extension

`x-compose-app` is a Compose top-level extension and **Maison's definition of an
app**: everything Maison renders, installs, runs and updates comes from it — the
store card, the tile, the web-UI address, folders, secrets, steps, lifecycle,
backups. An app needs nothing else.

`x-casaos` is supported for compatibility only, and **never mixed** with it:

- A compose that carries `x-compose-app` is read from `x-compose-app` alone. An
  `x-casaos` block beside it is **ignored**, field for field — a store may keep one
  for CasaOS itself, but Maison never reads it.
- A compose with **no** `x-compose-app` is a CasaOS app: its `x-casaos` block is
  translated whole into the equivalent `x-compose-app` (the right-hand column of the
  [field table](#fields)) and from there on goes through exactly the same code. Pure
  CasaOS apps are supported as well as that translation allows, and no further.
- The base `docker-compose.yml` decides which of the two an app speaks. What Maison
  and the operator write into the override's `x-compose-app` (`store-ref`, edited
  `tips`, a pinned `webui-host`) layers on top in either case, so installing a CasaOS
  app does not turn it into an empty native one. An override's `x-casaos` is never
  read.

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
  title:
    en_us: Jellyfin
  icon: icon.png
  category: Media
  tagline:
    en_us: Free software media system
    fr_fr: Système multimédia libre
  description:
    en_us: |
      Jellyfin is a media server for organizing and streaming your collection.
  developer: Jellyfin
  screenshots:
    - screenshot-1.png

  # --- the click URL ---
  webui-host: jellyfin-${domain}
  webui-path: /web/
```

[`x-compose-app.example.yml`](./x-compose-app.example.yml) is a complete compose
with **every** field below set once, to show what a fully loaded block looks like.

### Fields

| Field | Type | Required | Meaning / Maison use | Translated from `x-casaos` |
|---|---|---|---|---|
| `schema_version` | int | no (default `1`) | Spec version (currently **2**). `secrets`, `variables`, `files`, `steps` and the seed tree are **additive** and do not raise it — an older build ignores what it does not know instead of dropping the whole block (which would cost the app its folders and routes too). A build that predates the declared version treats the whole block as absent and reads the app as a CasaOS app (its `x-casaos`, if any) — note that it **does not refuse to install the app**, so raising the version degrades an app rather than gating it. v1 files keep working. A block that is merely malformed is *not* absent — Maison does not fall back to its `x-casaos` — but see [A malformed block](#a-malformed-block) for what it costs. | — |
| `id` | string | no | Stable app identifier (should equal the Compose project `name`). Defaults to the project name. | `store_app_id` |
| `title` | localized | no | Tile + store display name. | `title` |
| `icon` | path \| url | no | Tile icon. See [Assets](#assets). | `icon` |
| `category` | string | no | Store grouping. | `category` |
| `version` | string | no | The app's release as people read it (`2.4.0`, `2024.10.1-ls45`). Parsed but not yet displayed, and never compared: which data an install holds is `data_version`'s job. | — |
| **`view`** | `apps` \| `service` \| `system` | no (default: `apps`, or `service` for an app with no web UI) | Which dashboard grid the app's tile lands in — **not** a store category, and not a behaviour. See [Views](#views). | — |
| **`parent`** | string | no | Compose project name of the app this one **extends**. Its tile nests under that app's instead of standing alone. Unresolvable — absent, misspelled, itself an extension — falls back to an ordinary tile. See [`parent` makes the app an extension of another](#parent-makes-the-app-an-extension-of-another). | — |
| `tagline` | localized | no | One-line store summary. | `tagline` |
| `description` | localized | no | Store long description (Markdown). | `description` |
| `developer` | string | no | Store attribution. | `developer`, else `author` |
| `screenshots` | path[] \| url[] | no | Store gallery. See [Assets](#assets). | `screenshot_link` |
| `thumbnail` | path \| url | no | Store card image. See [Assets](#assets). | `thumbnail` |
| **`architectures`** | string[] | no (empty: any) | e.g. `[amd64, arm64]`. The store refuses to install the app on a box of another architecture. See [Requirements](#requirements). | `architectures` |
| **`requires`** | string[] | no | Capabilities the app cannot work without: `public-ip`, `gpu-nvidia`, `gpu-dri`. The store hides the app on a box that lacks one. See [Requirements](#requirements). | tag `needs-public-ip` → `[public-ip]` |
| **`resources`** | object | no | `{ min?, recommended? }`, each `{ cpu?, memory?, disk? }`. Below `min` installing asks for an acknowledgement; below `recommended` the store shows a note. See [Requirements](#requirements). | — |
| `min_memory` | int (MB) | no | Superseded by `resources.min.memory`, into which it is folded when that is absent. | `min_memory` |
| **`webui-host`** | string | **yes\*** | The web UI's **host** — the final URL host, templated exactly like the app's Caddy route (e.g. `jellyfin-${domain}`). Omit for headless apps. | `hostname` |
| `webui-port` | string | no (default `""`) | The **URL** port, appended as `:<port>`. **Not** the container port — it exists only to build the URL and is empty in the common gateway case (standard 443). Without a `webui-host` it names a port on the box's own address (the host the dashboard was opened on), which is how a CasaOS app with no `hostname` opens. | `port_map` (dropped when it is the scheme's default port) |
| `webui-scheme` | `http` \| `https` | no (default `https`) | The URL scheme the **browser** uses. | `scheme` (CasaOS default `http`) |
| `webui-path` | string | no (default `/`) | Path appended to the host; a missing leading `/` is added. May include a query string (e.g. `/?hash=$AUTH_HASH`). | `index` |
| **`routes`** | object[] | no | The app's web endpoints, stated without a domain or a proxy: `{ service?, name?, upstream-port, path?, upstream-scheme?, insecure-upstream? }`. The box's routing setting decides how each is reached — proxy labels on its domains, a published port, or an external router — and Maison writes it. The first route is the web UI. Counts as a web UI for [`view`](#an-app-with-no-web-ui-is-a-service). See [Routes](#routes) and [`routing.md`](./routing.md). | — |
| `links` | object[] | no | Extra buttons on the detail view: `{ name, url, icon? }` with an **absolute** `url` and a localized `name`. Parsed, but **not rendered by any current build**. | — |
| `tips` | localized | no | Guidance note (Markdown, `${VAR}` references resolved from the app's `.env`) shown from the tile menu. This is where Maison writes tips edited in **App settings** — into the **override's** `x-compose-app.tips`, never the store-provided base compose. An edit is stored as `{en_us: …}`, replacing the store's map, so the operator's note shows in every locale; clearing it falls back to the store's tips. | `tips.before_install` per locale, `tips.custom` appended (and `custom` alone as `en_us`) |
| **`folders`** | object[] | no | Directories **created and owned before every `up`**. See [Folders](#folders). |
| **`secrets`** | map | no | Values **generated once** and ensured in the app's `.env`. See [Secrets and variables](#secrets-and-variables). |
| **`variables`** | map | no | Templates ensured in the app's `.env` on **every** up. See [Secrets and variables](#secrets-and-variables). |
| **`credentials`** | object[] | no | The app's **initial logins**, shown to the operator the first time the app is opened. See [Credentials](#credentials). | — (a CasaOS app gets no credentials dialog) |
| **`files`** | object[] | no | Individual files Maison writes — the escape hatch beside the [seed tree](#the-seed-tree). See [Files](#files). |
| **`steps`** | object[] | no | [Script entries](#the-script-entry) run around the app's stack on every up: `{ name, at: setup\|started, run: once\|always }` plus `script` or `file`. See [Steps](#steps). | `pre-install-cmd` → a `setup` step, `post-install-cmd` → a `started` step, both `run: once` |
| **`lifecycle`** | object | no | `{ stoppable, uninstallable, updatable }`, all default `true` — whether Maison may stop, uninstall or update the app. See [Lifecycle](#lifecycle-what-the-operator-may-do-to-the-app). | — |
| **`backup`** | object | no | `{ exclude, skip }` — directories of **derived** data to leave out of backups, or the whole app. See [Backup exclusions](#backup-exclusions) and [Skipping an app entirely](#skipping-an-app-entirely). | — |
| **`data_version`** | int | no (default `1`) | The shape of data this compose expects. An install's data is marked with it; a start refuses data in another shape, and an update converts it with `migrations`. Read from the **base** compose only. See [`migrations.md`](./migrations.md). | — |
| **`migrations`** | object[] | no | One step per data version `2 … data_version`: `{ id, name?, timeout? }` (`name` localized) plus a [script entry](#the-script-entry) (`script` or `file`, with the `images` it runs) or `blocked` (localized Markdown; the update is refused). Read from the base compose only. See [`migrations.md`](./migrations.md). | — |

\* `webui-host` is required only to have a **clickable app**. An app with no
`webui-host` is headless — its tile has no "open" action — unless it publishes a
host port: then the one bound to the web UI's container port, else the first
published, opens it.

The one `x-casaos` field with no `x-compose-app` key, `webui_port` (the web UI's
port *inside* the container), survives the translation only to pick which
published host port opens a CasaOS app; a native app says the same with the
`upstream-port` of its first [`routes`](#routes) entry. Everything else in
`x-casaos` — `main` (which CasaOS reads), the per-service `apps` descriptions
(`envs`, `ports`, `volumes`, `devices`) and the localized `image` map — is not read.

### Localized strings

Every string Maison shows to a person is **localized**: a locale map that must
carry a non-empty `en_us`.

```yaml
title:
  en_us: Jellyfin
  fr_fr: Jellyfin
tagline: { en_us: Free software media system, fr_fr: Système multimédia libre }
```

That covers `title`, `tagline`, `description`, `tips`, `credentials[].label`,
`credentials[].note`, `links[].name`, `migrations[].name` and
`migrations[].blocked`. `category` and `developer` are plain strings: one is the
store's grouping key, the other a proper noun.

The dashboard shows the operator's UI language, falling back to `en_us`.
Server-side text (logs, mail, incidents) always uses `en_us`. Locale codes are the
dashboard's own: `en_us`, `fr_fr`, `de_de`, `zh_cn`, … (any other key is kept and
simply never chosen).

**A plain string is refused**, and so is a map with no `en_us` or with a non-string
value. A plain string would leave the maintainer unsure whether the field is
translated at all, and a map with no `en_us` leaves some readers with nothing to show.
The refused field is **dropped**: the tile falls back to the folder name, the card
loses its tagline, and so on. The rest of the block is kept (contrast
[a malformed block](#a-malformed-block)). The refusal is logged once per store
refresh and reported by `maison lint`. In `migrations` the same refusal is an error
that refuses the update instead, because dropping `blocked` would turn a refusal into
a step with no body.

Check a store before publishing it:

```bash
docker run --rm -v "$PWD:/s" -w /s ghcr.io/yundera/maison:<tag> \
  maison lint Apps/*/docker-compose.yml
```

It prints every dropped field and every `migrations` error, and exits 1 if there is any.

### A malformed block

A block Maison cannot decode — a list where a map belongs, a scalar YAML types
in a way a field refuses — is not reported anywhere. Every reader discards the
parse error and treats the app as carrying **no metadata at all**:

- the tile shows the folder name, no icon and no click URL;
- on every up, `folders`, `secrets`, `variables`, `files` and `steps` are all
  skipped, and `docker compose up` runs anyway.

One bad scalar therefore costs the whole block, not the one field. That is why the
nested entries (`folders`, `files`, `routes`, `credentials`, `backup.exclude`)
decode their scalars as text, so `mode: 0755`, `user: 1000` or
`upstream-port: 80` are taken as written rather than failing the block, and why a
malformed credential `change` only makes that credential initial-only.

`data_version` / `migrations` are the exception: they are read separately, and a
block Maison cannot read there refuses the update instead of meaning "no
migrations".

### Assets

`icon`, `thumbnail` and `screenshots` follow one rule: **a value that is not an
absolute URL names a file beside the compose.**

```yaml
icon: icon.png                 # the app's own folder — the normal way
thumbnail: thumbnail.png
screenshots:
  - screenshot-1.png
  - assets/screenshot-2.png    # subfolders are fine
```

The same line works on both sides of an install, because on both sides the compose
is on disk: in a store, it resolves inside the app's folder in the extracted store
tree; once installed, inside `/DATA/AppData/<app>/`. Maison serves store assets
from `/api/store/<id>/asset/<path>` and the installed tile's icon from
`/api/apps/<app>/icon`, so nothing on the page is a request to a third party.

Prefer this to a URL. A store that ships its own art — which is every store; the
CDN links in them point back at the store's own repository — otherwise pays a
round trip to read bytes it has already downloaded and extracted, and inherits
that host's cache, outages and link rot to do it. A box behind a filtering proxy,
or with no egress, shows blank tiles.

Rules:

- **Absolute URLs stay valid.** `https://…` is fetched as before. It is simply no
  longer the normal way to say where an icon is.
- **The path may not leave the app's folder.** `../`, absolute paths and
  backslashes are rejected, and a rejected value is *dropped*, not retried as a
  URL — a broken path should show a missing image, not a silent request to
  somebody else's server.
- **The extension must be an image one** (`.png .svg .jpg .jpeg .webp .gif .ico
  .avif`) — it is what types the response.
- **`icon.<ext>` is found without being declared.** An app folder holding a plain
  `icon.png` gets a tile whether or not its compose says so, which is the CasaOS
  layout every store already ships.
- The icon is **copied into the app folder at install** and refreshed on update
  (see [`app-model.md`](./app-model.md)), so an installed tile keeps working after
  the store is refreshed, removed, or was never on this box at all.

---

## The web-UI URL

**An app that declares [`routes`](#routes) is opened from its first route**, under the
box's routing setting: `https://<name>-<primary domain><path>` behind a proxy, the
external template's address, or — in ports mode — its published port, from the host
the dashboard is open at. Its `webui-*` fields, if it still carries them, are not
read. What follows is how an app *without* routes is opened.

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
| `${domain}` / `${APP_DOMAIN}` | the deployment's base domain | `.env.app`'s `APP_DOMAIN` |

`${APP_DOMAIN}` is the spelling the rest of an app's compose (Caddy labels) uses, so
a `webui-host` copied from a `caddy_N` label resolves as-is. Any other `${…}` left in
the host makes the URL unresolvable.

- Resolution happens on **every render**, so the URL tracks a domain change and
  works for **unmanaged/discovered** apps Maison never installed — nothing is
  stored.
- If `webui-host` references `${domain}` but the deployment has no domain
  configured (`APP_DOMAIN` empty), the URL is treated as **unresolvable**: the tile
  shows the "no reachable address" hint rather than a broken link.
- A literal `webui-host` (no placeholder, e.g. `nas.example.com`) is used verbatim.

### Examples

**Gateway app** (behind Caddy — the common case):

```yaml
x-compose-app:
  webui-host: jellyfin-${domain}
  webui-path: /web/
# APP_DOMAIN=app.localhost  →  https://jellyfin-app.localhost/web/
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

**Extra buttons** (`links` — parsed, not yet rendered):

```yaml
x-compose-app:
  webui-host: photoprism-${domain}
  links:
    - name: { en_us: Docs, fr_fr: Documentation }
      url: https://docs.photoprism.app
```

### Routes

`routes` declares how the app is reached without naming a domain, a proxy or TLS:
which service answers, on which port, under which name. The box's routing setting
turns each route into proxy labels on every one of its domains, a published host
port, or an entry in the route table an external router reads;
[`routing.md`](./routing.md) is authoritative.

```yaml
x-compose-app:
  routes:
    - service: outline         # first route = the web UI: the tile and APP_URL
      name: outline
      upstream-port: 80
    - service: outline-dex
      name: outline-auth       # → ROUTE_OUTLINE_AUTH_URL / _HOST
      upstream-port: 5556
```

| Key | Default | Meaning |
|---|---|---|
| `service` | the app's only service (required when it has several) | The service serving the port — the one the labels or the `ports:` entry go on. |
| `name` | the app's id | The host's leading label (`outline` in `outline-<domain>`), and the `<NAME>` in `ROUTE_<NAME>_URL`. A lowercase DNS label; required on every route but the first. |
| `upstream-port` | — | The port **inside** the container. Not `webui-port`, which is the click URL's port. |
| `path` | `/` | Where the web UI opens (`/webui/`): the click URL's path, never a dispatch rule. |
| `upstream-scheme` | `http` | `https` for an app that terminates TLS itself. |
| `insecure-upstream` | `false` | Skip verifying the upstream's certificate (self-signed). Only with `https`. |

**One route is one name, one service and one port.** Dispatch by path across
services, an auth check in front of a UI, a header rewrite: the app ships its own
router (an nginx service) and declares that router's port as its route — see
`in-app-router` in the store's `CONTRIBUTING.md`. A field is added here only when an
official app needs it and at least two unrelated proxies have the concept.

**An app is told its own address.** Maison seeds `APP_URL`, `APP_HOST` and
`ROUTE_<NAME>_URL` / `_HOST` into the app's `.env` before `files` and steps render, so
a listing writes `URL: ${APP_URL}` rather than `https://outline-${APP_DOMAIN}`. See
[`routing.md`](./routing.md#the-apps-own-address).

Non-HTTP listeners (BitTorrent, SMB, WireGuard) are not routes: they stay as
`ports:` in the compose. `maison lint` checks routes, including that the first
route's port is in its service's `expose:`. `routes` does not raise
`schema_version`: a build that predates it generates nothing, so a label-free app is
unreachable there but otherwise intact.

---

## Requirements

What an app needs from the box it runs on. Two kinds, and they behave differently on purpose:

```yaml
x-compose-app:
  architectures: [amd64, arm64]
  requires: [public-ip]              # public-ip | gpu-nvidia | gpu-dri
  resources:
    min:         { cpu: 2, memory: 2G, disk: 10G }
    recommended: { cpu: 4, memory: 8G }
```

| Kind | Means | In the store | Installing |
|---|---|---|---|
| **architecture** not listed | the images do not run here | hidden | refused, always |
| **capability** in `requires` the box lacks | the app installs and starts, then **cannot work** — a game server nobody can join | hidden (shown greyed with *Show all*, or when searched for) | after an explicit acknowledgement |
| **resource** below `min` | it works badly, or not at all under load | greyed, with the reason | after the same acknowledgement |
| **resource** below `recommended` | it works, slower | a note on the card | as usual |

**Capabilities.**

- `public-ip` — inbound connections from the internet reach the box directly, so a
  published raw TCP/UDP port (BitTorrent, WireGuard, a game server, a libp2p swarm) can be dialled.
  A box behind NAT, a CGNAT or the mesh tunnel gets its HTTPS routes from the gateway but has no
  such address. Any app that **publishes a host port on every address** needs it; `maison lint`
  says so (a port bound to one named address — a VPN's — is not for the internet and is exempt).
- `gpu-nvidia` — the Docker daemon has the `nvidia` runtime, so a `deploy.resources.reservations.devices`
  GPU request can be honoured. Without it such a container does not start at all.
- `gpu-dri` — the host has a render node (`/dev/dri/renderD*`) to hand an app for video
  acceleration. An app that merely runs *better* with one (a media server transcoding on the CPU
  otherwise) should not list it: `requires` is for what the app cannot work without.

**Resources.**

- `cpu` is a number of cores, compared to the host's logical core count.
- `memory` is compared to the box's total memory. The OS reports a little under the nominal
  size, so a box within 10% of the figure is not called short.
- `disk` is the free space on the data volume at install time.
- Sizes are written with a unit, binary as in a compose memory limit: `512M`, `2G`, `1.5GiB`.
  A bare number is refused, since `memory: 2` is far more likely a forgotten unit than two bytes.
- State `recommended` when you know what the app wants. State `min` only for a floor the app
  really has — below it, installing asks the operator to confirm.

**How the box is judged.** Maison detects every capability and the operator can override each one
(*auto / yes / no*) in **Settings › App stores** — see [`config.md`](./config.md#capabilities).
`public-ip` is *yes* when the address the deployment states (`APP_PUBLIC_IP`) is carried by a host
interface, as on a VPS, and *no* when it is not, as behind a home router or the tunnel. A home
server with forwarded ports is the case the *yes* override is for. A capability Maison could not
determine **never hides an app**: a store that silently empties because a probe failed is worse than
one that shows the app with a note.

**Updates are never refused** for a requirement, not even an architecture: the app is already
running on this box, and an architecture list is the maintainer's claim, not a manifest check. The
update carries a warning instead. An installed app that requires a capability the box no longer has
raises the `app.requirement` incident (see [`incidents.md`](./incidents.md)).

Neither key raises `schema_version`: a build that predates them lists and installs the app as it
always did. `x-casaos` has no such keys; a translated CasaOS block reads its `needs-public-ip` tag as
`requires: [public-ip]`.

## Reserved keys Maison writes

Two keys in an *installed* app's **override** are Maison's own bookkeeping rather
than author fields. They are listed here so a store author doesn't reuse the names,
and so an operator reading the file knows what they are:

| Key | Written by |
|---|---|
| `store-ref` | The install, recording where the app came from — `<store>/-/<folder>/<app id>` — so it can be updated later; rewritten when the Update tab points the app at another store. (`store` / `store-app-id` / `store-apps-path` are the superseded three-field spelling, still read, never written.) |
| `generated-routes` | Routing: what Maison wrote into the override for the box's routing setting — label keys, `ports:<entry>` values, `networks:<key>` attachments — and will delete before rewriting. See [`routing.md`](./routing.md). |

An app **without** `routes` that carries its own `caddy_N` labels is still routed on
the box's further domains: Maison copies its **Caddy route group** from the primary
domain onto each of them (clone mode), so it is reachable at all of them with no
extra field here. That is the other reason such an app keeps `webui-host` and its
`caddy_0` label the same string.

---

## The stack-up sequence

The declarative sections hang off one sequence, which **every** `docker compose up`
Maison runs goes through — install, start from the tile, store update, and saving
the app's config all take the same path:

```
data_version check → sync routing (override) → sync .env → seed the app's address
  → folders → secrets → variables → [migrations] → setup steps → seed → files
  → docker compose up -d --remove-orphans → started steps
```

The data-version check refuses to start an app whose data is in another shape than
its compose expects (an update checks instead that the data is where its plan
starts). `[migrations]` runs only during an update that crosses a data version —
see [`migrations.md`](./migrations.md).

**An install is this same sequence.** Nothing runs only on install: work for a
fresh install is a `run: once` [step](#steps). The installer adds only what
surrounds it — write the compose, `.env`, `.seed` and `.scripts`, ensure folders,
pull the images (the services' and those the steps declare):

```
write compose + .env + .seed + .scripts  →  ensure folders  →  pull images
                                         →  [ the up sequence ]
```

So a directory declared under `folders` is guaranteed to exist before an image is
pulled, before any step runs, and before the containers start — on the first boot
*and* on every boot after it. The same is true of every secret, seed file and
declared file: the sequence is a **converge**, not an installer, and it runs in
full on every start.

Everything before `compose up` is **fatal** on failure, and that is the point (a
`.env` sync failure is the exception: logged and reported as an incident). It
presumes the block parsed — a [malformed block](#a-malformed-block) skips these
steps instead of failing them. The shell hooks this replaced were not: a hook whose
`openssl` was missing wrote an empty secret, exited 0, and left an app that looked
installed.

[`lifecycle.md`](./lifecycle.md) is authoritative for these sequences, and covers
what the other operations (start, restart, update, save, uninstall) do with them.

---

## The seed tree

**Most first-run setup needs no declaration at all.** A store app ships a `seed/`
folder whose tree **mirrors the app directory**, so a path in the store *is* the
path on disk:

```
Apps/Caddy/seed/Caddyfile                    → /DATA/AppData/caddy/Caddyfile
Apps/Caddy/seed/www/index.html               → /DATA/AppData/caddy/www/index.html
Apps/Tuwunel/seed/element/config.json.tmpl   → /DATA/AppData/tuwunel/element/config.json
```

Nothing in `x-compose-app` mentions those files. The declaration was never
information — it was the shape of a folder.

At install Maison copies the store's `seed/` into the app folder as `.seed/`, for
the same reason it copies `docker-compose.yml` byte-for-byte: after that the app
folder stands on its own ([`app-model.md`](./app-model.md)), the backup archives
it, and a re-ensure works whether or not the store is still configured. An update
refreshes `.seed/` from the same store sync that refreshes the compose.

| Rule | Detail |
|---|---|
| `.tmpl` is rendered, and the suffix is stripped | Interpolated with the app's variables and its `.env`, exactly like `folders` paths. `$$` is a literal `$`. |
| Everything else is copied byte-for-byte | Binary-safe: icons, SQL dumps, scripts. |
| **Create-if-absent, on every up** | Never clobbers what the app or the operator has written. The accepted cost: a seed file deleted on purpose comes back on the next start. |
| Files land as `PUID:PGID`, `0644`, dirs `0755` | The exec bit survives, so an init script arrives runnable. Anything else is what [`files`](#files) is for. |
| Directories are created, but their **ownership** comes from `folders` | One place declares a directory's identity. |
| `docker-compose.yml`, `docker-compose.override.yml`, `.env` are refused | They are the app's identity, not its data. |
| Symlinks, escaping paths, and two entries writing one target are refused | A store is a downloaded archive. |
| An **unresolved `${VAR}` fails the up** | The old form of this failure wrote the literal `${VAR}` into the config and started the app. |

---

## Secrets and variables

A generated secret does not need its own plumbing. It needs to be **an ordinary
`.env` variable** — after which compose reads it, the seed renderer interpolates
it, and a hand-run `docker compose up` in the app folder sees exactly what Maison
sees.

```yaml
x-compose-app:
  secrets:                                  # values are ALWAYS generator specs
    SEARXNG_SECRET: hex:32
    DEX_PASSWORD_HASH: bcrypt:${APP_DEFAULT_PASSWORD}
  variables:                                # values are ALWAYS templates
    OUTLINE_URL: https://outline-${APP_DOMAIN}
```

```
seed/searxng/settings.yml.tmpl:   secret_key: "${SEARXNG_SECRET}"
```

Two sections rather than one because a bare map value cannot say whether it is a
literal or a generator. Splitting them by intent removes the question, and lets
`secrets` carry its own semantics: a generated value is never written to a log or
an install event.

| Generator | Produces |
|---|---|
| `hex:N` | N **bytes**, hex-encoded — `hex:32` is 64 characters, matching `openssl rand -hex 32` |
| `base64:N` | N bytes, base64 (standard alphabet), matching `openssl rand -base64 N` |
| `alnum:N` / `password:N` | N characters from `[A-Za-z0-9]` |
| `uuid` | a random (v4) UUID |
| `bcrypt:TEXT` | bcrypt of the rendered TEXT — replaces a `docker run … dex hash` |

- **Secrets are generated once and then stable.** A key already holding a value is
  reused, never regenerated: a signing key that rotates behind an app's back is
  indistinguishable from data loss. Pinning one by hand in `.env` is supported and
  survives. Rotation is an explicit act.
- **A `bcrypt:` spec may hash a sibling** — `ADMIN_HASH: bcrypt:${ADMIN_PASSWORD}`
  beside `ADMIN_PASSWORD: password:24`. Hashes are generated after every other
  secret, so the sibling exists even on a fresh install.
- **Variables are re-rendered on every up**, so a derived value follows a domain
  change instead of freezing at install time.
- Both are written key-by-key into the app's `.env`, the same path `.env.app` uses,
  so unrelated lines an operator added survive.
- **The dashboard hides and locks them.** In an app's Config › Env, a declared
  secret — and the variable a declared credential's `password` is — shows as
  `••••` and is read-only. The server never sends the value and keeps the one on
  disk whatever a save sends, a dropped row included, so the editor cannot regenerate
  a secret by accident. Change a login password through
  [`credentials[].change`](#credentials). A key the app does not declare but whose
  name reads like a credential (`*PASSWORD*`, `*TOKEN*`, `*_KEY`, …) is hidden too,
  but stays editable. The resolved Compose view masks the same values.
- Do **not** write a generator inside `${…}`. `docker compose` interpolates this
  file too, and `${random.hex(32)}` fails as an invalid interpolation before Maison
  ever reads it.

### Seeded secrets — moving an installed app off a shared value

```yaml
x-compose-app:
  secrets:
    DB_PASSWORD: ${APP_DEFAULT_PASSWORD}|alnum:32
```

Alternatives separated by `|`; the first one with a value wins, and the last must be
a generator. Each `${NAME}` before it is looked up in the app's `.env` **as it stood
before this install, update or start** — not in `.env.app`, not in the current file.

| The app | Its prior `.env` | `DB_PASSWORD` becomes |
|---|---|---|
| already installed, being updated to this listing | holds `APP_DEFAULT_PASSWORD` | that password — what its database was initialised with |
| freshly installed | none | 32 generated characters, its own |
| reinstalled over a restored backup | the restored one | the restored password |

After that it is an ordinary secret: generated (or seeded) once, never replaced.

This is the whole migration for a listing that wired an internal secret to the
box-wide password: point the service at `${DB_PASSWORD}` and declare the seed. A
password the app consumed once — an admin account, a database, an encryption key —
cannot simply change under an installed app; this keeps the old value where data
depends on it and stops handing it to every new install.

- A seed must be exactly `${NAME}` — no default, no surrounding text. Anything else
  is read as one ordinary spec, so `bcrypt:${APP_DEFAULT_PASSWORD}` is unaffected.
- A seed with no value is not an error: that is the fresh install.
- A seeded value is a secret like any other: it is never written to a log, an
  install event, or an error.

> `secrets` writes plaintext into the app's `.env`. The file is `0600` and owned by
> Maison's user (see [`app-env.md`](./app-env.md#who-can-read-it)), so it is out of
> reach of other apps and file shares, not of root. It removes a class of *silent
> failure*; it is not a secrets manager and does not pretend to be one — the app
> folder must keep working with a plain `docker compose up -d`.

---

## Credentials

An app's initial login, stated so Maison can **show** it, instead of a tip telling
the operator to go and find `APP_DEFAULT_PASSWORD`.

```yaml
x-compose-app:
  secrets:
    ADMIN_PASSWORD: password:20               # generated once, per app
  credentials:
    - label: { en_us: Administrator }         # localized
      username: admin                         # template
      password: ${ADMIN_PASSWORD}             # template
      note: { en_us: Change it under Settings → Security. }   # optional, localized
```

- `username` and `password` are **templates**, rendered exactly like `tips`: against
  the app's `.env`, which already carries the deployment's `.env.app` keys. So an app
  can declare `password: ${APP_DEFAULT_PASSWORD}` today and move to a per-app secret
  later without touching the UI.
- An unresolved `${VAR}` stays visible as written, as in tips; an entry with neither
  username nor password after rendering is dropped. A list rather than one entry,
  because some apps ship two accounts.
- An override's `credentials` **replaces** the store's list as a whole.

**What the operator sees.** Clicking the tile of an app that declares credentials
opens a dialog first — username and password with copy buttons, the password masked
until revealed, and **Open app**. Ticking *Don't show again* makes the next click open
the app directly; the tile menu's **Credentials** entry shows them again at any time.
The choice is a marker file, `.credentials-dismissed`, in the app's own folder, so
uninstalling clears it and a reinstall — which may come with a freshly generated
password — shows the dialog again.

**These are *initial* credentials** unless the entry declares a
[`change`](#changing-a-credential). Maison only displays what the compose declares;
it does not know about a password the operator later changes inside the app, and the
dialog says so.

**Where the values travel.** Never in the app list: `/api/apps` and its websocket
channel carry only `has_credentials` / `credentials_pending`, because the list is
cached in the browser's `localStorage`. The values come from
`POST /api/apps/{id}/credentials/show` (a POST with `Cache-Control: no-store`, so the
response is never a history entry or a prefetch); `PUT /api/apps/{id}/credentials/dismissed`
with `{"dismissed": bool}` records the choice. Like `secrets`, this removes a class of
friction, not the need for a trusted network: the values are the same ones the app's
`.env` already holds.

> **Successor to `APP_DEFAULT_PASSWORD`.** One box-wide password shared by every app
> is on its way out. The path is: declare `credentials` pointing at
> `${APP_DEFAULT_PASSWORD}` (no behaviour change, the dialog works at once), then move
> the app to its own `secrets` entry. Moving an *already installed* app needs care —
> its database was initialised with the old password, and a newly generated secret
> would be shown but would not work. Declare the secret
> [seeded](#seeded-secrets--moving-an-installed-app-off-a-shared-value)
> (`ADMIN_PASSWORD: ${APP_DEFAULT_PASSWORD}|password:20`): an installed app keeps — and
> shows — the password it has, and a fresh install gets its own.

### Changing a credential

A credential the operator can replace from Maison, in the app settings' **Security**
tab: a **Change password** form with a **Generate** button. The Credentials dialog
only shows credentials, it never changes them. The motivating case is an app whose
password lives only in its environment (Samba): without this, changing it meant
editing `.env` by hand.

```yaml
x-compose-app:
  secrets:
    SAMBA_ADMIN_PASSWORD: ${APP_DEFAULT_PASSWORD}|password:24
  credentials:
    - label: { en_us: SMB share }
      username: admin
      password: ${SAMBA_ADMIN_PASSWORD}
      change:
        apply: env                 # env | script — required
        policy:                    # all optional
          min_length: 8            # default 8
          max_length: 64           # default 128
          charset: printable       # printable (default) | alnum
          require: [lower, upper, digit]   # any of lower, upper, digit, symbol
          forbid: ""               # characters refused on top of Maison's own
```

**`apply` is how the new value reaches the app** — the one thing Maison cannot
guess, so the listing states it:

| `apply` | The app… | What Maison does |
|---|---|---|
| `env` | reads the variable on **every** start | writes `.env`, recreates the app, waits until it is running (and healthy, where it has a health check). If it does not come back, the previous `.env` is restored and the app recreated on it. |
| `script` | keeps its own copy (a user database) but has a way to change it | runs the [script entry](#the-script-entry) against the **running** app first; only if it succeeds writes `.env` and recreates. A failed script changes nothing. Once it has succeeded the app holds the new password, so a failed recreate keeps the new `.env` rather than reverting to a password the app no longer accepts. |
| *(no `change`)* | read the value once, at first boot | nothing — the credential is initial-only. Writing `.env` would make the dialog show a password the app does not accept. |

```yaml
      change:
        apply: script
        script: |
          OC_PASS="$ADMIN_PASSWORD" compose exec -e OC_PASS -u www-data nextcloud \
            php occ user:resetpassword --password-from-env admin
```

`change` is a [script entry](#the-script-entry) in every respect — `script` or
`file`, `images`, `timeout` (default `2m`), the same runner, command set and
environment as a step — plus two variables: the secret's own name holding the
**new** value (it overrides `.env`), and `OLD_<NAME>` the previous one. Pass the
value through the environment (`-e NAME`) rather than on a command line. The app
must be running; Maison refuses otherwise.

**Only an app's own secret can change.** `password` must be exactly `${NAME}` and
`NAME` a key under [`secrets`](#secrets-and-variables); anything else is shown as not
changeable. In particular `${APP_DEFAULT_PASSWORD}` is box-wide and re-synced from
`.env.app` on every up, so it cannot change for one app — moving the app to its own
[seeded secret](#seeded-secrets--moving-an-installed-app-off-a-shared-value) is what
makes it changeable.

**Derived secrets follow.** Every other secret whose generator reads the changed one
(`ADMIN_HASH: bcrypt:${ADMIN_PASSWORD}`) is emptied and regenerated from the new value
on the same up. `run: once` steps are **not** re-run: a `once` step that created the
account would fail the second time — an app that needs more than a restart to take a
new password uses `apply: script`.

**The policy applies to typed and generated values alike**, and the server enforces it
— the form only previews it. On top of whatever the app says, Maison always refuses
whitespace, control characters, anything outside printable ASCII and
`` $ ' " \ ` # ``: the value is written raw into `.env`, where compose interpolation,
quoting and comments would each misread one of them. **Generate** keeps the length of
the secret's own `password:N` / `alnum:N` when the policy allows it, always includes
each required class, and draws symbols only from `-_.!@%+=,:`.

**Values never travel back.** `POST /api/apps/{id}/credentials/{index}/generate`
returns a candidate (`no-store`, nothing written); `POST …/{index}/change` with
`{"password": …}` applies it — `400` with `rules` (`too_short`, `too_long`, `charset`,
`forbidden`, `require_<class>`) for a refused value, `409` while the app is updating,
being backed up, or (for `script`) stopped. Errors and logs are redacted of both the old
and the new value, because a script's or an up's error carries its output. `index` is the
entry's position in the declared list, which `credentials/show` returns with each
entry alongside its `change` (apply, normalized policy, and whether it can be offered).

Technical secrets — database passwords, signing and encryption keys — have no change
form. Rotating one is app-specific at best (a database password must change on both
sides) and data-destroying at worst (an encryption key), so it is not a generic
operation.

---

## Files

The escape hatch beside the seed tree, for the two things a folder of files cannot
say: a file that must be **re-rendered on every up**, or one that needs a
non-default owner or mode.

```yaml
x-compose-app:
  files:
    - path: /DATA/AppData/${AppID}/element/config.json
      from: element/config.json.tmpl      # a path in the app's seed tree
      ensure: always                      # tracks ${APP_DOMAIN} instead of freezing it
    - path: /DATA/AppData/${AppID}/db/init.sh
      from: db/init.sh
      mode: "0755"
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | required | Absolute host-spelled path under the data root. Same three declaration errors as `folders`. |
| `from` | string | — | A path inside the app's seed tree; `.tmpl` is rendered. |
| `content` | string | — | An inline body, always rendered. Exactly one of `from` / `content`. |
| `ensure` | `once` \| `always` | `once` | `always` re-renders on every up. |
| `user` / `group` / `mode` | | `${PUID}` / `${PGID}` / `0644` | As `folders` — **quote the octal**. |

A path claimed by a `files` entry is skipped by the seed tree, so nothing is
written twice. An `always` file whose content has not changed is left alone, so a
converge does not churn its mtime.

---

## Steps

The work no declaration expresses — the app's own binary seeding its database, a
call to the running app, another program's output to wait for and patch, the host —
is a **step**: a [script entry](#the-script-entry) with a name and a place in the
[stack-up sequence](#the-stack-up-sequence).

```yaml
x-compose-app:
  steps:
    - name: init-db
      at: setup                 # setup | started — required
      run: always               # once (default) | always
      script: |
        [ -e "$APP_DIR/db/database.db" ] && exit 0
        docker run --rm --user "$PUID:$PGID" -v "$APP_DIR/db:/db" \
          filebrowser/filebrowser:v2.63.23 config init --database /db/database.db

    - name: admin-hash
      at: setup
      script: |
        hash=$(printf 'admin:YaCy:%s' "$YACY_ADMIN_PASSWORD" | md5sum | cut -d' ' -f1)
        echo "YACY_ADMIN_HASH=$hash" >> "$MAISON_OUTPUT"   # for seed/…/yacy.conf.tmpl

    - name: configure
      at: started
      file: scripts/configure.sh  # the store app's scripts/configure.sh
```

| `at` | Runs | On failure |
|---|---|---|
| `setup` | Before the stack starts: after `secrets`, `variables` and any migrations, before the seed tree and `files` — so what it writes to `$MAISON_OUTPUT` renders into them. | **Fatal**: the app does not start, on every up until it is fixed. Nothing flaky belongs here. |
| `started` | After `docker compose up` returns. The containers have *started*; the app may still be booting, so a step waits for what it needs. | Logged and reported as an incident; the other started steps still run, and the step is retried on the next up. |

| `run` | |
|---|---|
| `once` (default) | Until it succeeds once, remembered in `.init/<name>` inside the app folder. The marker travels with the app's backup, so a restored app does not re-seed a database it already has; an uninstall that keeps the data removes it. When the step must follow the *data* rather than the install, use `always` and guard on the data (`[ -e "$APP_DIR/…" ] && exit 0`). A `once` step added in a later version **does** reach existing installs. |
| `always` | On every up: install, start, update, config save. It must be idempotent. |

Steps run in the order they are declared, `setup` ones before the stack starts and
`started` ones after. Names must be unique: a `once` marker is keyed by name.
Restart and Stop run no steps — they are container-level, not an up.

**Step or [migration](./migrations.md)?** A step is safe to run against any
install at any time. A migration happens exactly once, at a version boundary,
transforms data, and has a backup behind it.

## Views

`view` says which of the dashboard's grids an app's tile belongs to. It is
presentation, not capability: no value changes what Maison will do to the app.
Whether it can be stopped, uninstalled or backed up is the app's own
[`lifecycle`](#lifecycle-what-the-operator-may-do-to-the-app) and
[`backup.skip`](#skipping-an-app-entirely) declaration, whatever grid it sits in.

| Value | The tile |
|---|---|
| `apps` (default) | The ordinary app grid, alongside everything else. |
| `service` | The **Services** grid: an app with no web UI. Usually [derived](#an-app-with-no-web-ui-is-a-service), not declared. |
| `system` | The **System** grid: the platform's own pieces. A category only. |

The dashboard's section heading becomes an **App / Services / System** switch,
showing only the grids that have something in them, and is a plain heading when
only the app grid does — a deployment with no system apps and no services looks
exactly as it did.

```yaml
x-compose-app:
  view: system
```

`view` does **not** raise `schema_version`. A Maison that predates it ignores the
key and renders the app in the ordinary grid, which is where the app would have
been anyway — a version gate would turn a cosmetic hint into a refusal to start
the app at all.

Maison 1.1.5 up to the introduction of `lifecycle` derived a stop/uninstall guard
and a nightly-backup skip from `view: system`. A platform stack that must stay
guarded on such a build keeps `view: system` beside its `lifecycle` and
`backup.skip` keys; a current build reads only the latter.

An unrecognised value counts as no declaration rather than failing the app, in
keeping with "unknown keys are tolerated and skipped" — the app is sorted as if it
had said nothing (below).

There is no value for "no tile". `hidden` used to be one and is retired: a compose
that still says it is sorted like one that says nothing, so the app reappears in
the App or Services grid rather than staying invisible.

There is no `view` value for "extension of another app" — that is a relationship,
not a grid, and it is spelled [`parent`](#parent-makes-the-app-an-extension-of-another).

### An app with no web UI is a service

`service` is the one view Maison normally works out for itself. An app that
declares **no** `view` lands in the Services grid when nothing in its compose names
a web UI:

| Block | Counts as a web UI |
|---|---|
| `x-compose-app` | `webui-host`, or any `routes` entry |
| `x-casaos` (no `x-compose-app`) | `hostname`, `webui_port` or `port_map` |

A `webui-path`, `webui-port`, `index` or `scheme` on its own is not an address and
does not count — except that a CasaOS app names its address by port, so there
`port_map` and `webui_port` do. The answer comes from what the compose **declares**, never from
whether a URL resolved: a `webui-host` builds no URL until a domain is configured,
and that app is still an app.

A stack with no `x-compose-app` or `x-casaos` block at all (a compose brought up by
hand) declares nothing either way. There a published host port keeps it in the app
grid, clickable through that port as before, and no published port makes it a
service.

A **declared** `view` always wins, `apps` included: that is how a UI-less app stays
in the ordinary grid. An unrecognised value counts as no declaration.

A service is otherwise an ordinary app — like every view, it changes nothing about
what can be done to the app. Its tile is not greyed out: with
nothing to open, a click opens its settings. Maison also never turns a service's
published port into a click URL — that port is the non-HTTP listener the app
exists for (Samba's `445`), not a web page. A service that *does* resolve a URL
(an explicit `view: service` with a `webui-host`) opens it like any app.

A `system` tile with no URL is treated the same way: a platform piece with no UI of
its own (Maison's own tile) opens its settings on click instead of greying out.

### Lifecycle: what the operator may do to the app

```yaml
x-compose-app:
  view: system            # the grid — independent of the two keys below
  lifecycle:
    stoppable: false      # default true
    uninstallable: false  # default true
    updatable: false      # default true
```

Some apps take the operator's way back in with them when they go down: the
dashboard, the gateway in front of it, the login broker. They say so themselves:

| Operation | `stoppable: false` | `uninstallable: false` | `updatable: false` |
|---|---|---|---|
| **Stop** | Refused. The menu entry is withheld and the API answers `403`. Stopping the dashboard from its own tile takes the UI down with the click that asked for it, and leaves nothing running to bring it back. | — | — |
| **Update** | The app is **not stopped** first, is left out of **Update all**, and is updated only when named on its own (see [`lifecycle.md`](./lifecycle.md#update-all)). | — | **Outside the update system.** No row on Settings → Updates (not even under *Not linked to a store*), no Update tab, never queued by a run; checking, applying and setting the update source answer `403`. |
| **Uninstall** | — | Refused, the same way. | — |
| **Cleanup** | — | Its containers are never counted as orphans. | — |
| **Restart / Start** | **Allowed** — the stack comes back on its own, so it is how a wedged app is recovered without an SSH session. | Allowed. | Allowed. |

The three are independent: an app may be safe to stop but not to remove, or the
reverse.

`updatable: false` is for an app **something other than Maison keeps up to date** —
a platform stack its installer pins and upgrades (Maison itself, the mesh and auth
stacks, an admin app). Without it such a stack sits under *Not linked to a store*
forever, and the page offers to "link" it to whatever store app looks similar — a
link that would replace the platform's copy with the store's on the next update.
It says nothing about where the app's version comes from; that is its deployer's
business. Neither says anything about backups — an app that must not be stopped
nightly also declares [`backup.skip`](#skipping-an-app-entirely), since backing an
app up stops it.

This is the only guard; there is no operator-side list. An app is guarded because
**its own compose says so** — which is what lets a stack Maison merely
*discovered* be guarded too: Maison reads `x-compose-app` from the compose in the
directory Docker reports for the project, exactly as it does for the tile's icon
and name, and remembers the answer while the stack is stopped.

Being self-declared, it is a **foot-gun guard, not a security boundary**: Maison
has no authentication, and for a managed app the operator can edit the keys out
through Settings → override. That is deliberate — it is also the escape hatch for
an operator who genuinely means to remove a platform piece. Note that the override
merges `x-compose-app` **one level deep**: an override that declares `lifecycle:`
replaces the base's whole block, so `lifecycle: {}` there restores both defaults.

Whether a store app may declare either key is a store-review question; Maison does
not police it.

`lifecycle` does **not** raise `schema_version`, and its polarity is chosen for
that: a build that predates it (or a key of it) ignores a `false` and offers Stop,
Uninstall or Update, which is the ordinary behaviour of every app — a platform
stack on such a build is simply listed as *not linked to a store* again.

### `parent` makes the app an extension of another

Some apps are only meaningful beside a specific other app — a metadata agent for
a media server, a request front-end, a companion indexer. Given a top-level tile
each, a box with four of them reads as four unrelated apps.

`parent` names the app being extended, by its **Compose project name**:

```yaml
x-compose-app:
  parent: jellyfin
```

An app with a resolvable `parent` is an **extension**. It gets no top-level tile
of its own: the dashboard nests it under the parent's tile, and the store shows
it in an *Extensions* section on the parent's detail page rather than loose in
the catalog. There is no extensions grid and no `view` value for it — "which grid"
and "attached to what" are different questions, and only the second one has an
answer worth showing the operator.

Whether an app is an extension is the **maintainer's call**, exactly as `view`
is. Maison does not infer it from shared volumes, networks or naming.

#### An unresolvable `parent` is an ordinary app

`parent` is an unverified claim about *another maintainer's* app: the id may be
misspelled, may come from a store this box does not have, or may simply not be
installed yet. So the fallback is the same one an unrecognised `view` gets, for
the same reason — a cosmetic hint must never be able to make an app disappear:

| Situation | Result |
|---|---|
| Parent installed | Tile nests under it. |
| Parent absent, misspelled, or from another store | **Ordinary tile in the app grid.** No error, no hidden app. |
| Parent is itself an extension | `parent` ignored — ordinary tile. Nesting is **one level**; there are no chains. |
| `parent` names the app itself | Ignored, as above. |

An extension whose parent is later installed nests on the next refresh, and
un-nests if the parent is removed. Nothing about the app's own lifecycle
changes when that happens.

#### It is presentation, plus warnings

Like `view`, `parent` buys **no** behaviour. It is a declaration about
someone else's app, and the operations it invites — cascading stop, cascading
uninstall, a shared backup set — would each turn an unverified string into a way
to take down or delete data that belongs to a different app. So:

| Operation | On an extension / its parent |
|---|---|
| **Start / stop / restart** | Independent, both directions. Stopping the parent does not stop its extensions. |
| **Uninstall the extension** | Ordinary uninstall. |
| **Uninstall the parent** | Allowed. The dialog **lists the extensions that declare it** so the operator can see what is about to be orphaned — it does not remove them, and offers no checkbox that would. |
| **Backup** | Each app is backed up on its own schedule, into its own set. |
| **Install from the store** | Allowed with the parent missing; the store says which app it extends and flags that it is not installed. Never refused — the parent may be reachable some other way. |

Cascades can be added later on evidence that the relationship is reliable. They
cannot be walked back after one has removed an app folder the operator wanted.

#### `parent` is not `category`

`category` groups an app for **browsing the store** and is free-form store
metadata. `parent` places a tile on **this box's dashboard** and names a specific
installed app. An extension still carries whatever `category` its maintainer
chose; it is simply not shown as a top-level tile while its parent is there.

Like `view`, `parent` does **not** raise `schema_version`: a Maison that predates
it ignores the key and renders an ordinary tile — which is exactly where an
unresolvable parent lands anyway.

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

## Backup exclusions

Maison backs up `AppData/<app>` whole. Apps keep large **regenerable** data inside
that same folder — thumbnails, transcodes, search indexes, downloaded models — and
without a way to say so, every byte of it is copied on every backup and stored
offsite forever. On a media app the derived tree can rival the real data.

`backup.exclude` is how the app's author says which directories those are. It sits
next to `folders` for a reason: what a backup leaves out is exactly what the next
`up` has to recreate.

```yaml
x-compose-app:
  schema_version: 2
  backup:
    exclude:
      - cache/                                   # <app folder>/cache and everything under it
      - data/transcodes/                         # nested
      - "**/thumbs/"                             # a directory of that name at any depth
      - /DATA/AppData/${AppID}/models            # the absolute spelling folders: uses
```

| Form | Matches |
|---|---|
| `cache/` | the directory `cache` **at the app folder's root**, and its whole subtree |
| `data/transcodes/` | the same, nested. A trailing slash is optional |
| `"**/thumbs/"` | a directory named `thumbs` at **any** depth, including the root |
| `/DATA/AppData/${AppID}/models` | interpolated, then normalised to `models/` — accepted **only** inside this app's own folder |

Everything else is refused, one entry at a time: negations (`!keep`), `..`,
absolute paths outside the app's folder, bare `*`, and file globs such as `*.log`.
The grammar is deliberately this small because **two independent backup engines have
to implement it identically** — the local one walks the folder itself, kopia pushes
ignore rules into its repository — and an app whose backup contents depend on which
engine is selected is worse than one that cannot express a clever pattern.

A refused entry is **dropped, not fatal**. The app keeps every entry that parsed, and
the backup carries the refused path instead of skipping it — more data, never less.
The refusals are shown in the app's Backups tab, which is the only way a typo in a
store app ever gets noticed.

### `**/` must be quoted

```yaml
- "**/thumbs/"   # ✅
- **/thumbs/     # ❌ `*` is YAML's alias indicator — this fails to parse, and it
                 #    takes the WHOLE compose file with it, not just this entry.
```

### What is excluded is not restored

A restore replaces the app folder with the backup's contents, so an excluded
directory comes back **empty, not stale** — the app rebuilds it, which is the whole
premise of declaring it. Declare the directory in `folders` as well if the app needs
it to exist, with the right owner, before it starts.

This is also why an exclusion is a **declaration and never a heuristic**: Maison will
not guess that a directory looks like a cache, because a wrong guess drops real data
from a backup silently and the loss surfaces only during a restore. If a path is
excluded, it is because the app's author said so.

### The one exclusion Maison adds itself

Maison's own app folder contains the local archive tree
(`AppData/maison/.backups/`, see [`app-model.md`](./app-model.md)). Maison excludes
that tree from the backup of whichever app's folder holds it — otherwise the staging
directory would sit inside the folder being copied, and the size estimate would report
every archive on the box as that app's own data.

It is not an exception to the rule above: it is a structural fact about two paths, not
a guess about what a directory contains, and it applies to exactly one folder on a box.
It shows up in the backup dialog's excluded list like any declared entry. Restoring or
uninstalling that app is refused outright, because no exclusion can make either safe.

### Where it does not apply

The local engine's **uninstall** archive keeps everything. Uninstalling with a backup
is a folder rename there — instant, and the exclusions would cost work to apply
rather than save it — so that archive is a superset. A repository engine's uninstall
snapshot honours the exclusions like any other.

---

## Skipping an app entirely

`backup.skip` is the whole-app form of the same declaration: there is nothing in this
folder worth keeping.

```yaml
x-compose-app:
  schema_version: 2
  backup:
    skip: true
    exclude:
      - "**/cache/"
```

It is **not a bigger `exclude`**. An app that excluded everything would still be
stopped, snapshotted and restarted every night to produce an empty backup; a skipped
app is not a target at all:

| | Skipped app |
|---|---|
| Nightly run | not a target — `Scheduler.skip` |
| Back up now, in the Backups tab | refused, `403`; the tab says why |
| Backup before an update | not taken; the update proceeds and says so in the log (an update that converts data is refused) |
| Restore | **works normally** |
| Uninstall archive | still taken — see below |

**Restore is deliberately untouched.** An app can be marked skipped while backups
taken before the declaration still exist, and refusing to put those back would turn a
tidying-up declaration into data the owner cannot reach.

**The uninstall archive is still taken.** Uninstalling is the owner saying "remove
this, but keep a copy in case"; that is their decision at that moment, not the app
author's, and on the local engine it is a folder rename that costs nothing.

### It is not `view: system`

`view: system` used to decide, from one value, that the app tiled with the platform,
that Maison refused to stop or uninstall it, and that the nightly run skipped it. So
an ordinary app with nothing worth keeping had to pose as a platform component, and a
platform component had no way to ask to *be* backed up. Now `view` is only the grid,
[`lifecycle`](#lifecycle-what-the-operator-may-do-to-the-app) is the stop/uninstall
guard, and this is the backup decision — a platform stack that must not be stopped
nightly declares `skip` like any other app.

Backing a platform piece up *without* stopping it (a live, single-pass snapshot) is
not offered yet. It would be one more key in this block, the app's call like the rest.

It is also not the per-app opt-out an owner gets in the UI. This is the author saying
there is nothing here; that would be the owner saying they do not want it.

### Why there is no way to say `skip: false`

The field defaults to false and nothing can set it back to true-by-default, which is
the same forward-compatibility argument the rest of this block rests on: a build that
predates the field ignores it and backs the app up anyway — more data, never less. A
default-on flag an older build could ignore into *not* taking a backup would fail in
the other direction, which is not a trade this file makes anywhere.

---

## The script entry

Every piece of app-supplied code Maison runs is a **script entry**, whether it is a
[step](#steps), a [migration step](./migrations.md#migration-steps) or a
[credential change](#changing-a-credential). Where it is declared decides only
*when* it runs. Its keys, its environment and what it can do are the same in all
three places.

| Key | |
|---|---|
| `script` | Inline bash. |
| `file` | Instead of `script`: a `.sh` under the store app's `scripts/` folder (`file: scripts/patch.sh`). The folder is copied into the app folder as `.scripts/` on install and update, from the same store sync as the compose. For anything long enough to want its own file and `shellcheck`. |
| `images` | Images the script runs, pulled with the app's own images before anything is stopped. Optional; a migration should list them, since it runs after the stop. |
| `timeout` | Go duration. Defaults: step `10m`, migration `2h`, credential change `2m`. |

| In | Its own keys | Runs | On failure | Extra variables |
|---|---|---|---|---|
| `steps` | `name`, `at`, `run` | every up | `setup`: the app does not start. `started`: logged, retried | — |
| `migrations` | `id`, `name` (or `blocked`) | once, in the update that crosses `id` | the update is rolled back | `MIGRATION_ID`, `DATA_VERSION_FROM`, `DATA_VERSION_TO` |
| `credentials[].change` | `apply: script`, `policy` | when the owner changes the password | nothing changes | `<NAME>` (the new value), `OLD_<NAME>` |

### Where a script runs

`bash -euo pipefail` **inside the Maison container**, with the working directory set
to the app's folder, talking to the **host** Docker daemon
(`DOCKER_HOST=unix:///var/run/docker.sock`). Strict mode is what makes a failure
loud: `x=$(missing-tool)` fails the script instead of leaving an empty value behind.
(A translated CasaOS `pre-install-cmd` / `post-install-cmd` is the one exception:
it runs as the plain `bash -c` it was written for.)

The environment holds:

| Variable | |
|---|---|
| everything in the app's `.env`, and the [system variables](./app-env.md) | so a script sees the same values its compose does |
| `AppID` | the app's id |
| `APP_DIR` | the app folder, as the **host** spells it |
| `DATA_ROOT` | the data folder, as the host spells it |
| `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE` | the app's compose project — what `compose` (below) and a bare `docker compose` act on |
| `MAISON_OUTPUT`, `MAISON_ENV` | the two files a script hands values on through (below) |
| the extra variables of where it is declared | see the table above |

The script text is **not** interpolated or rewritten: write `$VAR`, never `$$VAR`.
Avoid `${VAR:?}`, which `docker compose` rejects when it reads the file.

### Paths

`$APP_DIR` and `$DATA_ROOT` are **host** paths, and they are valid **inside the
script too**. A `docker run -v "$APP_DIR/db:/db"` is resolved by the host daemon,
which needs the host's spelling. A `[ -e "$APP_DIR/db/x" ]` runs in Maison's
container. When a deployment mounts the data folder at a different path than the
host's (Unraid: `/mnt/user/appdata/maison` → `/DATA`; Docker Desktop), Maison
creates the host path inside its container at startup as a link to its data mount,
so one spelling works on both sides. Write `$APP_DIR/…`, not `/DATA/AppData/<app>/…`.

**Ownership.** A file written from the script's own shell belongs to Maison's user.
Appending (`>>`) keeps a file's owner; `sed -i`, `>` on a new file and `mv` do not.
Edit a file the app owns from a container run as `--user "$PUID:$PGID"`, or inside
the app with `compose exec`. A directory the app needs is declared under
[`folders`](#folders), not `mkdir`'d.

### Talking to the app: `compose`

`compose` is `docker compose` bound to the app's own project: `compose exec <service>
…`, `compose restart <service>`, `compose ps`. `exec` never allocates a TTY (a script
has none) but passes stdin, so `printf … | compose exec database psql …` works.
Name services, not container names.

### Running a tool in an image

A tool Maison's container does not carry runs in a pinned image. Pass values with
`-e`, never by splicing them into an inner script, and quote the heredoc so nothing
is expanded twice:

```bash
docker run --rm -i --entrypoint sh --user "$PUID:$PGID" \
  -v "$APP_DIR/config:/out" -e CRAFTY_ADMIN_PASSWORD \
  registry.gitlab.com/crafty-controller/crafty-4:4.10.7 -s <<'SH'
set -e
printf '{"password":"%s"}' "$CRAFTY_ADMIN_PASSWORD" > /out/default.json
SH
```

### Handing values on: `$MAISON_OUTPUT` and `$MAISON_ENV`

A script writes `KEY=value` lines to either file:

| File | Reaches | Lifetime |
|---|---|---|
| `$MAISON_OUTPUT` | later scripts of the same operation (as variables), the seed tree's `.tmpl` files and `files` | this up only — recomputed next time |
| `$MAISON_ENV` | the app's `.env`, so the compose and every later up | kept. **Adds a key only when the `.env` lacks it** — a user edit, an empty setting the user cleared, or a generated keypair is never overwritten |

Both work in all three places; where nothing renders after the script (a
credential change), `$MAISON_OUTPUT` simply has no reader. Assign a command
substitution to a variable before echoing it: `set -e` catches a failed
`hash=$(…)`, not a failed `echo "$(…)"`.

### The command set

A script may call these, and nothing else:

```
bash sh docker compose
cat chmod chown cp cut date dirname echo env expr find grep head id install
ln ls md5sum mkdir mktemp mv od printf readlink realpath rm rmdir sed seq
sha256sum sleep sort stat tail tee test timeout touch tr uniq wc wget xargs
```

Plus bash's own builtins. Anything else **fails the script** with a message naming
the sanctioned alternative, even when the call sat inside `"$(...)"`. The list is
the entries of `/opt/maison/hookbin`; it is a public contract, so entries get added
but not removed.

Two kinds of command are deliberately outside it, and the reason is the same in
both cases — without the guardrail they fail *silently*:

- **Not present in the image** — `openssl`, `curl`, `python`, `jq`, `git`, `unzip`.
  Run the tool in a pinned image: `docker run` fails loudly.
- **Present but scoped to the wrong machine** — `sysctl`, `ip`, `mount`, `adduser`,
  `modprobe`, `chroot`, `reboot` and ~25 other busybox applets act on *the
  container*, whose namespaces and user database vanish on restart. Use a recipe
  below.

### Reaching the host

Host access is legitimate and supported, through the Docker socket the script
already holds. Pin an image tag, and justify the access in the app's
`rationale.md`.

| To change | Recipe |
|---|---|
| Kernel parameters (`sysctl`) | `docker run --rm --privileged --network=host <image> sysctl -w <key>=<value>` |
| Network state (`ip`, `route`) | `docker run --rm --privileged --network=host <image> ip ...` |
| Files, users, `/etc` | `docker run --rm -v /:/host <image> chroot /host sh -c '...'` |
| Filesystems, devices | `docker run --rm --privileged -v /:/host <image> chroot /host mount ...` |
| Kernel modules | `docker run --rm --privileged <image> modprobe ...` |

`--privileged` and `--network=host` are **both** required for `sysctl`: the first
for a writable `/proc/sys`, the second because `net.*` keys are per-namespace.
Host *service* management (`systemctl`, `snap`) has no verified recipe yet.

> Nothing here is a security boundary. A script holds the Docker socket, which is
> root on the host by construction; the command set is an **authoring aid** that
> turns silent mistakes into loud ones, not a sandbox.

### CasaOS install commands

A store app that carries only `x-casaos` gets its `pre-install-cmd` as a `setup`
step and its `post-install-cmd` as a `started` step, both `run: once` (markers
`casaos-pre-install` / `casaos-post-install`), run without strict mode. Beside an
`x-compose-app` block they never run.

---

## Precedence

There is no field-by-field precedence between the two blocks: Maison reads one or
the other, decided by the base compose.

```
base has x-compose-app  →  x-compose-app            (+ override x-compose-app)
otherwise               →  x-casaos, translated     (+ override x-compose-app)
neither, no override    →  no metadata: published host port, or a service
```

Within the chosen block, the click URL is the `webui-host` URL when it resolves;
without a `webui-host`, the box's own address on `webui-port` — overridden by the
container's actual published host port when it has one.

## Minimum viable block

The smallest `x-compose-app` that changes Maison's behavior is just the host:

```yaml
x-compose-app:
  webui-host: myapp-${domain}
```

Everything else has a default or is absent. A store app should also carry `title`
and `icon` — without them the store card and the tile show the folder name and no
icon, whatever its `x-casaos` block says.
