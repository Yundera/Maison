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
  icon: icon.png
  category: Media
  tagline: Free software media system
  description: |
    Jellyfin is a media server for organizing and streaming your collection.
  developer: Jellyfin
  screenshots:
    - screenshot-1.png

  # --- the click URL ---
  webui-host: jellyfin-${domain}
  webui-path: /web/
```

### Fields

| Field | Type | Required | Meaning / Maison use | `x-casaos` fallback |
|---|---|---|---|---|
| `schema_version` | int | no (default `1`) | Spec version (currently **2**). `secrets`, `variables`, `files`, `init` and the seed tree are **additive** and do not raise it — an older build ignores what it does not know instead of dropping the whole block (which would cost the app its folders and routes too). A build that predates the declared version falls back to `x-casaos` for the whole block — note that it **does not refuse to install the app**, so raising the version degrades an app rather than gating it. v1 files keep working. | — |
| `id` | string | no | Stable app identifier (should equal the Compose project `name`). Defaults to the project name. | `store_app_id` |
| `title` | string \| localized | no | Tile + store display name. | `title` |
| `icon` | path \| url | no | Tile icon. See [Assets](#assets). | `icon` |
| `category` | string | no | Store grouping. | `category` |
| **`view`** | `apps` \| `system` \| `hidden` | no (default `apps`) | Which dashboard grid the app's tile lands in — **not** a store category. `system` also makes the app **protected**. See [Views](#views). | — |
| `tagline` | string \| localized | no | One-line store summary. | `tagline` |
| `description` | string \| localized | no | Store long description (Markdown). | `description` |
| `developer` | string | no | Store attribution. | `developer` / `author` |
| `screenshots` | path[] \| url[] | no | Store gallery. See [Assets](#assets). | `screenshot_link` |
| `thumbnail` | path \| url | no | Store card image. See [Assets](#assets). | `thumbnail` |
| `architectures` | string[] | no | e.g. `[amd64, arm64]`. Advisory. | `architectures` |
| **`webui-host`** | string | **yes\*** | The web UI's **host** — the final URL host, templated exactly like the app's Caddy route (e.g. `jellyfin-${domain}`). Omit for headless apps. | derived from `hostname` |
| `webui-port` | string | no (default `""`) | The **URL** port, appended as `:<port>`. **Not** the container port — it exists only to build the URL and is empty in the common gateway case (standard 443). | derived from `port_map` |
| `webui-scheme` | `http` \| `https` | no (default `https`) | The URL scheme the **browser** uses. | `scheme` |
| `webui-path` | string | no (default `/`) | Path appended to the host. May include a query string (e.g. `/?hash=$AUTH_HASH`). | `index` |
| `links` | object[] | no | Extra buttons on the detail view: `{ name, url, icon? }` with an **absolute** `url`. Never the tile's default action. | — |
| `tips` | string \| localized | no | Guidance note (Markdown, `${VAR}` references resolved from the app's `.env`) shown from the tile menu. This is where Maison writes tips edited in **App settings** — into the **override's** `x-compose-app.tips`, never the store-provided base compose. When set, it replaces the `x-casaos` tips; clearing it falls back to them. | `tips.before_install` + `tips.custom` |
| **`folders`** | object[] | no | Directories **created and owned before every `up`**. See [Folders](#folders). |
| **`secrets`** | map | no | Values **generated once** and ensured in the app's `.env`. See [Secrets and variables](#secrets-and-variables). |
| **`variables`** | map | no | Templates ensured in the app's `.env` on **every** up. See [Secrets and variables](#secrets-and-variables). |
| **`files`** | object[] | no | Individual files Maison writes — the escape hatch beside the [seed tree](#the-seed-tree). See [Files](#files). |
| **`init`** | object[] | no | One-shot containers run around the app's stack. See [Init](#init). | — |
| **`hooks`** | object | no | `{ pre_install, post_install, pre_up, post_up }` — host shell around the app's lifecycle. See [Hooks](#hooks). | `pre-install-cmd` / `post-install-cmd` |
| **`backup`** | object | no | `{ exclude }` — directories of **derived** data to leave out of backups. See [Backup exclusions](#backup-exclusions). | — |

\* `webui-host` is required only to have a **clickable app**. An app with no
`webui-host` (and no `x-casaos` fallback) is headless — its tile has no "open"
action.

**Localized** means either a plain string (`title: Jellyfin`) or a locale map
(`title: { en_us: Jellyfin, fr_fr: Jellyfin }`). Maison prefers `en_us`.

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
| `store-ref` | The install, recording where the app came from — `<store>/-/<folder>/<app id>` — so it can be updated later; rewritten when the Update tab points the app at another store. (`store` / `store-app-id` / `store-apps-path` are the superseded three-field spelling, still read, never written.) |
| `generated-routes` | Route generation: the Caddy label keys Maison added to publish the app on the deployment's **additional domains**, and will delete before rewriting them. See [`domains.md`](./domains.md). |

Route generation is the other reason to keep `webui-host` and the app's `caddy_N`
label the same string. Maison clones the app's **Caddy route group** onto every
additional domain the deployment answers on (`sslip.io`, `nip.io`, …), so an app
that declares its route in labels is reachable at all of them with no extra field
here — and its click URL keeps mirroring the route it was cloned from.

---

## The stack-up sequence

The declarative sections hang off one sequence, which **every** `docker compose up`
Maison runs goes through — install, start from the tile, store update, and saving
the app's config all take the same path:

```
folders → secrets → variables → init(pre_up) → seed → files
        → pre_up → docker compose up -d → init(post_up) → post_up
```

`pre_install` / `post_install` bracket that sequence, but only the **first** time —
during the install itself:

```
write compose + .env + .seed  →  ensure folders  →  pull images
                              →  pre_install  →  [ the up sequence ]  →  post_install
```

So a directory declared under `folders` is guaranteed to exist before an image is
pulled, before any hook runs, and before the containers start — on the first boot
*and* on every boot after it. The same is true of every secret, seed file and
declared file: the sequence is a **converge**, not an installer, and it runs in
full on every start.

Everything before `compose up` is **fatal** on failure, and that is the point. The
shell version of this work was not: a hook whose `openssl` was missing wrote an
empty secret, exited 0, and left an app that looked installed.

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
- **Variables are re-rendered on every up**, so a derived value follows a domain
  change instead of freezing at install time.
- Both are written key-by-key into the app's `.env`, the same path `.env.app` uses,
  so unrelated lines an operator added survive.
- Do **not** write a generator inside `${…}`. `docker compose` interpolates this
  file too, and `${random.hex(32)}` fails as an invalid interpolation before Maison
  ever reads it.

> `secrets` writes plaintext into the app's `.env`, on a box that has no auth and
> whose `.env.app` already holds `APP_DEFAULT_PASSWORD` in the clear (see
> [`app-env.md`](./app-env.md)). It removes a class of *silent failure*; it is not
> a secrets manager and does not pretend to be one.

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

## Init

The one case that is not a declaration in disguise: work that needs *a container* —
the app's own binary seeding its database, or a tool computing a value.

```yaml
x-compose-app:
  init:
    - name: seed-db
      image: filebrowser/filebrowser:v2.63.2
      command: config init --database /db/database.db
      user: "${PUID}:${PGID}"
      volumes:
        - /DATA/AppData/${AppID}/db:/db
      when: absent:/DATA/AppData/${AppID}/db/database.db

    - name: obscure-pass
      image: rclone/rclone:1.73.3
      command: [obscure, "${APP_DEFAULT_PASSWORD}"]
      capture: RCLONE_PASS        # stdout → a variable the seed template can use
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required for `when: once` | Identifies the step in logs, and is what `once` is remembered by. |
| `image` | string | required | |
| `command` / `entrypoint` | list \| string | — | A string is split on whitespace, honouring quotes. It is argv — there is no shell, no expansion and no operators. |
| `user` / `environment` / `volumes` / `network` | | | The `docker run` surface. `environment` takes a list or a mapping. |
| `when` | `once` \| `always` \| `absent:<path>` | `once` | The guard, replacing every hand-written `if [ ! -f ]`. |
| `capture` | string | — | Binds trimmed stdout to a variable for the seed renderer and `files`, for this converge only. Not written to `.env` unless a `variables` entry asks. |
| `phase` | `pre_up` \| `post_up` | `pre_up` | `post_up` for a seeder that needs the app's own network or a running service. |

**Two path spellings, deliberately.** A `volumes` source is resolved by the *host
daemon*, so it is a host path — the same rule a hook's `docker run -v` lives under.
The path in `when:` is resolved by *Maison*, so it is container-side. Two different
processes resolve them; one spelling could not serve both.

`when: once` is remembered in `.init/<name>` inside the app folder, so it travels
with the app's backup: an app restored from an archive does not re-seed a database
it already has. A step that fails records nothing and is retried on the next up.

A failing `pre_up` step is **fatal**; a `post_up` step is logged and swallowed, for
the same reason the matching hooks are.

## Views

`view` says which of the dashboard's grids an app's tile belongs to. It is
presentation, not capability — with one deliberate exception, below.

| Value | The tile |
|---|---|
| `apps` (default) | The ordinary app grid, alongside everything else. |
| `system` | The **System** grid, and the app is **protected** (below). |
| `hidden` | No tile at all. |

The dashboard's section heading becomes an **App / System** switch as soon as one
app declares `view: system`, and is a plain heading otherwise — a deployment that
declares nothing looks exactly as it did.

```yaml
x-compose-app:
  view: system
```

`view` does **not** raise `schema_version`. A Maison that predates it ignores the
key and renders the app in the ordinary grid, which is where the app would have
been anyway — a version gate would turn a cosmetic hint into a refusal to start
the app at all. Note the consequence for `system`: on an older build the app is
also not protected.

An unrecognised value falls back to `apps` rather than failing the app, in
keeping with "unknown keys are tolerated and skipped".

### `view: system` protects the app

A system app is the platform itself — the dashboard, the gateway in front of it,
the host stack. Maison therefore refuses to take it down:

| Operation | On a system app |
|---|---|
| **Stop** | Refused. The menu entry is withheld and the API answers `403`. Stopping the dashboard from its own tile takes the UI down with the click that asked for it, and leaves nothing running to bring it back. |
| **Uninstall** | Refused, the same way. |
| **Restart** | **Allowed** — the stack comes back on its own, so it is how a wedged platform app is recovered without an SSH session. |
| **Start** | Allowed. |
| **Scheduled backup** | Skipped. Backing an app up stops it, and taking the gateway down nightly is not a backup strategy (see [`lifecycle.md`](./lifecycle.md)). |

This is the only protection mechanism; there is no operator-side list. An app is
a system app because **its own compose says so** — which is what lets a stack
Maison merely *discovered* be protected too: Maison reads `x-compose-app` from
the compose in the directory Docker reports for the project, exactly as it does
for the tile's icon and name.

Being self-declared, it is a **foot-gun guard, not a security boundary**: Maison
has no authentication, and for a managed app the operator can edit `view` out
through Settings → override. That is deliberate — it is also the escape hatch for
an operator who genuinely means to remove a platform piece.

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

### Where it does not apply

The local engine's **uninstall** archive keeps everything. Uninstalling with a backup
is a folder rename there — instant, and the exclusions would cost work to apply
rather than save it — so that archive is a superset. A repository engine's uninstall
snapshot honours the exclusions like any other.

---

## Hooks

**Reach for a hook last.** Everything above — the seed tree, `secrets`,
`variables`, `files`, `init` — exists because the work these hooks were doing was
declarative all along, and expressing it as shell cost correctness: an undeclared
command set, host-vs-container path rewriting, hand-rolled idempotence, and values
frozen at install time. What is left for a hook is genuinely imperative work, such
as waiting on another program to write its own config and then patching it.

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
      # openssl is NOT available to hooks — run it in a pinned image.
      # See "The command set" below for why.
      docker run --rm alpine:3.20 openssl rand -hex 32 \
        > ${DATA_ROOT}/AppData/${AppID}/secrets/key
    pre_up: |
      docker pull ghcr.io/example/sidecar:1.4.2
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

> **A hook does not run on the host.** Older store documentation said it did. It
> never has — the CasaOS fork Maison replaced ran hooks in its own container too.
> What changed is that Maison now says so out loud instead of letting a hook
> quietly act on the wrong machine.

The distinction that matters: a hook's **shell** is containerised, but its
**`docker` client is not** — every container it starts is a real container on the
real host. That is the seam every recipe below goes through.

#### The command set

A hook may call these, and nothing else:

```
bash sh docker
cat chmod chown cp cut date dirname echo env expr find grep head id install
ln ls md5sum mkdir mktemp mv od printf readlink realpath rm rmdir sed seq
sha256sum sleep sort stat tail tee test timeout touch tr uniq wc wget xargs
```

Plus bash's own builtins. Anything else **fails the hook** with a message naming
the sanctioned alternative. The list is the symlinks in `/opt/maison/hookbin`; it
is a public contract, so entries get added but not removed.

Two kinds of command are deliberately outside it, and the reason is the same in
both cases — without the guardrail they fail *silently*:

**Not present in the image** — `openssl`, `curl`, `python`, `jq`, `git`, `unzip`.
A missing command inside a command substitution is not an error in bash:

```bash
SECRET="$(openssl rand -hex 32)"     # -> ""   and the hook still exits 0
```

Two shipped apps wrote empty secrets this way and installed green. Run the tool in
a pinned image instead — `docker run` fails loudly:

```bash
SECRET="$(docker run --rm alpine:3.20 openssl rand -hex 32)"
```

**Present but scoped to the wrong machine** — `sysctl`, `ip`, `mount`, `adduser`,
`modprobe`, `chroot`, `reboot` and ~25 other busybox applets. These exist in the
container and appear to work, but act on *the container*, whose network namespace,
mount namespace and user database all vanish on restart. Use a recipe below.

#### Reaching the host

Host access is legitimate and supported. It is not a workaround — it is the
mechanism, and it goes through the Docker socket the hook already holds. Pin an
image tag, and justify the access in the app's `rationale.md`.

| To change | Recipe |
|---|---|
| Kernel parameters (`sysctl`) | `docker run --rm --privileged --network=host <image> sysctl -w <key>=<value>` |
| Network state (`ip`, `route`) | `docker run --rm --privileged --network=host <image> ip ...` |
| Files, users, `/etc` | `docker run --rm -v /:/host <image> chroot /host sh -c '...'` |
| Filesystems, devices | `docker run --rm --privileged -v /:/host <image> chroot /host mount ...` |
| Kernel modules | `docker run --rm --privileged <image> modprobe ...` |

`--privileged` and `--network=host` are **both** required for `sysctl`: the first
for a writable `/proc/sys`, the second because `net.*` keys are per-namespace.

Host *service* management (`systemctl`, `snap`) has no verified recipe yet — a
`chroot` has no systemd to talk to. Don't rely on it from a hook.

> Nothing here is a security boundary. A hook holds the Docker socket, which is
> root on the host by construction; the command set is an **authoring aid** that
> turns silent mistakes into loud ones, not a sandbox. An absolute path
> (`/bin/sysctl`) still bypasses it, and that is fine — the point is to catch the
> author who did not know, not the one who insists.

#### Paths

Because hooks are aimed at the host daemon, `/DATA` and `${DATA_ROOT}` in a hook's
script are rewritten to **host** paths — a `docker run -v` must name a path the
host daemon can resolve. The consequence is the one trap worth knowing:

> A hook that just wants a directory to exist should **not** `mkdir` it. Written in a
> hook, that path is a host path, and the `mkdir` would run in the Maison
> container — creating the wrong directory in the wrong place. Declare it under
> `folders` instead: those are created through Maison's data mount and are correct
> on both sides. The same trap applies to a hook that writes a *file* into `/DATA`:
> use the [seed tree](#the-seed-tree) or [`files`](#files), which are written
> container-side and are correct on both.

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
