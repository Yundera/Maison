import { get, writable } from 'svelte/store'
import { live } from '../live/ws'
import { api } from '../api/client'

export interface App {
  id: string
  name: string
  icon: string
  status: 'running' | 'stopped' | 'partial'
  managed: boolean
  store?: string
  scheme?: string
  hostname?: string
  port?: string
  index?: string
  category?: string
  /** Which dashboard grid this tile belongs in, from the app's x-compose-app
   *  `view`. Absent means the ordinary grid; "hidden" apps get no tile. */
  view?: 'apps' | 'system' | 'hidden'
  /** Fully-resolved click URL from x-compose-app (webui-*); wins when set. */
  url?: string
  /** Aggregated Docker health-check verdict; drives the tile status dot.
   *  Absent when no container declares a health check. */
  health?: 'healthy' | 'unhealthy' | 'starting'
  /** A system app (view === 'system'): the tile withholds Stop and Uninstall
   *  and the backend refuses both. Restart stays available — it is how a
   *  wedged platform app is recovered without an SSH session. */
  protected?: boolean
  /** True while a lifecycle op (start/stop/restart/uninstall) is in flight:
   *  the tile shows a "…" overlay and hides its burger menu. */
  busy?: boolean
  /** True while a store install is in flight for this app: the tile shows a
   *  progress bar (download, then start) instead of being clickable. */
  installing?: boolean
  /** Image-pull progress, 0-100 (only meaningful while installing). */
  download?: number
  /** Stack-start progress, 0-100, driven by Docker (only while installing). */
  start?: number
  /** Current install/uninstall phase: pull | prepare | start | remove |
   *  archive | done | error. */
  phase?: string
  /** Set when an install failed; the tile shows the error until retried. */
  install_error?: string
  /** True while an uninstall is in flight for this app: the tile shows the same
   *  progress bar as an install, in red. */
  uninstalling?: boolean
  /** Container-removal progress, 0-100 (only while uninstalling). */
  remove?: number
  /** Folder-archiving progress, 0-100 (only while uninstalling). */
  archive?: number
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
      delete c.remove
      delete c.archive
      delete c.uninstall_error
      return c
    })
    localStorage.setItem(CACHE_KEY, JSON.stringify(clean))
  } catch {
    /* private mode / quota — cache is best-effort */
  }
}

/** Apply an authoritative snapshot: replace the grid and refresh the cache. */
function applySnapshot(list: App[]): void {
  apps.set(list)
  persistApps(list)
}

export const apps = writable<App[]>(cachedApps())

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
    const inFlight = current.some((a) => a.installing || a.uninstalling || a.busy)
    if (list.length === 0 && current.length > 0 && !inFlight) return
    applySnapshot(list)
  })
}

export async function appAction(id: string, action: 'start' | 'stop' | 'restart'): Promise<void> {
  await api.post(`/api/apps/${encodeURIComponent(id)}/${action}`)
  await loadApps()
}

/** Start uninstalling an app. Its folder is always preserved — renamed to
 *  `<app>.<date>.archive` (never deleted). When zip is true it is compressed to
 *  a `.zip` archive instead of a plain rename.
 *
 *  The request returns as soon as the uninstall is *accepted*, not when it is
 *  done: from there the tile carries its progress (a red bar, remove then
 *  archive) over the live app list, so nothing has to wait on a zip that can run
 *  for minutes. A rejection (a protected app) still surfaces here, as a throw. */
export async function uninstallApp(id: string, zip = false): Promise<void> {
  await api.del<{ status: string }>(`/api/apps/${encodeURIComponent(id)}?zip=${zip}`)
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
export type ProgressKind = 'download' | 'install' | 'uninstall' | 'backup'

export interface AppProgress {
  kind: ProgressKind
  /** 0-100 for the step in progress — not an average across the operation. */
  pct: number
  /** i18n key for the step's label ("Downloading…", "Archiving…", …). */
  label: string
}

/** The single progress bar an app shows, or null when nothing is in flight.
 *
 *  The backend measures each operation on two tracks (install: pull then
 *  bring-up; uninstall: containers then folder), but the UI shows **one bar at a
 *  time**: the step currently running, in that operation's colour. Both the
 *  dashboard tile and the store's install pill read this, so they can never
 *  disagree about what an app is doing.
 *
 *  A failed operation is not progress — it clears the bar and leaves the error
 *  (install_error / uninstall_error) to be shown instead. */
export function appProgress(a: App | undefined): AppProgress | null {
  if (!a) return null
  if (a.uninstalling) {
    const removing = (a.remove ?? 0) < 100
    return {
      kind: 'uninstall',
      pct: removing ? (a.remove ?? 0) : (a.archive ?? 0),
      label: removing ? 'removing' : 'archiving',
    }
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
    if (a.phase === 'restore') return { kind: 'backup', pct: 100, label: 'restoring' }
    if (a.phase === 'start') return { kind: 'backup', pct: 100, label: 'starting_up' }
    if (a.phase === 'compress') return { kind: 'backup', pct: a.compress ?? 0, label: 'compressing' }
    if (a.phase === 'sync') return { kind: 'backup', pct: a.sync ?? 0, label: 'syncing' }
    if (a.phase === 'copy') return { kind: 'backup', pct: a.copy ?? 0, label: 'copying' }
    // Unknown or absent phase: read the counters rather than guess a label, so a
    // phase added on the server renders as an honest bar instead of the wrong step.
    if ((a.copy ?? 0) < 100) return { kind: 'backup', pct: a.copy ?? 0, label: 'copying' }
    return { kind: 'backup', pct: a.sync ?? 0, label: 'syncing' }
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
  tips: string
}

export function getConfig(id: string): Promise<AppConfig> {
  return api.get<AppConfig>(`/api/apps/${encodeURIComponent(id)}/config`)
}

/** Save (or clear, when blank) the app's tips into its override. */
export async function setTips(id: string, tips: string): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/tips`, { tips })
}

/** Fetch the app's tips with ${VAR} references resolved (env-replaced preview). */
export async function renderTips(id: string): Promise<string> {
  const r = await api.get<{ tips: string }>(`/api/apps/${encodeURIComponent(id)}/tips`)
  return r.tips
}

export async function setConfig(id: string, override: string): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/config`, { override })
  await loadApps()
}

/** Save the opening-URL fields into the app's override (webui-* shortcut). */
export async function setWebUI(id: string, w: Omit<WebUI, 'url'>): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/webui`, w)
  await loadApps()
}

/** The app's .env, in file order — prefilled at install (PUID, DATA_ROOT, REF_*,
 *  …) and hand-editable since. It is what ${VAR} in the app's compose resolves
 *  against; it is NOT the same as a service's `environment:` block (that lives in
 *  the override form). Shares the EnvVar shape declared below. */
export function getEnv(id: string): Promise<EnvVar[]> {
  return api.get<EnvVar[]>(`/api/apps/${encodeURIComponent(id)}/env`)
}

/** Rewrite the app's .env to exactly these vars and recreate the stack — compose
 *  reads .env only when it brings the project up. */
export async function setEnv(id: string, vars: EnvVar[]): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/env`, { vars })
  await loadApps()
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

export interface OverrideForm {
  services: FormService[]
}

/** The friendly view of the app's override — one entry per compose service. */
export function getOverrideForm(id: string): Promise<OverrideForm> {
  return api.get<OverrideForm>(`/api/apps/${encodeURIComponent(id)}/override/form`)
}

/** Patch the override with the form's values and recreate the app. */
export async function setOverrideForm(id: string, form: OverrideForm): Promise<void> {
  await api.put(`/api/apps/${encodeURIComponent(id)}/override/form`, form)
  await loadApps()
}

/** Check a candidate override against the app's base compose without applying it.
 *  A rejected override is a normal response carrying Compose's own message. */
export function validateOverride(id: string, override: string): Promise<{ ok: boolean; error?: string }> {
  return api.post<{ ok: boolean; error?: string }>(
    `/api/apps/${encodeURIComponent(id)}/override/validate`,
    { override },
  )
}

/** The project as Compose resolves it: base + override, merged and interpolated. */
export async function effectiveConfig(id: string): Promise<string> {
  const r = await api.get<{ config?: string; error?: string }>(
    `/api/apps/${encodeURIComponent(id)}/effective`,
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
  /** Reference store URL and the catalog id within it. */
  store: string
  store_app_id: string
  /** Non-fatal lookup failure (store unreachable, app removed from catalog, …). */
  error?: string
}

/** Check whether the app's reference store has a newer compose than installed. */
export function checkUpdate(id: string): Promise<UpdateStatus> {
  return api.get<UpdateStatus>(`/api/apps/${encodeURIComponent(id)}/update`)
}

/** Pull the store's current compose (when it differs) and bring the stack back
 *  up. Returns true when an update was actually applied. */
export async function applyUpdate(id: string): Promise<boolean> {
  const res = await api.post<{ status: string; updated: boolean }>(
    `/api/apps/${encodeURIComponent(id)}/update`,
  )
  await loadApps()
  return res?.updated ?? false
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
  // x-compose-app (webui-*) resolves the full URL server-side; use it verbatim.
  if (a.url) return a.url
  const scheme = a.scheme || 'http'
  const index = a.index && a.index !== '/' ? a.index : ''
  if (a.hostname) return `${scheme}://${a.hostname}${index}`
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
 *  the click handler so the popup blocker stays clear. */
export function openApp(a: App): void {
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
  window.open(target, '_blank', 'noopener')
}
