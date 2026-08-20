# Router-neutral routing — proposal

> **Status: proposal. Nothing is implemented.** This asks for a go/no-go on a model
> change in `internal/caddyroutes`, `internal/domains` and the `x-compose-app`
> schema, plus a store-side migration that depends on it.

## What we are trying to fix

Four asks that turn out to be one change:

1. **Be closer to Docker Compose.** Maison's job is compose files; routing should
   be expressed in compose's vocabulary, not a proxy's.
2. **Caddy must not be Maison's business logic.** Maison will always speak Compose;
   it will not always sit behind Caddy.
3. **The primary domain should be like the others** — an entry in the domains list,
   with its own labels, generated into the app's override like every other domain.
4. **(Roadmap) Trim the store.** Simple apps should stop shipping `caddy_*` labels.
   `nsl.sh`, `sslip.io` and `nip.io` are deployment facts, not app facts, and have
   no business in a store definition.

(3) and (4) are the same requirement seen from two ends, and neither is reachable
without changing how routes are generated. (4) **cannot ship without (3)**: the
moment an app carries no `caddy_0`, something has to generate its route on
`${APP_DOMAIN}`, and that is exactly what (3) is.

## Where we stand today

Caddy is already well contained. `stackup`, `apps`, `config`, the manifest and the
override-patching are router-agnostic. Actual Caddy knowledge lives in three places:

| Location | What is Caddy-specific |
|---|---|
| `internal/caddyroutes` | the `caddy_N` / `caddy_N.sub` group grammar (`caddyKey`, `caddyIdx`, `routeGroups`), index allocation, and `domainOwned()` — which knows that `import` and `tls*` mean TLS |
| `internal/domains` | `Domain.Directives` — the name *means* "Caddy sub-directives" |
| `web/.../DomainSection.svelte` + i18n | "Caddy directives", "use `import = gateway_tls`" |

The blocking design choice is that **Maison clones, it does not render.** `generate()`
finds a route group in the base compose whose host contains `${APP_DOMAIN}` and copies
the group with the host swapped. That single decision causes both problems:

- Cloning is inherently proxy-shaped: to clone you must know which sub-key holds the
  host, which ones are TLS and must be stripped, and how indices are numbered.
- **Cloning needs a seed.** The primary route cannot be "just another entry" because
  there would be nothing to clone from — which is why the panel shows it read-only.

## The data

Every routed app in AppStoreLab (74 of them), classified by route shape:

| Shape | Count | Apps |
|---|---|---|
| host + `import` + `reverse_proxy: {{upstreams N}}` — nothing else | **69** | Outline, Jellyfin, everything |
| one upstream, decorated | 3 | Crafty, Hubs, PhotoBridge |
| structurally complex (path trees, `forward_auth`) | **2** | Seafile, ClaudeCode |

Multi-service apps — the case an earlier brainstorm treated as hard — are only 3:
Appium (2 hosts), Outline (2), Hubs (5). **Every individual host in them is still one
service and one port.** That case is not hard; it only looked hard because
`x-compose-app` has a *singular* `webui-host`. Make it a list and it dissolves.

The genuinely irreducible case is **2 apps out of 74**. Seafile dispatches four
upstreams by path with rewrites; ClaudeCode uses `forward_auth` with
`handle_response`. Their Caddy config *is* the app's routing logic. Any neutral
abstraction of that is Caddy with different spelling — which is how the last attempt
ended up "still Caddy-aware".

**Proposal: do not abstract those. Escape them.**

## The model

Maison owns the route's **identity**; the author owns the route's **body**.

### 1. The app declares routes, not labels

```yaml
x-compose-app:
  routes:
    - service: outline          # where the labels land; default = the only/main service
      name: outline             # subdomain prefix; default = the app id
      upstream-port: 80
    - service: dex
      name: outline-auth
      upstream-port: 5556
```

Note what is **absent**: no domain, no proxy, no TLS. The domain comes from the
settings list and is applied identically to every entry — so ask (3) falls out with
no privileged `${APP_DOMAIN}` anywhere in the model, and `webui-host` becomes
*derived* (`routes[0].name` + the primary domain) instead of authored. The "keep
`webui-host` and the caddy label the same string" convention in
[`docs/x-compose-app.md`](./docs/x-compose-app.md) deletes itself.

### 2. The deployment declares a dialect — as data, not Go

```yaml
# the caddy preset (default, swappable per deployment)
key:  "caddy_{n}"
host: "{key}: {host}"
body: "{key}.reverse_proxy: {{upstreams {port}}}"
```

```yaml
# traefik, written by an operator, no Maison release required
labels:
  "traefik.http.routers.{app}{n}.rule": "Host(`{host}`)"
  "traefik.http.services.{app}{n}.loadbalancer.server.port": "{port}"
```

Maison's logic becomes: *for each app × each route × each domain, render, reconcile
into the override, record in the manifest.* No proxy vocabulary in Go.

**Not a pure template — a pair.** "Is this host already routed?" requires reading a
host back *out* of a label. For Caddy the value *is* the host; for Traefik it is
inside `` Host(`…`) ``. So a preset is `{render, extractHost}`, where `extractHost`
is a key pattern plus a capture regex. This is the one place a naive template is not
enough; it should be designed in from the start rather than discovered.

### 3. Domains, including the primary

```jsonc
"domains": [
  { "name": "primary", "domain": "${APP_DOMAIN}",
    "labels": { "import": "gateway_tls" } },
  { "name": "nip",     "domain": "${APP_PUBLIC_IP_DASH}.nip.io",
    "labels": { "import": "gateway_tls" } },
  { "name": "sslip",   "domain": "${APP_PUBLIC_IP_DASH}.sslip.io",
    "labels": { "import": "" } }
]
```

`directives` is renamed to `labels` — compose's word, not Caddy's. `import:
gateway_tls` stops being a concept Maison ships and becomes *a string the operator
typed*, in **one** settings row instead of 66 store files.

### 4. The escape hatch: clone mode, minus the Caddy vocabulary

An app that ships a hand-written route group keeps it (the existing "already routed"
skip leaves it alone), and Maison clones that group onto the other domains — today's
behaviour, retained for the complex apps.

**`domainOwned()` is deleted** and replaced with one generic rule:

> The domain's labels win on key collision, and an **empty value deletes the key**.

`sslip` says `import: ""` → the cloned `import` is dropped. `nip` says `import:
gateway_tls` → it is replaced. Maison no longer knows what `import` *is*; it is just
a key. That is smaller than the rule it replaces and genuinely router-neutral.

**The accepted cost:** clone mode needs a *group grammar* (`caddy_{n}` +
`{key}.{sub}`), which the preset declares. A dialect without one supports the
rendered apps and not the hand-written ones. Stated plainly: **a complex app is bound
to its proxy and says so in its own file. Maison is not.** This has been accepted.

## Worked examples

### Outline — the 69-app case, and the multi-service case

```yaml
# before — 12 label lines, 3× repetition, hosting policy baked into the store
outline:
  labels:
    caddy_0: outline-${APP_DOMAIN}
    caddy_0.import: gateway_tls
    caddy_0.reverse_proxy: "{{upstreams 80}}"
    caddy_1: outline-${APP_PUBLIC_IP_DASH}.nip.io
    caddy_1.import: gateway_tls
    caddy_1.reverse_proxy: "{{upstreams 80}}"
    caddy_2: outline-${APP_PUBLIC_IP_DASH}.sslip.io
    caddy_2.reverse_proxy: "{{upstreams 80}}"
dex:
  labels:
    caddy_0: outline-auth-${APP_DOMAIN}
    …×3 again

# after — no labels, no proxy, no domains
x-compose-app:
  routes:
    - { service: outline, name: outline,      upstream-port: 80 }
    - { service: dex,     name: outline-auth, upstream-port: 5556 }
```

### Hubs — the worst multi-service case (5 hosts, 3 decorated)

Three of its five routes are `reverse_proxy: "https://{{upstreams 8080}}"` plus
`transport.tls` and `transport.tls_insecure_skip_verify`. That is **one concept**,
and a proxy-universal one (Traefik: `scheme: https` + `serversTransport.
insecureSkipVerify`; nginx: `proxy_pass https://`):

```yaml
x-compose-app:
  routes:
    - { service: hubs,        name: hubs,           upstream-port: 4001 }
    - { service: nearspark,   name: hubsnearspark,  upstream-port: 5000 }
    - { service: hubs-client, name: hubsassets,     upstream-port: 8080,
        upstream-scheme: https, insecure-upstream: true }
    - { service: spoke,       name: hubsspoke,      upstream-port: 8080,
        upstream-scheme: https, insecure-upstream: true }
    - { service: dialog,      name: hubsdialog,     upstream-port: 4443,
        upstream-scheme: https, insecure-upstream: true }
```

PhotoBridge needs one more of the same kind: `max-body: 10G`
(`caddy_0.request_body.max_size`).

**Decision for the architect:** three optional knobs — `upstream-scheme`,
`insecure-upstream`, `max-body` — move coverage from 69 to **71 apps** and reduce the
proxy-bound set from 5 to **3**. All three are concepts every reverse proxy has. The
proposed rule for holding the line: *a knob is admissible only if at least two
unrelated proxies have the concept; anything else is an escape, not a knob.*

Without them, Hubs and PhotoBridge use clone mode and are Caddy-bound. That is a
defensible answer too — it is 3 apps versus 5, against 3 new schema fields.

### Seafile — clone mode, unchanged

Its `caddy_0` group stays exactly as it is today: valid compose, 20 lines instead of
60 (one group instead of three hand-repeated ones). Maison clones it onto nip/sslip
and applies the domains' labels over the top. Seafile is Caddy-bound; its file says so.

## The store trim, and its one real hazard

### `webui_port` is not the upstream port — the upstream port has no home

Verified in code before writing this:

| Field | What it actually is | Used where |
|---|---|---|
| `x-casaos.webui_port` | the **container** port (CasaOS semantics) | `apps.go:499` → `reachableHostPort()`, matched against `p.Private` to choose a *published host port*. Only when `app.URL == "" && app.Hostname == ""` — the no-proxy fallback. **Never routing.** |
| `x-casaos.port_map` | the host port for the URL | `apps.go:462` |
| `x-compose-app.webui-port` | the **URL** port, appended as `:<port>`; empty = standard 443 | `xcomposeapp.go:257` |

So the upstream port lives in **exactly one place — `{{upstreams N}}` inside the
Caddy label — for all 74 apps.** There is no second source: `expose:` is a hint and
may list several ports, `webui_port` is a different concept and for `x-compose-app`
apps a different *number*.

Two consequences:

- **`routes[].upstream-port` is new data**, not a re-derivation. That is the honest
  justification for the field: a route needs a port and the store has never had a
  proxy-neutral place to say it.
- **The migration is not `yq delete`.** The script must *harvest* `{{upstreams N}}`
  on the way out, per app, per service.
- **Do not name the field `port`.** `routes[].port` sitting next to `webui-port` in
  the same block is a trap — the two numbers differ for essentially every gateway app
  (80 vs empty). `upstream-port` or `container-port`.

Fallback order, with the failure mode made loud:

```
routes[].upstream-port         ← authoritative
  else x-casaos.webui_port     ← only when there is no x-compose-app block
  else the single exposed port ← only when there is exactly one
  else: no route + a visible install-time error
```

The last line matters: an unresolvable port must fail visibly, not emit a label
pointing at nothing. Otherwise the trim produces apps that install cleanly and 502.

Side effect worth having: for a **vanilla, unmodified CasaOS store app** —
no `caddy_*`, no `x-compose-app` — `webui_port` genuinely is the container port, so
those apps become gateway-routable for the first time. Narrow (single obvious web
port only), but free.

### Version skew across the CDN — the sharpest edge

The store is a moving jsDelivr target consumed by **every** PCS, including those
running older Maison. The instant labels vanish from `@main`:

- `update.go` byte-compares each app's base compose against the store's → every app
  on every PCS reports "update available";
- a user on an older Maison clicks update → receives a compose with no routes and no
  generator → **the app becomes unreachable**, via a button we put there.

The mitigation is already in the architecture: `StoreURLs` is a list. **Trim into a
separate store URL** (`AppStore@v2` or a distinct CDN path) rather than in place on
`@main`. The old fleet keeps eating the old store indefinitely; only a Maison that
can generate routes subscribes to the new one.

**Open question:** how `store` / `store-app-id` in an installed app's override behave
when that app's store URL changes. That path has to be graceful before any cutover.

## Code impact

**Deleted:** `domainOwned()`; the `caddy_N` regexes as *hardcoded* constants (they
become preset data); the "keep `webui-host` and the caddy label in sync" convention.

**Kept unchanged — all already router-neutral, and the hard-won parts:**
manifest-exact deletion, YAML-node patching (comments and key order survive),
resolved-host identity via `envinject.Render`, "already routed → skip", index
allocation above both files, `prune`, override-only-never-base.

**Renamed:** `internal/caddyroutes` → `internal/routes`; `Domain.Directives` →
`Domain.Labels`; neutral i18n across en/fr/zh/de.

**Docs to follow:** [`docs/domains.md`](./docs/domains.md) and
[`docs/x-compose-app.md`](./docs/x-compose-app.md) both describe the clone model as
*the* mechanism and need rewriting alongside the code.

## Proposed sequencing

1. **Maison ships generation** — rendered routes, primary as a domain entry, clone
   mode retained for the complex apps. **The store is untouched and nothing changes
   for anyone**, because already-routed-skip means every existing app keeps its own
   labels and gets nothing generated. Safe to ship alone, and worth shipping alone.
2. **Convert the store into a new store URL**, script-generated from the labels being
   deleted so the ports come along. The complex apps keep their `caddy_0` group.
3. **Point new deployments at it**; cut the fleet over when comfortable.

Step 1 is what turns step 2 from a coordinated flag day into an ordinary store commit.

## Decisions requested

1. Go / no-go on **render-from-declaration** replacing clone-as-the-only-mechanism.
2. **The three knobs** (`upstream-scheme`, `insecure-upstream`, `max-body`): in, and
   coverage is 71/74 with 3 proxy-bound apps — or out, and it is 69/74 with 5?
3. **Preset shape**: confirm `{render, extractHost}` rather than a plain template.
4. **Store cutover**: new store URL vs. in-place trim on `@main` (this proposal
   argues strongly for a new URL).
5. Whether the primary domain row may be **removed** in the UI (a LAN-only box) or is
   pinned — it is also the app's click URL.
