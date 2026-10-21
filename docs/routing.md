# Routing

A store listing says what an app needs to be reachable: which service answers on
which port, under which name. It never says how. How is the box's: the **routing
setting** picks one of three renderers, and Maison writes what that renderer needs
into the app's `docker-compose.override.yml` and `.env` before every start.

```yaml
# the listing: no labels, no domain, no TLS
x-compose-app:
  routes:
    - { service: outline,     name: outline,      upstream-port: 80 }
    - { service: outline-dex, name: outline-auth, upstream-port: 5556 }
```

| Renderer | What it does | What Maison writes |
|---|---|---|
| `ports` | Each route's port is published on the host and opened as `http://<address>:<port>`. No proxy, no TLS: the host port speaks whatever the container speaks. | a `ports:` entry per route |
| `labels` | Each route is served by a reverse proxy on every domain of the list, with labels a *dialect* spells (Caddy today). | one label group per route per domain |
| `external` | Nothing is published here; an operator's own router reads the route table. | only the network attachment below |

The route fields are in [`x-compose-app.md`](./x-compose-app.md#routes). The first
route is the web UI: the tile opens it and the app's `APP_URL` comes from it.

## The setting

```jsonc
// <state>/settings.json — edited in Settings › Routing, or managed by the deployment
"routing": {
  "renderer": "labels",
  "dialect": "caddy",
  "domains": [
    { "name": "gateway", "domain": "{name}-${APP_DOMAIN}",
      "labels": { "import": "gateway_tls" } },
    { "name": "nip",     "domain": "{name}-${APP_PUBLIC_IP_DASH}.nip.io",
      "labels": { "import": "gateway_tls" } },
    { "name": "sslip",   "domain": "{name}-${APP_PUBLIC_IP_DASH}.sslip.io" }
  ]
}
```

| Key | Renderer | Meaning |
|---|---|---|
| `renderer` | all | `ports`, `labels` or `external`. |
| `bind` | ports | Host address ports are published on. Default `0.0.0.0`. |
| `url-host` | ports | The address apps are told they live at (`APP_URL`). Empty means the address the dashboard was first opened at. |
| `port-range` | ports | Where ports below 1024 and taken ones go. Default `[20000, 29999]`. |
| `dialect` | labels | The proxy's label grammar. `caddy` (the default and the only one today). |
| `domains` | labels | The domains apps are published on. **The first is the primary**: tiles open there and `APP_URL` names it. Never empty. |
| `url-template` | external | The address of a route, e.g. `https://{name}.home.lan`, for `APP_URL` and the tile. |

A domain's `domain` is a **host template**: `{name}` is the route's name, and the
rest stays templated in the generated label for Compose to interpolate, so a route
follows the box when its IP changes. A host written without `{name}` reads as
`{name}-<host>`, which is what every entry meant before the placeholder existed.

A domain's `labels` are added to every route on it, and win on a key collision; an
**empty value deletes the key**. TLS lives here, not in the app: on a Yundera PCS the
gateway and nip.io hosts carry `import: gateway_tls` (the deployment's own CA), while
sslip.io carries nothing and falls through to Let's Encrypt. A response header that
belongs to every host — HSTS — goes here too.

Two domains where one's hosts end with the other's are refused: a stale route on
the shorter one could not be told apart from the longer one's.

### With nothing stored

| Deployment | Default |
|---|---|
| `.env.app` has `APP_DOMAIN` | `labels` / `caddy`, one domain: `{name}-${APP_DOMAIN}` with no labels. Every store listing that still routes itself is left exactly as it was. |
| no `APP_DOMAIN` (a standalone install) | `ports` with its defaults. |

### Managed by the deployment

A deployment states its routing once, through the `maison` CLI, and the setting is
then read-only in the dashboard ("Managed by …"):

```sh
printf '%s' "$ROUTING_JSON" | docker exec -i maison-app maison settings set routing --owner Yundera
docker exec maison-app maison settings release routing     # hand it back to the user
```

Setting or releasing it republishes every running app, as a save in the dashboard
does. See [`config.md`](./config.md#managed-settings).

## What gets written

### labels

For each route on each domain, the dialect renders one group into the override:

```yaml
# docker-compose.override.yml (Outline, the setting above)
services:
  outline:
    labels:
      # Maison: route outline on gateway
      caddy_0: outline-${APP_DOMAIN}
      caddy_0.import: gateway_tls
      caddy_0.reverse_proxy: "{{upstreams 80}}"
      # Maison: route outline on nip
      caddy_1: outline-${APP_PUBLIC_IP_DASH}.nip.io
      …
```

The Caddy dialect writes `reverse_proxy {{upstreams N}}`. A route with
`upstream-scheme: https` gets `{{upstreams https N}}`, pinned to HTTP/1.1 — over TLS
Caddy would otherwise negotiate HTTP/2 with the container, and websockets do not
pass over that — and, with `insecure-upstream: true`, certificate verification off.

**A listing with no `routes`** that still carries its own `caddy_N` labels is routed
the way Maison always routed it: every group on the primary domain is copied onto
each further domain (*clone mode*). The whole group is copied — a route can be a
`handle_path` tree — except its TLS keys, which belong to the domain.

**A host that is already routed is skipped**, whether the listing ships it or the
operator wrote it, compared on the *resolved* host: `${PUBLIC_IP_DASH}` and
`${APP_PUBLIC_IP_DASH}` are one address. That is what lets Maison run against a
store that still routes itself without publishing an app twice.

Indices are allocated after the highest one either file already uses.

### ports

```yaml
services:
  outline:
    ports:
      # Maison: route outline
      - "${APP_BIND}:${APP_PORT_OUTLINE}:80"
```

The port is chosen once and kept in the app's `.env` (`APP_PORT_<NAME>`); it is never
renumbered silently. The rules:

1. a port the app already has is kept;
2. otherwise the route's own container port, when it is 1024 or above and free;
3. otherwise the first free port in `port-range`. Ports below 1024 always go there —
   whichever app was installed first would otherwise own the short address.

*Free* means not published by another app's container and not kept in another app's
`.env`. Maison runs in a container and cannot see a host process bound outside
Docker, so `docker compose up` is the real check: when it reports a port already
allocated, Maison picks the next one, retries once, and raises an incident naming
the new address.

A port the listing publishes itself is left to it, and used as the route's address.

### external

Nothing for routing. The route table is served at `GET /api/routes` and kept in
`<state>/routes.json`, for the operator's own router to read.

### The app network

Under `labels` and `external`, every routed service is attached to the deployment's
app network (`${APP_NET:-pcs}`), so the proxy can reach it — unless the listing
already put it there, or gave it a `network_mode`. A service with no networks of its
own is attached to `default` as well, or naming a network would cut it off its
siblings.

## The app's own address

Apps write their public address into their own configuration (Seafile's
`SERVICE_URL`, Outline's `URL`, Dex's issuer). Maison seeds it into the app's `.env`
on every start, before `files` and steps render, so a listing writes
`URL: ${APP_URL}` and never names a domain:

| Variable | labels (PCS) | ports (home) |
|---|---|---|
| `APP_URL` | `https://outline-alice.nsl.sh` | `http://192.168.1.10:20000` |
| `APP_HOST` | `outline-alice.nsl.sh` | `192.168.1.10:20000` |
| `ROUTE_<NAME>_URL`, `_HOST` | each further route (`ROUTE_OUTLINE_AUTH_URL`) | same |
| `APP_BIND`, `APP_PORT_<NAME>` | — | what the `ports:` entries read |

`APP_URL` is an **origin**: `scheme://host[:port]`, no path and no trailing slash, and
the scheme's default port left out. Append a path to it (`${APP_URL}/seafhttp`). The
values are resolved, not templated, and rewritten on every start, so a new domain,
IP or port follows. A route whose address is not known (a domain variable the
deployment does not set) gets no keys: an unresolved `${APP_URL}` is reported by
compose, a blank one would silently route at nothing. `<NAME>` is the route name
upper-cased, with `-` turned into `_`.

## The override is shared, so generation is exact

The base compose is off limits: it is byte-compared against the store's on every
update check, so anything written there would read as a permanent "update
available". The override is the only legal target — and it is also **the operator's
file**.

So Maison records what it wrote in the override's `generated-routes` manifest — label
keys, `ports:<entry>` values, `networks:<key>` attachments — and the next run deletes
**exactly those** before writing the new set. Everything else in the file is left
untouched: it is patched through its YAML node tree, so comments and key order
survive. When nothing is left, the manifest goes, and the file with it if it held
nothing else.

> One deliberate exception: a label group in the override whose host sits on one of
> the further domains is treated as Maison's even if the manifest does not list it —
> that is how generation recovers if the manifest is edited away through the YAML
> view.

## When it runs

Routing is the first step of the **up sequence**, so every path into a stack —
install, start, store update, a config or `.env` save, a routing change — converges on
the same files. A label or a published port is read off the *container*, so a
routing change only reaches a running app through a recreate, which is what saving
the setting does: **Maison republishes every running app** (stopped ones pick it up
when they next start).

`docker compose up -d` run by hand in the app's folder brings up exactly what Maison
brings up: everything lives in the two files Compose loads by itself, and every
variable they reference is in the app's `.env`.

## The settings panel

**Settings › Routing** edits the setting. The preview under it is the generator's own
output for a sample app (`POST /api/settings/routing/preview`), so the panel carries
no copy of the rules above. The panel is served by `GET`/`PUT /api/settings/routing`
(`PUT null` goes back to the default; `409` while the setting is managed).

## Before the routing setting

Settings › Domains was a list of *additional* domains whose routes were cloned from
an app's own labels. On the first start of a Maison that has the routing setting, a
non-empty list becomes `labels` / `caddy` with the primary `{name}-${APP_DOMAIN}`
first and every old entry after it; a managed list keeps its owner. The old endpoint,
panel and CLI name are gone.
