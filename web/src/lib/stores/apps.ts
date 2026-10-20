import { get, writable } from 'svelte/store'
import { live } from '../live/ws'
import { api } from '../api/client'
import { settings } from './settings'
import { locale, pick, type Localized } from '../i18n'

export interface App {
  id: string
  /** Display title in the current locale: localizeApp() picks it from `title`,
   *  falling back to the server's en_us (or the project name). */
  name: string
  /** The declared title in every locale (x-compose-app `title`). */
  title?: Localized
  icon: string
  status: 'running' | 'stopped' | 'partial'
  managed: boolean
  store?: string
  scheme?: string
  port?: string
  index?: string
  category?: string
  /** Which dashboard grid this tile belongs in, from the app's x-compose-app
   *  `view`. Absent means the ordinary grid. "service" is usually derived
   *  server-side: an app that declares no view and no web UI. */
  view?: 'apps' | 'system' | 'service'
  /** The id of the app this one extends, from its x-compose-app `parent`, and
   *  only ever an app that is actually installed — the server clears a parent it
   *  cannot resolve, so an extension whose parent is missing arrives as an
   *  ordinary app. Carrying one means: no top-level tile, shown under the
   *  parent's instead. It confers nothing else; lifecycle stays per-app. */
  parent?: string
  /** The app declares x-compose-app `credentials` (its initial logins), and —
   *  pending — the operator has not ticked "Don't show again", so a click shows
   *  them before opening the app. Flags only: the values never travel in the list
   *  (it is cached in localStorage); showCredentials fetches them on demand. */
  has_credentials?: boolean
  credentials_pending?: boolean
  /** The app's effective tips are non-empty: the tile menu offers Tips only then
   *  (they are still edited, or written from scratch, in Settings). */
  has_tips?: boolean
  /** Fully-resolved click URL from x-compose-app (webui-*); wins when set. */
  url?: string
  /** Aggregated Docker health-check verdict; drives the tile status dot.
   *  Absent when no container declares a health check. */
  health?: 'healthy' | 'unhealthy' | 'starting'
  /** The app's own x-compose-app `lifecycle` declaration, true unless it says
   *  otherwise — never derived from `view`. When false the tile withholds the
   *  entry and the backend refuses it. Restart stays available either way — it is
   *  how a wedged platform app is recovered without an SSH session. */
  stoppable?: boolean
  uninstallable?: boolean
  /** `lifecycle.updatable`: false when something other than Maison keeps the app
   *  up to date. It then has no Updates row and no Update tab, and the update
   *  routes answer 403. */
  updatable?: boolean
  /** True while a lifecycle op (start/stop/restart/uninstall) is in flight:
   *  the tile shows a "…" overlay and hides its burger menu. */
  busy?: boolean
  /** True while a store install is in flight for this app: the tile shows a
   *  progress bar (download, then start) instead of being clickable. */
  installing?: boolean
  /** True while an update runs: the tile shows the step it is on — check | pull |
   *  backup | stop | apply | start | rollback — and that step's progress, instead
   *  of the bare busy overlay. Byte counts, rate and ETA are set in backup only. */
  updating?: boolean
  update_phase?: string
  update_message?: string
  update_pct?: number
  update_done?: number
  update_total?: number
  update_rate?: number
  update_eta?: number
  /** Image-pull progress, 0-100 (only meaningful while installing). */
  download?: number
  /** Stack-start progress, 0-100, driven by Docker (only while installing). */
  start?: number
  /** Current install/uninstall phase: pull | prepare | start | backup |
   *  archive | remove | done | error. */
  phase?: string
  /** Set when an install failed; the tile shows the error until retried. */
  install_error?: string
  /** True while an uninstall is in flight for this app: the tile shows the same
   *  progress bar as an install, in red. */
  uninstalling?: boolean
  /** Backing the app up through the default engine, 0-100 (only while
   *  uninstalling) — an upload when that engine is a repository, and the step an
   *  uninstall now spends nearly all its time in. */
  uninstall_backup?: number
  /** Finalising that backup, 0-100: a zip's compression, instant otherwise. */
  archive?: number
  /** Container- and folder-removal progress, 0-100 (only while uninstalling). */
  remove?: number
  /** Set when an uninstall failed; the tile shows the error until dismissed. */
  uninstall_error?: string
  /** True while a backup or restore is in flight: the tile shows the same one
   *  bar again, in its own colour. */
  backing_up?: boolean
  /** First (live) mirror pass, 0-100 — the long one, with the app still up. */
  copy?: number
  /** Second (stopped) mirror pass, 0-100 — the app's downtime. */
  sync?: number
  /** Snapshot compression, 0-100 (zip backups only). */
  compress?: number
  /** Set when a backup/restore failed; the tile shows it until dismissed. */
  backup_error?: string
  /** Bytes moved and expected in the current phase. Absent when the engine reports
   *  no byte counts, which is normal — see internal/apps/progress.go. */
  backup_done?: number
  backup_total?: number
  /** Bytes per second, absent until it can be measured. */
  backup_rate?: number
  /** Seconds left in the current phase, absent until the estimate is worth showing.
   *  Per phase, not for the whole backup: during `sync` it is how much longer the
   *  app is down. */
  backup_eta?: number
}

// Last-known app list, cached in localStorage so a page reload paints the grid
// instantly — before the backend (or Docker) has answered. The server is now the
// source of truth for *what exists* (folder-driven, docs/app-model.md), so the
// cache only bridges the first-paint gap; a fresh snapshot supersedes it in ms.
const CACHE_KEY = 'maison.apps'

/** Read the cached tiles, or [] when absent/corrupt. */
function cachedApps(): App[] {
  try {
    const v = JSON.parse(localStorage.getItem(CACHE_KEY) ?? '[]')
    return Array.isArray(v) ? (v as App[]) : []
  } catch {
    return []
  }
}

/** Persist tiles for the next reload, minus volatile in-flight overlays — so a
 *  reload mid-install/-op never resurrects a stuck "…" or a frozen progress bar.
 *  Identity, name, icon and last-known status/health are kept so the tile looks
 *  right immediately (greyed vs coloured) until the live state refreshes. */
function persistApps(list: App[]): void {
  try {
    const clean = list.map((a) => {
      const c: App = { ...a }
      delete c.busy
      delete c.installing
      delete c.download
      delete c.start
      delete c.phase
      delete c.install_error
      delete c.uninstalling
      delete c.uninstall_backup
      delete c.archive
      delete c.remove
      delete c.uninstall_error
      delete c.backing_up
      delete c.updating
      delete c.update_phase
      delete c.update_message
      delete c.update_pct
      delete c.update_done
      delete c.update_total
      delete c.update_rate
      delete c.update_eta
      delete c.copy
      delete c.sync
      delete c.compress
      delete c.backup_done
      delete c.backup_total
      delete c.backup_rate
      delete c.backup_eta
      return c
    })
    localStorage.setItem(CACHE_KEY, JSON.stringify(clean))
  } catch {
    /* private mode / quota — cache is best-effort */
  }
}

/** Set `name` to the title in the current locale. Idempotent: it reads `title`,
 *  never the previous `name`, so a locale switch can re-run it over the grid. */
function localizeApp(a: App): App {
  return a.title ? { ...a, name: pick(a.title, get(locale), a.name) } : a
}

/** Apply an authoritative snapshot: replace the grid and refresh the cache. */
function applySnapshot(list: App[]): void {
  list = list.map(localizeApp)
  apps.set(list)
  persistApps(list)
}

export const apps = writable<App[]>(cachedApps())

// Every name everywhere (tile, menus, dialogs, search) follows a language switch.
locale.subscribe(() => apps.update((list) => list.map(localizeApp)))

/** One-shot REST load (used on mount for an immediate render). The REST endpoint
 *  is authoritative — including a genuine empty list — so its result always wins;
 *  on failure we keep whatever is on screen (cache or a prior load). */
export async function loadApps(): Promise<void> {
  try {
    applySnapshot((await api.get<App[]>('/api/apps')) ?? [])
  } catch {
    /* leave previous value (cached tiles stay visible) */
  }
}

/** Live updates over the WebSocket "apps" channel. Returns unsubscribe.
 *
 *  Non-destructive: an empty live frame never blanks a populated grid, because
 *  the server also emits [] while it is still warming up (Docker not answered
 *  yet) and a cache-painted dashboard must not flash empty underneath it.
 *
 *  With ONE exception, and it is the whole reason this is not a plain guard:
 *  when the grid holds an operation in flight, an empty frame is that
 *  operation's completion — uninstalling the last app on the box is exactly
 *  this — and dropping it strands the tile on its final progress bar
 *  ("Removing… 0%") until the page is reloaded. loadApps() cannot cover it: the
 *  DELETE returns when the uninstall is *accepted*, so the REST replace that
 *  follows it still reports the app as uninstalling.
 *
 *  The exception is safe during warm-up because the in-flight flags only ever
 *  come from a server snapshot — persistApps() strips them from the cache — so
 *  a grid painted from localStorage alone can never satisfy it. */
export function subscribeApps(): () => void {
  return live.subscribe('apps', (d) => {
    const list = (d as App[]) ?? []
    const current = get(apps)
    const inFlight = current.some((a) => a.installing || a.uninstalling || a.busy || a.updating)
    if (list.length === 0 && current.length > 0 && !inFlight) return
    applySnapshot(list)
  })
}

export async function appAction(id: string, action: 'start' | 'stop' | 'restart'): Promise<void> {
  await api.post(`/api/apps/${encodeURIComponent(id)}/${action}`)
  await loadApps()
}

/** Start uninstalling an app. The app is backed up through every engine set to
 *  receive uninstalls first, and only removed once that backup is committed — so on a
 *  box backing up offsite, an uninstalled app's data is offsite. A box where no engine
 *  receives uninstalls removes the app without a backup.
 *  `zip` only means anything to the local engine (see UninstallDialog).
 *
 *  The request returns as soon as the uninstall is *accepted*, not when it is
 *  done: from there the tile carries its progress (a red bar — backing up, then
 *  archiving, then removing) over the live app list, so nothing has to wait on an
 *  upload that can run for minutes. A rejection (an app that declares it cannot be
 *  uninstalled) still surfaces
 *  here, as a throw. */
export async function uninstallApp(id: string, zip = false): Promise<void> {
  await api.del<{ status: string }>(`/api/apps/${encodeURIComponent(id)}?zip=${zip}`)
  await loadApps()
}

/** Uninstall an app WITHOUT backing it up: its containers and its data are deleted.
 *
 *  The way past an uninstall whose backup keeps failing, when the user has decided the
 *  data is not worth fixing the engine for. Offered from the app's Settings › Backups
 *  tab only, behind its own confirmation — never from the tile menu, where the normal
 *  uninstall backs up first. */
export async function uninstallAppWithoutBackup(id: string): Promise<void> {
  await api.del<{ status: string }>(`/api/apps/${encodeURIComponent(id)}?backup=false`)
  await loadApps()
}

/** Dismiss a failed install/uninstall overlay, clearing the tile's red "!". */
export async function dismissAppError(id: string): Promise<void> {
  await api.post(`/api/apps/${encodeURIComponent(id)}/dismiss`)
  await loadApps()
}

/** Which operation a progress bar is showing. The kind picks the bar's colour
 *  (`--progress-*` in styles/tokens.css): downloading is blue, starting the
 *  stack is green, uninstalling is red, backing up is amber. */
export type ProgressKind = 'download' | 'install' | 'uninstall' | 'backup' | 'update'

export interface AppProgress {
  kind: ProgressKind
  /** 0-100 for the step in progress — not an average across the operation. */
  pct: number
  /** i18n key for the step's label ("Downloading…", "Archiving…", …). */
  label: string
  /** Seconds left in this step, absent when not yet knowable. Per step, not for the
   *  whole operation — during a backup's `sync` it is how much longer the app is
   *  down, which is the part of it the user can actually feel. */
  eta?: number
  /** Bytes moved, expected, and per second. Absent for an engine that reports no
   *  byte counts. The tile has no room for these; they are what its tooltip says. */
  done?: number
  total?: number
  rate?: number
}

/** The single progress bar an app shows, or null when nothing is in flight.
 *
 *  The backend measures each operation on several tracks (install: pull then
 *  bring-up; uninstall: back up, finalise, then remove), but the UI shows **one bar at a
 *  time**: the step currently running, in that operation's colour. Both the
 *  dashboard tile and the store's install pill read this, so they can never
 *  disagree about what an app is doing.
 *
 *  A failed operation is not progress — it clears the bar and leaves the error
 *  (install_error / uninstall_error) to be shown instead. */
/** i18n label for each update step. */
const updateLabels: Record<string, string> = {
  check: 'update_checking',
  pull: 'downloading',
  backup: 'backing_up',
  stop: 'stopping',
  apply: 'update_applying',
  migrate: 'update_migrating',
  start: 'starting_up',
  rollback: 'update_rolling_back',
}

export function appProgress(a: App | undefined): AppProgress | null {
  if (!a) return null
  if (a.updating) {
    // One bar for the step running now, in the update colour. Only the backup step
    // is measured in bytes; the rest are moments or unmeasured, and carry their
    // own percentage (full while a step with no track runs).
    const backup = a.update_phase === 'backup'
    return {
      kind: 'update',
      pct: a.update_pct ?? 0,
      label: updateLabels[a.update_phase ?? ''] ?? 'updating',
      ...(backup
        ? { eta: a.update_eta, done: a.update_done, total: a.update_total, rate: a.update_rate }
        : {}),
    }
  }
  if (a.uninstalling) {
    // Keyed on the phase, for the reason the backup branch below spells out: reading
    // the counters means "no track is still running" has to fall through to some
    // label, and whichever one it picks is wrong for most of the operation. It also
    // matters more here than it used to — an uninstall now starts with a backup that
    // can run for minutes, and reporting that as "Removing" describes the one thing
    // that has definitely not happened yet.
    if (a.phase === 'archive')
      return { kind: 'uninstall', pct: a.archive ?? 0, label: 'archiving' }
    if (a.phase === 'remove') return { kind: 'uninstall', pct: a.remove ?? 0, label: 'removing' }
    return { kind: 'uninstall', pct: a.uninstall_backup ?? 0, label: 'backing_up' }
  }
  if (a.installing) {
    const downloading = (a.download ?? 0) < 100
    return {
      kind: downloading ? 'download' : 'install',
      pct: downloading ? (a.download ?? 0) : (a.start ?? 0),
      label: downloading ? 'downloading' : 'starting_up',
    }
  }
  if (a.backing_up) {
    // Keyed on the phase, not on the counters. Keying on counters meant "none of
    // the tracks is still running" had to fall through to *some* label, and the
    // one it fell through to was `compressing` — which only actually happens for a
    // zip backup. A folder backup therefore reported "Compressing" for the whole
    // window between sync reaching 100 and the operation ending.
    //
    // Restore and start have no measured track — a restore is renames and (for a
    // zip) an extract — so they sit at a full bar while the label says what is
    // happening.
    // The measured numbers ride along with whichever track is reported below. They
    // belong to the phase, not to the operation, so they are attached once here
    // rather than being re-derived per branch.
    const m = { eta: a.backup_eta, done: a.backup_done, total: a.backup_total, rate: a.backup_rate }
    if (a.phase === 'restore') return { kind: 'backup', pct: 100, label: 'restoring', ...m }
    if (a.phase === 'start') return { kind: 'backup', pct: 100, label: 'starting_up' }
    if (a.phase === 'compress') return { kind: 'backup', pct: a.compress ?? 0, label: 'compressing', ...m }
    if (a.phase === 'sync') return { kind: 'backup', pct: a.sync ?? 0, label: 'syncing', ...m }
    if (a.phase === 'copy') return { kind: 'backup', pct: a.copy ?? 0, label: 'copying', ...m }
    // Unknown or absent phase: read the counters rather than guess a label, so a
    // phase added on the server renders as an honest bar instead of the wrong step.
    if ((a.copy ?? 0) < 100) return { kind: 'backup', pct: a.copy ?? 0, label: 'copying', ...m }
    return { kind: 'backup', pct: a.sync ?? 0, label: 'syncing', ...m }
  }
  return null
}

/** Effective opening-URL (x-compose-app webui-*) config for an app, plus the
 *  server-resolved preview URL. Editing it writes into the app's override. */
export interface WebUI {
  scheme: string
  host: string
  port: string
  path: string
  url: string
}

export interface AppConfig {
  base: string
  override: string
  webui: WebUI
  /** App guidance — seeded from the store, editable; saved into the override. */
  /** The app's tips in every locale; the editor shows the current one. */
  tips: Localized
}

export function getConfig(id: string): Promise<AppConfig> {
  return api.get<AppConfig>(`/api/apps/${encodeURIComponent(id)}/config`)
}

/** Save (or clear, when blank) the app's tips into its override. */
export async function setTips(id: string, tips: string): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/tips`, { tips })
}

/** Fetch the app's tips with ${VAR} references resolved (env-replaced preview),
 *  every locale of them. */
export async function renderTips(id: string): Promise<Localized> {
  const r = await api.get<{ tips: Localized | null }>(`/api/apps/${encodeURIComponent(id)}/tips`)
  return r.tips ?? {}
}

/** The rules a new credential value must meet (normalized by the server). */
export interface CredentialPolicy {
  min_length: number
  max_length: number
  charset: 'printable' | 'alnum'
  require?: ('lower' | 'upper' | 'digit' | 'symbol')[]
  /** Every character refused, Maison's built-in set included. */
  forbid: string
}

/** How a credential may be changed from Maison; absent = initial-only. */
export interface CredentialChange {
  apply: 'env' | 'hook'
  changeable: boolean
  /** Why it cannot be changed: 'shared' (box-wide password) or 'template'. */
  reason?: string
  policy: CredentialPolicy
}

/** One x-compose-app credential, rendered against the app's .env. */
export interface AppCredential {
  /** Position in the declared list — what a change names. */
  index: number
  label?: Localized
  username?: string
  password?: string
  note?: Localized
  change?: CredentialChange
}

/** Fetch the app's initial credentials. A POST so the secrets never sit behind
 *  a navigable URL (history, prefetch). */
export async function showCredentials(id: string): Promise<{ credentials: AppCredential[]; dismissed: boolean }> {
  return api.post(`/api/apps/${encodeURIComponent(id)}/credentials/show`)
}

/** A fresh password meeting the credential's policy. Nothing is saved. */
export async function generateCredential(id: string, index: number): Promise<string> {
  const r = await api.post<{ password: string }>(`/api/apps/${encodeURIComponent(id)}/credentials/${index}/generate`)
  return r.password
}

/** Replace a credential's password and apply it to the app (recreates it). A
 *  refused value rejects with an ApiError whose body carries `rules`. */
export async function changeCredential(id: string, index: number, password: string): Promise<void> {
  await api.post(`/api/apps/${encodeURIComponent(id)}/credentials/${index}/change`, { password })
}

/** Rules a value breaks, mirroring the server's secretgen.Policy.Validate so the
 *  form can say what is wrong before saving. The server stays the authority. */
export function credentialRuleViolations(p: CredentialPolicy, v: string): string[] {
  const out: string[] = []
  const n = [...v].length
  if (n < p.min_length) out.push('too_short')
  if (n > p.max_length) out.push('too_long')
  let lower = false, upper = false, digit = false, symbol = false, charset = false, forbidden = false
  for (const ch of v) {
    const c = ch.codePointAt(0) ?? 0
    if (c < 0x21 || c > 0x7e) charset = true
    else if (p.forbid.includes(ch)) forbidden = true
    else if (ch >= 'a' && ch <= 'z') lower = true
    else if (ch >= 'A' && ch <= 'Z') upper = true
    else if (ch >= '0' && ch <= '9') digit = true
    else {
      symbol = true
      if (p.charset === 'alnum') charset = true
    }
  }
  if (charset) out.push('charset')
  if (forbidden) out.push('forbidden')
  const has = { lower, upper, digit, symbol }
  for (const r of p.require ?? []) if (!has[r]) out.push(`require_${r}`)
  return out
}

/** Record (or clear) "Don't show again" for the app's credentials. */
export async function setCredentialsDismissed(id: string, dismissed: boolean): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/credentials/dismissed`, { dismissed })
}

/** One save of the settings window's Config tab: the YAML view's draft of the
 *  override, the Form view's edits (applied on top of the draft, or of the file
 *  when no draft is sent), and/or the .env. A part left out is left as it is.
 *  Validated by Compose before anything is written, then the app is recreated
 *  once. */
export interface SettingsChange {
  override?: string
  form?: OverrideForm
  env?: EnvVar[]
}

export async function saveSettings(id: string, change: SettingsChange): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/settings`, change)
  await loadApps()
}

/** The app's .env, in file order — prefilled at install (PUID, DATA_ROOT, REF_*,
 *  …) and hand-editable since. It is what ${VAR} in the app's compose resolves
 *  against; it is NOT the same as a service's `environment:` block (that lives in
 *  the override form). Shares the EnvVar shape declared below. */
export function getEnv(id: string): Promise<EnvEntry[]> {
  return api.get<EnvEntry[]>(`/api/apps/${encodeURIComponent(id)}/env`)
}

/** Who sets a .env key. `maison` (PUID, DATA_ROOT, AppID, …) and `deployment`
 *  (forwarded from .env.app) are written back on every start, so an edit to one
 *  never takes effect; `seeded` (the default password) is written once and is the
 *  app's to keep; `secret` is one of the app's own generated secrets, changed only
 *  through its credentials. Every sourced row is locked — the server keeps its
 *  value whatever a save sends. No source: the operator's own variable. */
export type EnvSource = 'maison' | 'deployment' | 'seeded' | 'secret'

export interface EnvEntry extends EnvVar {
  source?: EnvSource
  /** The value is withheld (served as ""). Sent back blank it is kept as is; an
   *  unlocked masked row takes a typed value as a replacement. */
  masked?: boolean
}


/** A single-valued override field. `value` is what runs, `base` what the store
 *  ships; clearing `value` resets the field to the store's. `complex` marks a
 *  construct only the YAML view can edit safely (a list-form command, a hand-
 *  written tag) — the form shows it read-only. */
export interface Scalar {
  value: string
  base: string
  overridden: boolean
  complex: boolean
  /** The field's YAML, when `complex` — so the form can show what it won't edit. */
  raw?: string
}

/** A sequence field (ports, volumes, …). `value` is the effective list after
 *  Compose's merge; entries also present in `base` came from the store. */
export interface ListField {
  value: string[]
  base: string[]
  overridden: boolean
  complex: boolean
  raw?: string
}

export interface EnvVar {
  key: string
  value: string
}

export interface EnvField {
  value: EnvVar[]
  base: EnvVar[]
  overridden: boolean
  complex: boolean
  raw?: string
}

/** One compose service as the settings form edits it. */
export interface FormService {
  name: string
  image: Scalar
  restart: Scalar
  ports: ListField
  volumes: ListField
  environment: EnvField
  privileged: Scalar
  command: Scalar
  mem_limit: Scalar
  cpus: Scalar
  devices: ListField
  cap_add: ListField
}

/** The app's opening URL (x-compose-app webui-*), as form scalars: an empty value
 *  inherits the store's. */
export interface FormWebUI {
  scheme: Scalar
  host: Scalar
  port: Scalar
  path: Scalar
}

export interface OverrideForm {
  services: FormService[]
  webui?: FormWebUI
  /** The click URL the app resolves to today (read-only preview). */
  web_url?: string
}

/** The friendly view of the app's override — one entry per compose service. */
export function getOverrideForm(id: string): Promise<OverrideForm> {
  return api.get<OverrideForm>(`/api/apps/${encodeURIComponent(id)}/override/form`)
}

/** The override a form would save, without saving it (Form → YAML with edits):
 *  the form applied onto `draft`, the YAML view's text. */
export async function renderOverrideForm(
  id: string,
  form: OverrideForm,
  draft: string,
): Promise<string> {
  const r = await api.post<{ override: string }>(
    `/api/apps/${encodeURIComponent(id)}/override/form/render`,
    { form, override: draft },
  )
  return r.override
}

/** The form for a candidate override, without saving it (YAML → Form with edits). */
export function parseOverrideForm(id: string, override: string): Promise<OverrideForm> {
  return api.post<OverrideForm>(`/api/apps/${encodeURIComponent(id)}/override/form/parse`, {
    override,
  })
}

/** Check a candidate override against the app's base compose without applying it.
 *  A rejected override is a normal response carrying Compose's own message. */
export function validateOverride(id: string, override: string): Promise<{ ok: boolean; error?: string }> {
  return api.post<{ ok: boolean; error?: string }>(
    `/api/apps/${encodeURIComponent(id)}/override/validate`,
    { override },
  )
}

/** The project as Compose merges it: base + override. Variables stay written as
 *  ${VAR} unless `resolve` asks for them filled in from the .env. */
export async function effectiveConfig(id: string, resolve = false): Promise<string> {
  const r = await api.get<{ config?: string; error?: string }>(
    `/api/apps/${encodeURIComponent(id)}/effective${resolve ? '?resolve=1' : ''}`,
  )
  if (r.error) throw new Error(r.error)
  return r.config ?? ''
}

/** Whether an app's reference store carries a newer docker-compose.yml than the
 *  installed copy. Resolved from the store reference recorded in the override. */
export interface UpdateStatus {
  /** The app records where it was installed from (needed to check at all). */
  has_ref: boolean
  /** The store's compose differs from the installed one. */
  available: boolean
  /** The whole reference as one locator — `<store>/-/<folder>/<app id>`, the same
   *  grammar the store's deep links use. This is what the Update tab shows and
   *  takes back; the two fields below are the same thing taken apart. */
  ref: string
  /** Reference store URL and the catalog id within it. */
  store: string
  store_app_id: string
  /** Non-fatal lookup failure (store unreachable, app removed from catalog, …). */
  error?: string
  /** What the update does to the app's data (docs/migrations.md): the version the
   *  data is at and the one the new version expects, with the steps between. Absent
   *  when the update leaves the data alone — every ordinary update. */
  data_from?: number
  data_to?: number
  migrations?: { id: number; name: string; title?: Localized }[]
  /** Set when a step on the way is marked as manual by the app's maintainer: the
   *  update is refused, and this is their message (en_us, in Maison's words). */
  blocked?: string
  /** The maintainer's own text for that step, every locale of it. */
  blocked_message?: Localized
}

/** Check whether the app's reference store has a newer compose than installed. */
export function checkUpdate(id: string): Promise<UpdateStatus> {
  return api.get<UpdateStatus>(`/api/apps/${encodeURIComponent(id)}/update`)
}

/** Pull the store's current compose (when it differs) and bring the stack back
 *  up. `noBackup` skips the backup — the owner's choice, made with the Backup box
 *  beside "Update now". An update refused because the app could not be backed up is
 *  an ApiError whose body has `no_rollback`, or — run in the background — a refused
 *  item on the Updates page. */
export async function applyUpdate(
  id: string,
  noBackup = false,
): Promise<{ updated: boolean; started?: boolean; warning?: string }> {
  const res = await api.post<{ status: string; updated: boolean; started?: boolean; warning?: string }>(
    `/api/apps/${encodeURIComponent(id)}/update`,
    noBackup ? { noBackup: true } : undefined,
  )
  await loadApps()
  // `started`: an update runs in the background (its backup can take as long as an
  // upload, a data conversion hours); the tile's progress bar and the Updates page
  // follow it.
  return { updated: res?.updated ?? false, started: res?.started, warning: res?.warning }
}

/** Point the app at a different store, app or folder. The reference is resolved
 *  server-side before it is recorded, so a locator that does not exist throws here
 *  rather than turning into a "couldn't check" later. The stack keeps running —
 *  only where the next update comes from changed — and the fresh status against the
 *  new store comes back. */
export function setUpdateRef(id: string, ref: string): Promise<UpdateStatus> {
  return api.put<UpdateStatus>(`/api/apps/${encodeURIComponent(id)}/update/ref`, { ref })
}

/** One container of a multi-service stack, with live state and health. */
export interface AppService {
  service: string
  container_id: string
  state: string
  health: string // '', starting, healthy, unhealthy
}

export function getServices(id: string): Promise<AppService[]> {
  return api.get<AppService[]>(`/api/apps/${encodeURIComponent(id)}/services`)
}

/** Compute an app's web URL, if it has a reachable one. */
export function appUrl(a: App): string {
  // A webui-host resolves the full URL server-side; use it verbatim. Otherwise a
  // port names a port on the box's own host.
  if (a.url) return a.url
  const scheme = a.scheme || 'http'
  const index = a.index && a.index !== '/' ? a.index : ''
  if (a.port) return `${scheme}://${location.hostname}:${a.port}${index}`
  return ''
}

/** Open an app, or warn if it has no reachable URL.
 *
 *  Optimistic routing (matching CasaOS): an app the tile already shows as cleanly
 *  running goes STRAIGHT to its URL — no interstitial, so the nominal case has no
 *  flash. Only an app that isn't cleanly up (stopped, partial, still health-check
 *  "starting", or unhealthy) goes through `/launch?app=<id>`, which starts the
 *  stack if needed and holds a friendly status page until it responds — instead of
 *  a browser connection error or a gateway 502. Both paths open synchronously in
 *  the click handler so the popup blocker stays clear.
 *
 *  New tab or this one is the operator's choice (Settings → "open apps in a new
 *  tab"), except that a modifier/middle click always means a new tab because that
 *  is what it means everywhere else in the browser. `ev` carries that click. */
export function openApp(a: App, ev?: MouseEvent): void {
  const url = appUrl(a)
  if (url === '') {
    alert(
      `${a.name} has no directly reachable web address.\n\n` +
        `It exposes its UI to a reverse-proxy/gateway rather than a host port. ` +
        `Add a published port via the app's Settings → override, or put it behind a gateway.`,
    )
    return
  }
  const cleanlyUp = a.status === 'running' && a.health !== 'starting' && a.health !== 'unhealthy'
  const target = cleanlyUp ? url : `/launch?app=${encodeURIComponent(a.id)}`
  openTarget(target, ev)
}

/** Whether this click should open a new tab regardless of the setting: the
 *  browser's own conventions for "somewhere else" — ctrl/cmd/shift-click and the
 *  middle button — outrank a preference about the plain left click. */
function forcesNewTab(ev?: MouseEvent): boolean {
  return !!ev && (ev.button === 1 || ev.ctrlKey || ev.metaKey || ev.shiftKey)
}

/** Navigate to a tile's target, honouring the new-tab preference.
 *
 *  Same-tab uses assign, not replace: the dashboard has to stay in history for
 *  Back to come back to it, which is the entire point of the option. */
export function openTarget(url: string, ev?: MouseEvent): void {
  if (get(settings).open_in_new_tab || forcesNewTab(ev)) window.open(url, '_blank', 'noopener')
  else window.location.assign(url)
}
