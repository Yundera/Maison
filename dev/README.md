# Maison — dev stack ("closer to prod")

The default `docker-compose.yml` at the repo root runs Maison **alone** on a
plain `mesh` bridge. That is enough to browse the store and install ordinary
apps, but it can't test **SSO-enabled** store apps: those apps expect a **Caddy
router** to route them by their `caddy_*` labels and a **Dex** OIDC provider to
log users in. This `dev/` stack adds exactly those pieces, wired the same way a
production PCS (`yundera-root/packages/template-root`) wires them.

## What's in the stack

| Service | Image | Role |
|---|---|---|
| `maison` | built from `..` | The dashboard (prod calls this `casaos`). |
| `mesh-router-caddy` | `ghcr.io/yundera/mesh-router-caddy` | Reverse proxy. Discovers app containers by `caddy_*` labels; owns `:80`/`:443`. |
| `dex` | `ghcr.io/dexidp/dex` | OIDC provider at `https://auth-<DOMAIN>`. |
| `auth-registrar` | `ghcr.io/yundera/mesh-auth` | Apps POST their redirect URI here on first login and get an OIDC `client_id`/`secret` back (registered into Dex over gRPC). |

All four run on the **`pcs`** network — the same external network store apps
declare (`networks: pcs external: true`) — and Maison is configured with
`REF_NET=pcs`, so every app it installs joins `pcs` too.

### The one deliberate difference from prod

In production, Dex federates login to CasaOS via the `casaos-oidc-bridge`
(→ `POST /v1/users/login`). **Maison has no auth and no login API by design**,
so that bridge can't work here. Instead Dex uses its built-in local password DB
with a single seeded test user (`dev/dex/config.yaml`):

```
email:    test@maison.local
password: maison
```

Everything else — issuer shape (`https://auth-<DOMAIN>`), dynamic gRPC client
registration, Caddy label routing — matches prod.

## Run it

```bash
cd dev
cp .env.dev.example .env.dev        # then set DOCKER_GID + DATA_HOST_PATH for your host
./gen-certs.sh                      # once — writes dev/certs (already generated in this tree)
docker compose --env-file .env.dev up -d --build
```

Then open:

- **Dashboard:** <https://app.localhost> (root domain → maison) or
  <https://maison-app.localhost>
- **Dex:** <https://auth-app.localhost/.well-known/openid-configuration>

`*.localhost` resolves to `127.0.0.1` in modern browsers, and the mounted dev CA
covers `*.localhost`, so no hosts-file editing is needed on the host.

## Testing an SSO app

1. Install an SSO-enabled app from the store (e.g. an `nginx-hash-lock` app whose
   compose sets `OIDC_REGISTRAR_URL: http://auth-registrar:9092/register`).
2. On first hit, the app's auth sidecar POSTs its callback to `auth-registrar`,
   which registers a client in Dex and returns `client_id`/`secret` + the issuer
   `https://auth-app.localhost`.
3. The app redirects you to Dex; log in as `test@maison.local` / `maison`;
   Dex redirects back to the app's callback and you're in.

### TLS trust caveat (read this if OIDC login fails)

Caddy serves `auth-<DOMAIN>` with the **dev CA** (self-signed). The browser only
warns, but an app's **back-channel** OIDC calls (e.g. Node `openid-client`'s
`Issuer.discover`) validate TLS against system roots and will **reject** the dev
cert. For a full end-to-end login, make the app trust `dev/certs/ca.pem`. For the
Node-based `nginx-hash-lock` sidecar, add to that app's compose:

```yaml
environment:
  NODE_EXTRA_CA_CERTS: /certs/ca.pem      # and bind-mount dev/certs/ca.pem to /certs/ca.pem
# quick-and-dirty alternative (dev only): NODE_TLS_REJECT_UNAUTHORIZED: "0"
```

This is the only spot where the local self-signed CA leaks into an app; on a real
PCS the certs are publicly trusted so it doesn't arise.

## Changing `DOMAIN`

`app.localhost` is the zero-config default. If you switch to something else
(e.g. a `nip.io` form for LAN access), update **three** places to keep them in
sync: `DOMAIN` in `.env.dev`, the `issuer:` line in `dev/dex/config.yaml`, and
the cert SANs (`./gen-certs.sh --force` after editing its `san.ext` block).

## Teardown

```bash
docker compose --env-file .env.dev down
# add -v to also drop the pcs/dex-internal networks and caddy state
```
