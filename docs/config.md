# Configuration

Where Maison's configuration lives, who owns each part, and how a deployment manages
it. Companions: [`app-env.md`](./app-env.md) (what apps receive), [`feedback.md`](./feedback.md),
[`incidents.md`](./incidents.md) and [`backup.md`](./backup.md) (mail).

## The rule

- **The environment is plumbing only**: what Maison needs before it can read a file
  (`HTTP_ADDR`, `DATA_ROOT`, `DATA_HOST_PATH`, `STATE_DIR`, `PUID`/`PGID`, `TZ`,
  `HOST_PROC`, `ADMIN_SOCKET`).
- **Every behavioural setting lives in `settings.json`**, and has exactly one home:
  never one value in the environment and another in the file.
- **Some settings can be managed by the deployment.** A managed setting is read-only in
  the dashboard, shows who owns it, and is re-asserted by the deployment on every
  self-check. When the deployment stops offering the feature it *releases* the setting:
  the value is cleared and the setting is the user's again.
- **Secrets are never in a setting.** A setting holds a reference, `${secret:<name>}`;
  the value lives in `secrets.env`.

## Files in `STATE_DIR`

| File | Mode | Written by |
|---|---|---|
| `settings.json` | 0600 | Maison only — the dashboard, or the deployment through the admin socket |
| `secrets.env` | 0600 | Maison only — same two paths |
| `.env.app` | 0600 | the deployment ([`app-env.md`](./app-env.md)) |

Both Maison-owned files are written atomically (temp file + rename).

## Managed settings

| Setting | Value | Notes |
|---|---|---|
| `smtp` | `{host, port, user, pass, from, to, security}` | `pass` may be a secret reference. Empty `from`/`to` fall back to `noreply@APP_DOMAIN` / `APP_EMAIL`. |
| `domains` | `[{name, domain, labels}]` | Same validation as the dashboard's editor. A change republishes every app. |
| `feedback` | `{url, token}` | `token` should be a secret reference. Never user-editable. |
| `onboarding` | `{url}` | While set, the dashboard is replaced by the setup interstitial. Never user-editable. |

On disk each of these is wrapped:

```json
"smtp": { "managed": true, "owner": "mesh", "value": { "host": "smtp", "port": 587 } }
```

Only a managed value is wrapped. An unmanaged one keeps its plain shape (a plain `smtp`
object, a bare `domains` array), which is what Maison before 1.1.50 reads, so a box rolled
back to an older image keeps the user's settings. (1.1.50 also wrapped unmanaged values,
with `"managed": false`; those still read, and are written back plain.)

Over the HTTP API settings stay flat, plus `"managed": {"<setting>": "<owner>"}`.
`PUT /api/settings` silently drops a managed field (the dashboard sends its whole settings
object on every change), and never sets `feedback` or `onboarding` at all.
`PUT /api/settings/domains` answers 409 while the list is managed.

Stores are not managed: they are the user's list. A deployment only makes sure its own
store is on it (`maison stores ensure`).

## Secrets

- `secrets.env`: one `name=value` per line, names `[a-z][a-z0-9_]*`, single-line values.
  Comments are kept on rewrite.
- A reference is a **whole value**: the string `${secret:smtp_pass}` resolves; a longer
  string containing it does not. A real password containing `${` is never misread.
- References are resolved at use, never stored resolved.
- **The API never returns a secret value.** It returns the reference itself, which is
  its own mask, or `********` for a password a box still carries literally. A client
  that sends either back unchanged keeps the stored secret; an empty value deletes it.
- A password typed into *Settings → Notifications* is stored as `smtp_pass` and the
  setting saves the reference. At boot, a literal password left in `settings.json` from
  an older version is moved into `secrets.env` the same way.
- Releasing a managed setting deletes the secrets its value referenced.

## The admin socket and the `maison` CLI

The deployment manages settings through a unix socket inside the container
(`ADMIN_SOCKET`, default `/run/maison/admin.sock`, mode 0600), never over HTTP. The HTTP
API has no authentication of its own and every app on the shared network can reach it,
so it must not be able to change who owns a setting or write a secret. The socket is
neither on the network nor on the `/DATA` mount.

Its boundary is "can run code in this container". App hooks also run in this container,
but they already have the Docker socket, which means root on the host, so the socket
gives them nothing new.

```sh
docker exec -i maison-app maison secret set feedback_token <<<"$FEEDBACK_TOKEN"
docker exec -i maison-app maison settings set feedback --owner yundera \
  <<<'{"url":"http://admin-app/api/feedback/ingest","token":"${secret:feedback_token}"}'
docker exec maison-app maison settings release feedback
docker exec maison-app maison stores ensure https://github.com/Yundera/AppStore/archive/refs/heads/main.zip
docker exec maison-app maison settings get
```

| Command | Effect |
|---|---|
| `settings get` | The settings as the API shows them (secrets masked, owners named). |
| `settings set <name> --owner <o>` | Manage a setting. Value is JSON on stdin, decoded strictly: an unknown field, an invalid value or a reference to a missing secret is refused. |
| `settings release <name>` | Clear the value, unmanage it, delete its secrets. **A no-op if the setting is not managed**, so a deployment can call it every night while its feature is off without touching a user's value. |
| `secret set <name>` / `secret rm <name>` | Store (value on stdin, one trailing newline dropped) / delete a secret. |
| `stores ensure <url>` | Add the store unless it is already listed; the catalog refreshes in the background. |
| `backup approve <engine>` | Approve a backup engine the deployment provisioned, without the container check the dashboard's Enable button makes. Idempotent. See backup.md › "The adapter image is provisioned, and approved before it runs". |

Every command waits up to 30 s for the socket (a self-check runs right after
redeploying the container) and exits non-zero with the server's reason on any failure.
An image that does not know a setting therefore fails visibly in the self-check log,
which the old `SMTP_*`/`FEEDBACK_*` variables never did: an older image ignored them.

Owner names are letters, digits and `-`, starting with a letter, and are shown to the
user ("Managed by Yundera").

## Transition

These still work, and go away once every deployment manages the settings instead:

- `SMTP_*`: while `smtp` is **not managed**, the env transport is still the base layer
  under a user's `smtp` value (`EffectiveSMTP`). A managed `smtp` ignores it.
- `FEEDBACK_URL`/`FEEDBACK_TOKEN`: used while no `feedback` setting exists.
- `onboarding.json`: read while no `onboarding` setting exists.
- `APPSTORE_URL`: the boot default for the store list, until `stores ensure` replaces it.
