# `.env.app` — what an app receives

CasaOS keeps the variables it needs to run and the variables it hands to the apps it
manages in **one environment**. You cannot tell, looking at a CasaOS container's env,
which half is which — and the two drift into each other.

Maison separates them:

| | Lives in | Owned by | Example |
|---|---|---|---|
| **What Maison needs to run** | Maison's own environment (`docker-compose.yml`) | Maison | `DATA_ROOT`, `APPSTORE_URL`, `PROTECTED_APPS`, `HTTP_ADDR` |
| **What an app receives** | `.env.app` | the **deployment** | `APP_NET`, `APP_DOMAIN`, `APP_PUBLIC_IP_DASH`, `APP_DEFAULT_PASSWORD` |

Nothing is in both, so there is never a question of which one wins.

## The file

```
${DATA_ROOT}/AppData/maison/.env.app
```

`AppData/maison/` is Maison's own app directory — the same folder a deployment
installs the dashboard's compose stack into, and where Maison keeps its settings and
store cache. Everything Maison owns is in that one place, with no hidden sibling.

It is a plain `KEY=VALUE` file, and it belongs to the **deployment**, not to Maison:
on a Yundera PCS the orchestrator writes it at provisioning; on a plain install it is
the operator's to edit. Maison creates it once, with a documented default, and
**never overwrites it** — an upgrade that silently reverted the deployment's domain,
network and credentials would be a bad day.

An empty value means *the deployment does not have this*, and the key is skipped
rather than written blank: an app is better off with an unresolved `${APP_DOMAIN}` —
which `docker compose` reports — than a blank one, which silently routes it at
nothing.

## How it reaches an app

On install, and again on **every start**, Maison reads `.env.app` and ensures each
key in the app's own `.env`:

- a key already there is **set** to the current value, in the line it already occupies;
- a key that is missing is **appended**;
- **everything else is left alone** — a variable the operator added to an app's `.env`
  is theirs, and survives.

Keys are ensured one at a time, so neither file's ordering matters.

Merged in alongside are the few variables Maison computes per app and per install,
which a deployment cannot state: `AppID`, `PUID`, `PGID`, `TZ`, `DATA_ROOT`,
`DATA_HOST_PATH`. Setting those in `.env.app` has no effect.

## Why it is re-applied on every start

Because an app is installed against one deployment and started against whatever that
deployment has since **become** — a new app network, a new data root, a new domain,
a new public IP. None of that invalidates the app's own configuration, so none of it
should stop the app from starting.

This is why an app's generated compose refers to its surroundings only through
`${APP_NET}`, `${DATA_ROOT}`, `${APP_DOMAIN}` … and **never** through a resolved
literal (see `envinject.Transform`). The literal is the bug: Maison used to bake
the network name and the host data path into each app's `docker-compose.yml` at
install time, which froze the app to the deployment it happened to be installed on.
Move the box, and every app was unstartable — with reinstall as the only way out.

Now the compose says *"whatever the app network is"*, `.env.app` says what it
currently is, and every start resolves the two afresh.

## Why the values are written into the app's `.env`

Maison could just pass them to `docker compose` in its own process environment —
it already runs the command. But then the app folder would only work when *Maison*
brought it up.

Writing them into the `.env` is the point: a `docker compose up -d` you run by hand
in `AppData/<app>/` must bring the app up **exactly** as Maison does. The folder
stands on its own. That is the promise of the app model (see
[`app-model.md`](./app-model.md)), and it is also what makes an app debuggable
without the dashboard in the loop.

## Adding a variable

Add a line to `.env.app`. That is the whole procedure — no rebuild, no code change.
It reaches every app on its next start.

## Editing it from the dashboard

**Settings › App environment** edits this file, for a deployment where opening a shell
is not the natural move. It is the file itself in a text box, not a form: the comments
are the documentation, there is no fixed schema — a deployment adds its own keys — and
an empty value means something a form field cannot express.

Three things the page is careful about, all of them consequences of the above:

- **It says the file has another owner.** On a PCS the orchestrator wrote it and may
  write it again, over your edit. Maison cannot prevent that and does not pretend to.
- **It saves nothing it cannot read back.** A line that is not `KEY=VALUE`, a name that
  is not a shell identifier, a duplicated key — all rejected, with the line number, and
  the file on disk is left untouched. The parser is deliberately forgiving (it skips a
  line it cannot read); a save is not, because a typo that silently does nothing would
  surface days later as an app that will not start.
- **It restarts nothing.** The variables land in each app's `.env` on that app's next
  start, per the section above. Unlike the domains list — which rewrites Caddy labels
  and so *must* recreate containers to mean anything — there is nothing here that
  recreating every container on the box would buy.

A key Maison computes per app (`AppID`, `PUID`, `PGID`, `TZ`, `DATA_ROOT`,
`DATA_HOST_PATH`) is reported as having no effect rather than rejected: the file is the
deployment's, and it is free to say what it likes.

Note the file holds `APP_DEFAULT_PASSWORD` and `DefaultPassword` in plain text, so the
page shows credentials. It was always a readable file on a box with no auth (Maison
assumes a trusted network — see the README); the page does not change who can read it,
only how far they have to reach.

## The default

```sh
APP_NET=mesh                 # the external network apps are attached to
APP_DOMAIN=                  # the deployment's base domain
APP_PUBLIC_IP=               # …and its public IP, in the spellings the store uses
APP_PUBLIC_IP_DASH=
APP_PUBLIC_IPV4=
APP_PUBLIC_IPV4_DASH=
APP_PUBLIC_IPV6=
APP_PUBLIC_IPV6_DASH=
domain=                      # lowercase alias: some x-compose-app webui-host use it
APP_EMAIL=
APP_DEFAULT_PASSWORD=casaos
DefaultUserName=admin
DefaultPassword=casaos
```

That default describes a standalone local install: apps on Maison's own `mesh`
network, no domain, so no reachable web address. A PCS overwrites it with
`APP_NET=pcs` and its real domain and IP; `dev/docker-compose.yml` does the same
through its `appenv` init container.

## A note on `REF_*`

CasaOS used `REF_SCHEME` / `REF_PORT` / `REF_DOMAIN` / `REF_SEPARATOR` to *synthesize*
an app's web-UI URL as `scheme://<app><sep><domain>:<port>` (`casa-img`,
`route/v2/appstore_pcs.go`). Maison replaced that mechanism entirely with
`x-compose-app`'s `webui-host` / `webui-scheme` / `webui-port` (see
[`x-compose-app.md`](./x-compose-app.md)), so those variables have no consumer here
and are gone. `REF_NET` and `REF_DOMAIN` were duplicates of `APP_NET` and
`APP_DOMAIN`, which a PCS already sets; they are gone too.

A `REF_*` line left over in an old app's `.env` is inert — nothing interpolates it —
and Maison leaves it alone rather than deleting a line it no longer owns.
