# Study — app secrets on disk

Status: working notes, 2026-10-21. What is decided, what was rejected and why, and
what is still open, so the next pass does not re-derive it.

## Where secrets live

| What | Where | Mode |
|---|---|---|
| An app's generated / seeded secrets (`x-compose-app.secrets`) | `AppData/<app>/.env` | `0600`, Maison's user |
| The box's shared default password (`APP_DEFAULT_PASSWORD`, `DefaultPassword`) | `AppData/maison/.env.app`, and the `.env` of apps that still use it | `0600` |
| Maison's own secrets (SMTP, feedback token, …) | `AppData/maison/secrets.env`, referenced as `${secret:name}` | `0600` |
| Backup engine secret | the engine's dir (`repository.password`), escrowed by mail | `0600` |
| Template stack state (`.stack.env`, mesh `.env`) | written by the templates | `600` already |

Through maison v1.1.77 an app's `.env` and `.env.app` were `0644`: readable by every
process on the box.

## Threat model

Who can read an app's `.env`, and whether it is worth defending against them:

1. **Root, the Docker socket, the `admin`/`pcs` sudoers.** Out of scope. `docker
   inspect` prints every container's environment anyway, and an app that mounts the
   socket (Terminal, ClaudeCode, …) is root. Nothing on disk survives this reader.
2. **Other apps and file shares.** The real exposure. FileBrowser mounts the whole
   data root read-write, Samba shares it as `PUID`, the Kopia store app mounts it.
   Anyone who logs into one of them — or compromises one — could read
   `AppData/<any app>/.env`. **Fixed for files** by `0600` root (they run as `PUID`);
   a root-run app that mounts `/DATA` still sees everything (see *Open* below).
3. **Backups.** `.env` is in every snapshot. The repository is encrypted, so this is
   acceptable; a restore puts back the old mode, which the next up tightens.
4. **The Maison UI.** Already masks and locks declared secrets and credential-looking
   keys server-side (Config › Env, the resolved Compose view).

## Decided and implemented (maison, 2026-10-21, unstaged)

- **Every `.env` Maison writes is `0600`, owned by its user** — written through
  temp-file + rename (`envinject.WriteEnvFile`). A file Maison did not write is
  tightened on the app's next up even when nothing changed (`EnsureVars` →
  `TightenEnvFile`), and every stack's at boot (`appenv.TightenAll`). Symlinks are
  left alone. Hand-run `docker compose up` needs `sudo`; accepted.
  - Checked: steps run in Maison's container (root), kopia runs as root with DAC caps,
    the admin app runs as root and does not read app `.env` files, no store app
    bind-mounts its own `.env` or uses `env_file:` — nothing loses access.
- **The shared password goes only to apps that use it.** `appenv.Sync` forwards
  `APP_DEFAULT_PASSWORD` / `DefaultPassword` into an app's `.env` only when it
  already has a value (kept, as before) or its compose / override / `.seed` /
  `.scripts` reference it other than as a seed (`${APP_DEFAULT_PASSWORD}|…`). Text
  scan, errs wide. The official store has zero non-seed references, so a fresh
  official install no longer carries the box password at all.

## Rejected

- **Splitting secrets into a second file** (`.secrets.env`, `COMPOSE_ENV_FILES`,
  `--env-file`). The app folder must come up with a plain `docker compose up -d` and
  be fully manageable with ordinary Docker tooling, without Maison; we will not tell
  compose how to load secrets. Mode and ownership on the one `.env` give the same
  protection without that cost.
- **Encryption at rest** (sops/age, Vault, TPM-sealed). The box boots and starts
  apps unattended, so the key sits on the same disk for the same root — no protection,
  plus a new way to brick every app if the key is lost (we already live that with
  kopia's `needs-recovery`). Only worth reopening if a *separate* trust domain holds
  keys (e.g. the orchestrator pushing them like the identity JWT).
- **Docker `secrets:` / `_FILE` variables as a platform mechanism.** Per-image, per-app
  store work, and the value is still a file on disk. It does keep values out of
  `docker inspect` and inherited environments, so a store author may still use it in
  one app; Maison does not need to know.

## Open

- **Apps that mount the whole data root** (FileBrowser, Samba, Kopia store app, and
  anything root-run that mounts `/DATA`). The `0600` change closes `.env` for `PUID`
  apps, not the apps' own data. Work started under the *app vs app-root* model
  (separate listings for full access, e.g. `FileBrowserRoot`); also a `maison lint`
  warning for a bind of bare `${DATA_ROOT}`. Out of scope 2026-10-21.
- **Technical secrets seeded from the shared password cannot be changed.** A
  pre-migration install keeps a DB password equal to the old box password forever;
  `credentials[].change` covers logins only (`x-compose-app.md` › Credentials). Low
  risk (internal networks) but it means "the user rotated everything" is never fully
  true on an old box.
- **Retiring `DEFAULT_PWD` from `.env.app`.** The template still writes it
  (`mesh-router-template-root/scripts/self-check/ensure-maison-stack.sh`). Remove it
  once (a) this Maison is on the whole fleet and (b) no store a box can reach has a
  non-seed reference. Then `.env.app` holds nothing secret.
- **Other stores.** AppStoreLab: 193 non-seed references across 47 apps;
  Worph-AppStore: 24 across 9. They keep receiving the shared password (correctly,
  per the scan) until migrated to `secrets` + seeds.
- **Cross-app reads.** SeafileDrive's setup step reads `AppData/seafile/.env`
  (falling back to its `APP_DEFAULT_PASSWORD`). Works because steps run as root;
  it is still one app reaching into another's secrets and should become a declared
  dependency/export some day.
