# Router-neutral routing — proposal

> **Status (2026-10-21): step 1 done; step 2 implemented in Maison, not yet released;
> steps 3–4 not started.** The official store no longer carries a proxy-specific route
> tree: the five apps that had one ship their own router (store commits `663717b`,
> `e67ba1a`), tested on holyhorse and running on real boxes. Maison now has the routing
> setting, the three renderers, the Caddy dialect and the seeded `APP_URL` family
> (unstaged); [`docs/routing.md`](./docs/routing.md) describes what is built and is
> authoritative where this proposal and it differ.

## Goal

Maison stops knowing how apps are routed. A store listing says **what an app needs to
be reachable**; the deployment decides **how**. Three deployment shapes, equal citizens:

| Shape | Who routes | What Maison writes |
|---|---|---|
| **No domain** (the default, no config) | nobody: the browser hits a host port | `ports:` in the override, bound to a configurable address |
| **Label-driven proxy** (Caddy on a Yundera PCS / nsl.sh, Traefik) | the proxy, reading container labels | templated labels in the override |
| **External routing** (nginx, a hand-written Traefik file, a router on another box) | the operator's own config | nothing in compose; the resolved route table is published for them to consume |

The store has **no knowledge of the routing engine**: no labels, no domain, no TLS, no
proxy vocabulary.

## Decisions taken (2026-10-21)

1. **Anything beyond "host → one service:port" lives inside the app.** Path dispatch
   across services, an auth hook in front of the UI, rewrites: the app ships its own
   small router (an nginx service) and declares that router's port as its one route.
   It is the only answer that keeps every app installable in all three shapes, ports
   mode included. Validated on the two hardest listings, see below.
2. **Ports mode binds `0.0.0.0` by default**, with a visible warning when the box has a
   public IP.
3. **AppShield is out of scope.** A gate is a service of the app like any other; a route
   that points at it is an ordinary route. Where the app's router sits relative to its gate
   is the app's choice: **in front** when the gate must see only some paths (Vaultwarden),
   **behind** when every request is gated anyway (qBittorrent).

## The store: `routes`

```yaml
x-compose-app:
  routes:
    - service: seafile-router   # where the traffic goes; default: the only service
      name: seafile             # the app's name on the network; default: the app id
      upstream-port: 80         # the port INSIDE the container
      path: /                   # entry path: the click URL, not a dispatch rule
```

One route is one name, one service and one port. Nothing else is a route: non-HTTP
listeners (BitTorrent, SMB, WireGuard) stay as `ports:` in the listing, as today.

- **`upstream-port`, not `port`.** `webui-port` already exists and means the port in the
  click URL (empty behind a gateway on 443). The two differ for nearly every routed app.
- **`path`** covers the "open the UI at `/webui/`" case (Kubo) without a redirect rule.
- **Two optional fields**, for a backend that serves HTTPS itself: `upstream-scheme: https`
  (default `http`) and `insecure-upstream: true` (don't verify its certificate, which is
  self-signed). They cover Crafty and Hubs without a router. A field is added only when an
  official app needs it **and** at least two unrelated proxies have the concept; `max-body`
  was considered and left out, since no official app needs it and Caddy and Traefik don't
  cap bodies. Anything else is the app's own router's job.
- `webui-host` stops being authored: the click URL is derived from the first route and
  the deployment's rendering of it.

### The app must be told its own URL

Routing is not only labels. Apps bake their public origin into their own config:
Seafile sets `SEAFILE_SERVER_HOSTNAME=seafile-$APP_DOMAIN` and
`SERVICE_URL=https://seafile-$APP_DOMAIN`, and Seadoc and the notification server
do the same. That is the `{name}-{domain}` naming convention leaking into the store
through `environment:`, and it is wrong in ports mode (`http://192.168.1.10:20003`)
and on any deployment that names hosts differently.

So Maison resolves each route and seeds it into the app's `.env`, next to what
`.env.app` already provides:

| Variable | Example (labels, Yundera) | Example (ports) |
|---|---|---|
| `APP_URL` | `https://seafile-alice.nsl.sh` | `http://192.168.1.10:20003` |
| `APP_HOST` | `seafile-alice.nsl.sh` | `192.168.1.10:20003` |
| `ROUTE_<NAME>_URL` / `ROUTE_<NAME>_HOST` | one pair per extra route (`ROUTE_OUTLINE_AUTH_URL`) | same |

`APP_URL` is an **origin**: `scheme://host[:port]`, no path and no trailing slash, so a
listing appends its own path (`FILE_SERVER_ROOT=${APP_URL}/seafhttp`). There is no
`APP_SCHEME`: it is the start of `APP_URL`. The values are those of the first route on the
primary domain (the first entry of `routing.domains`); `<NAME>` is the route's `name`,
upper-cased with `-` turned into `_`.

The first route's values are the `APP_*` set. A listing then writes
`SERVICE_URL=${APP_URL}`, and `APP_DOMAIN` disappears from routing-related env. It
stays available for what is genuinely the domain (an e-mail address like
`admin@$APP_DOMAIN`).

## The in-app router (validated)

The pattern: add one nginx service to the app. It joins the app's private network and
the shared one; the platform routes the app's name to its port 80 and nothing else. The
path rules that used to be a Caddy `handle_path` / `forward_auth` tree move into its
config, which ships **inline in the listing** through compose's top-level
`configs: … content:` (supported by Compose 2.23+, already used by NNTmux). No seed file
and no new Maison feature are needed.

```yaml
services:
  seafile-router:
    image: nginx:1.28.0-alpine
    expose: [80]
    configs:
      - source: seafile-router
        target: /etc/nginx/conf.d/default.conf
    networks: [seafile-net, pcs]
configs:
  seafile-router:
    content: |
      # `$$` is compose's escape for nginx's own `$`
      …
```

### Results (2026-10-21)

| App | Router does | Checks vs the label-routed listing | Live |
|---|---|---|---|
| Seafile | path dispatch over 3 services | 39/39 identical | mestio |
| ClaudeCode | `auth_request` gate in front of ttyd | 33/33 identical | yunderalabs |
| ClaudeCodeRoot | same as ClaudeCode | not box-tested (host SSH over the private network to confirm) | — |
| Vaultwarden | `/admin` + `/nhl-auth` to the gate, the rest direct | 36/36 identical, browser-tested | holyhorse |
| qBittorrent | strips `Referer`/`Origin` on the tile's navigation, behind the gate | 9/9 identical + CSRF table, browser-tested | holyhorse |

Method, for Seafile and ClaudeCode: the official listings were installed through Maison (the baseline), then their
`docker-compose.yml` was replaced with the router version and the stacks recreated. The
same 72 checks ran on all three published hosts (`<app>-holyhorse.nsl.sh` through the
CF worker, `.nip.io`, `.sslip.io`). **All 72 results were identical**, and the router
access logs confirm the traffic went through them.

| Seafile (13.0.19) | ClaudeCode (1.0.26) |
|---|---|
| web login, `/api2/auth-token/` | `/` without a session → `302 /login` |
| 64 MiB upload through `/seafhttp`, 64 MiB download, SHA match | wrong password → 401; right one → session cookie |
| WebDAV `PROPFIND /seafdav` → 207 | `/` with the session → ttyd; `/ws` → 101 |
| notification websocket `/notification/` → 101 | `/mcp` → auth server; `/logout` then `/` → `302 /login` |
| SeaDoc `/socket.io/` polling + websocket → 101, `/sdoc-server/` | |

Side effect worth having: only the router sits on the shared network now. In the store
versions `claude:8080` (ttyd, which has no login of its own; the gate was Caddy's
`forward_auth`) and `seafile-notification-server` sat on `pcs`, so other containers could
reach them directly; in the router versions they don't resolve from `pcs` at all.
Seafile itself stays on `pcs` because the Seafile Virtual Folder app reaches
`seafile:8080/seafdav` over it.

### nginx checklist for a store author

Each line is a difference from Caddy's defaults that would break an app silently:

- `client_max_body_size 0;` and `proxy_request_buffering off; proxy_buffering off;`.
  nginx caps bodies at 1 MiB and buffers to disk; Caddy does neither.
- `proxy_read_timeout` / `proxy_send_timeout` raised (3600s) for long transfers and
  websockets.
- WebSocket: `proxy_http_version 1.1` plus `Upgrade` / `Connection` from a `map`.
- `X-Forwarded-Proto` **passed through** from the proxy in front, not `$scheme` (which
  is `http` inside); `Host $http_host`.
- Upstreams as **variables** with `resolver 127.0.0.11`, so nginx starts before its
  upstreams exist and re-resolves them when a container is recreated.
- A `rewrite … break` is how `handle_path`'s prefix stripping is expressed with variable
  upstreams.
- `absolute_redirect off;`, or nginx turns `return 302 /login` into an `http://` URL.
- `proxy_set_header` in a `location` **drops every inherited one**: restate `Host` and
  `X-Forwarded-*` in any location that sets its own (the `auth_request` subrequest).
- Caddy `forward_auth` ≙ `auth_request` + `proxy_method GET`, no body, `X-Forwarded-Method`
  and `X-Forwarded-Uri`, `error_page 401 = @login`.

The store's `CONTRIBUTING.md` now carries this checklist as the `in-app-router` check (plus
the client-IP rule: take the *last* `X-Forwarded-For` entry, never the first), and the five
listings are its reference implementations.

### Official store census (2026-10-21)

Apps whose routes are more than host + `import` + `reverse_proxy {{upstreams N}}`:

| App | What the labels do | Disposition |
|---|---|---|
| Seafile | path dispatch over 3 services | **done**: in-app router |
| ClaudeCode, ClaudeCodeRoot | `forward_auth` gate + ungated paths | **done**: in-app router (Root differs only by stripping `/mcp`) |
| Vaultwarden | `/admin`, `/nhl-auth/*` to the gate, the rest straight to the backend | **done**: in-app router in front of the gate |
| qBittorrent | strips `Referer`/`Origin` on tile navigation (CSRF check) | **done**: in-app router behind the gate |
| Crafty | HTTPS upstream, self-signed, websocket headers | knobs (`upstream-scheme`, `insecure-upstream`); the headers are Caddy defaults anyway |
| Hubs | HTTPS upstreams, self-signed | knobs |
| Kubo | `/` → `/webui/` redirect | `path: /webui/` |
| Nextcloud | adds an HSTS header | drop, or set it in the app |
| Ntfy | answers `/favicon.ico` with 204 | drop (cosmetic) |

The remaining five are expressible as plain routes once the schema exists; they wait for the
store trim (step 4).

Everything else is the plain shape, generated with no loss.

## The deployment: one `routing` setting

A Maison setting like smtp or domains, managed on a PCS through the `maison` CLI (the
template already does this for smtp and feedback):

```jsonc
"routing": {
  "renderer": "ports",              // ports | labels | external
  // ports
  "bind": "0.0.0.0",
  "url-host": "192.168.1.10",       // what goes in click URLs; bind 0.0.0.0 is not a URL
  "port-range": [20000, 29999],
  // labels
  "dialect": "caddy",               // a preset: data, not Go
  "domains": [
    { "name": "primary", "domain": "{name}-${APP_DOMAIN}", "labels": { "import": "gateway_tls" } },
    { "name": "nip",     "domain": "{name}-${APP_PUBLIC_IP_DASH}.nip.io", "labels": { "import": "gateway_tls" } },
    { "name": "sslip",   "domain": "{name}-${APP_PUBLIC_IP_DASH}.sslip.io" }
  ],
  // external
  "url-template": "https://{name}.home.lan"
}
```

The primary domain is an entry like the others: **the first entry is the primary** (it gives
the click URL and `APP_URL`), it can be reordered or replaced, and the list must keep at
least one entry. A LAN-only box uses the `ports` renderer or a LAN domain entry, not an
empty list. `{name}` puts the naming convention in the deployment rather than the store. `gateway_tls` becomes a string an operator
typed into one settings row, not something written into every store file.

## Renderers

All three produce one **route table** (app × route → name, upstream service:port,
public URL per domain or port), and everything else derives from it: the override, the
`APP_URL` family, the click URL and the external export.

### `ports`

- Each route gets `ports: ["${APP_BIND}:${APP_PORT_<ROUTE>}:<upstream-port>"]` in the
  override. The chosen host port is **saved in the app's `.env`**, so it is stable across
  restarts and a hand-run `docker compose up -d` matches Maison.
- **Allocation:** try the upstream port itself first (Jellyfin on 8096 reads well), else
  the next free port in the range. "Free" = not published by any container (Docker API)
  and not reserved in another app's `.env`.
- **Conflicts Maison cannot see:** it runs in a container and does not see host processes
  bound outside Docker. `docker compose up` is the real check: on `port is already
  allocated`, pick the next port, save it, retry once, and surface the change.
- **Never silently renumbered** once assigned; the Config tab shows the port and lets the
  operator change it.
- **The port is forwarded as it is.** Maison adds no TLS and no scheme handling: whatever
  the container speaks on its port (HTTP, or HTTPS for a route with
  `upstream-scheme: https`) is what the host port speaks. The click URL and `APP_URL` use
  `http://<url-host>:<port>`; for a route with `upstream-scheme: https` they use `https`,
  since that port only speaks TLS.
- **No special case for apps that need HTTPS** (WebAuthn, service workers, Vaultwarden's
  web vault). They install like any other; over plain HTTP the parts that need a secure
  context don't work, which the app's own tips can say. HTTPS is what the `labels` and
  `external` renderers are for.
- Warning when `bind` is `0.0.0.0` and the box has a public IP: every app is then on the
  internet, unauthenticated unless the app has its own login.

### `labels`

The generator the old proposal described, unchanged in substance:

- A dialect preset is a pair `{render, extractHost}`. "Is this host already routed" needs
  the host read back out of a label: for Caddy the value *is* the host, for Traefik it
  sits inside `` Host(`…`) ``.
- `domainOwned()` is deleted. A domain's labels win on a key collision, and an **empty
  value deletes the key** (`sslip` says `import: ""`). Maison stops knowing what `import`
  means.
- Writes into the override only, under the existing manifest, so it stays exact and
  never touches the base compose.

### `external`

- Maison writes nothing for routing. It keeps the routed service on `APP_NET`, and
  publishes the route table as `GET /api/routes` and as `routes.json` in its state
  directory, for the operator's nginx generator or sync script.
- Click URLs and `APP_URL` come from `url-template`.
- Later, and only if wanted: a file renderer (a text template → `<app>.conf` in a
  directory) for zero-glue nginx. It reads the same table.

## Coupling outside Maison: one source for the domains

Today the domain list is written in three places that must agree: the store labels,
the auth-registrar's (and each gate's) `REDIRECT_HOST_SUFFIXES`, and mesh-router-caddy's
root and catch-all routes. A domain added only in Maison gets routes, but its OIDC
callbacks are refused. With `routing.domains` as the deployment's single statement, the
mesh template writes it into Maison **and** derives the registrar's suffix list from the
same values. Otherwise the labels renderer ships with a feature that half-works.

## What stays from today's code

Kept as is: manifest-exact deletion, YAML-node patching (comments and key order
survive), comparing hosts after variable resolution (`envinject.Render`), skipping a
host that is already routed, allocating indices above both files, `prune`, writing only
the override.

**Clone mode becomes read-only legacy.** A listing (or a plain CasaOS store app) that
still carries its own `caddy_N` groups keeps working under the Caddy dialect: they are
cloned onto the extra domains as today. No new listing is written that way, and on the
`ports` and `external` renderers such an app gets nothing generated.

**Fallback for the upstream port**, when a listing has no `routes`:
`x-casaos.webui_port` (only when there is no `x-compose-app`), else the single exposed
port, else no route plus a visible install-time error. An app that installs cleanly and
502s is the failure to avoid.

## Store migration and version skew

- The upstream port lives today **only** inside `{{upstreams N}}`. The migration script
  must harvest it into `routes[].upstream-port` as it removes the labels; it is not a
  `yq delete`.
- Trimmed listings ship at a **new store URL**. Every PCS pulls the store from jsDelivr;
  trimming `@main` in place would make every installed app report "update available",
  and a Maison too old to generate routes would update an app into unreachability.
  Only a Maison that understands `routes` subscribes to the new URL.
- Open: how `store-ref` in an installed app's override behaves when the app's store URL
  changes. It has to be graceful before any cutover.

## Sequencing

1. ~~**Seafile, ClaudeCode(Root), Vaultwarden, qBittorrent move to in-app routers in the
   current store**, keeping a plain `caddy_0/1/2`.~~ **Done 2026-10-21** (`663717b`,
   `e67ba1a`). Independent of Maison; the store has no proxy-specific route tree left.
2. **Maison: route table + `APP_URL` family + the three renderers**, the primary domain
   as an entry. Store untouched, so nothing changes for anyone: the "already routed"
   skip leaves every labelled app alone.
3. **Mesh template**: sets `routing` through the CLI and feeds the registrar's suffixes
   from it.
4. **New store URL**, script-generated: labels → `routes`, `seafile-$APP_DOMAIN` →
   `${APP_URL}`. New deployments point at it; the fleet is cut over when comfortable.

## Settled questions (2026-10-21)

1. **Route fields:** `upstream-scheme` and `insecure-upstream`, no `max-body`. A new field
   needs an official app that uses it and two unrelated proxies that have the concept.
2. **Address variables:** `APP_URL` (an origin) and `APP_HOST`, plus `ROUTE_<NAME>_URL` /
   `ROUTE_<NAME>_HOST` for further routes. No `APP_SCHEME`.
3. **Primary domain:** the first entry of `routing.domains`; reorderable and replaceable,
   and the list is never empty on the `labels` renderer.
4. **HTTPS-only apps in ports mode:** no flag and no refusal. Ports mode forwards the
   container's port as it is and opens `http://` (or `https://` for an `upstream-scheme:
   https` route).

No open question blocks step 2.

## Settled while writing the worked example (2026-10-21)

1. **Ports below 1024** never go to the app that asks first: they come from the range.
2. **`APP_URL` drops a default port** (`http://box`, not `http://box:80`).
3. **Maison attaches a routed service to the app network** (labels and external), unless
   the listing already did; a service with no networks keeps `default` too.
4. **`APP_URL` is resolved**, rewritten on every start.
5. **The route table comes before `files` and steps**, so a config file can name the
   app's address (Dex's issuer).
6. **`ports:` entries are recorded by value** in the manifest.
7. **First route = web UI**, checked by `maison lint` (its port must be exposed).
8. **Internal and public addresses stay separate variables**; worth a line in the
   store's CONTRIBUTING.

And from the store census: the Caddy dialect **pins HTTP/1.1 for an HTTPS upstream**
(Crafty's websockets), and **HSTS belongs to a domain's labels**, not to one app.

One deviation from the plan as written: clone mode (a listing that still routes itself)
keeps stripping a copied group's TLS keys, as a Caddy-dialect rule, rather than relying
on "an empty domain label deletes the key". A migrated sslip.io entry has no
`import: ""`, and without the strip it would inherit the gateway's TLS. Rendered routes
start clean, so the empty-deletes rule is what applies to them.
