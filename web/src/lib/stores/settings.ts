import { writable, derived } from 'svelte/store'
import { api } from '../api/client'
import { locale } from '../i18n'

// A domain apps are published on, in the routing setting's labels renderer. `domain`
// is a host template — `{name}-${APP_DOMAIN}` — with the route's name in place of
// {name}; it stays templated in the app's labels and is resolved by compose, so the
// route follows the box changing IP. `labels` are added to every route on it (TLS
// lives here: `import = gateway_tls`); an empty value deletes that key.
export interface Domain {
  name: string
  domain: string
  labels?: Record<string, string>
}

/** How the box reaches its apps (internal/routing). */
export interface RoutingSetting {
  renderer: string
  bind?: string
  'url-host'?: string
  'port-range'?: [number, number]
  dialect?: string
  /** labels renderer: the first entry is the primary. */
  domains?: Domain[]
  'url-template'?: string
}

/** One route reached through one target, as the server's route table has it. */
export interface RouteEntry {
  name: string
  service: string
  'upstream-port': number
  target: string
  primary: boolean
  host?: string
  'host-port'?: number
  url?: string
}

/** The routing editor's payload. */
export interface RoutingView {
  /** What is stored; null = the box runs on its default. */
  setting: RoutingSetting | null
  /** What the box runs on, defaults filled in. */
  effective: RoutingSetting
  managedBy?: string
  dialects: string[]
  /** The .env.app routing variables a host template may interpolate. */
  vars: Record<string, string>
  /** The address the dashboard was first opened at: ports mode's url-host fallback. */
  seenHost?: string
  /** Ports published on every address of a box with a public IP. */
  publicWarning: boolean
  /** What the setting generates for a sample app, rendered by the real generator. */
  preview: { app: string; override: string; routes: RouteEntry[]; vars: Record<string, string>; error?: string }
}

// Where Maison's outbound mail goes — the backup failure alerts and the
// encryption-key mail. Absent means the box adds nothing to whatever relay the
// deployment provisioned; a host set here carries its own credentials with it,
// while `from` and `to` resolve on their own (see usersettings.EffectiveSMTP).
export interface SmtpConfig {
  host: string
  port: number
  user?: string
  pass?: string
  from: string
  to: string
  security?: string
}

// Operator preferences, persisted server-side via /api/settings so they follow
// the server rather than a single browser.
export interface Settings {
  wallpaper: string
  language: string
  widgets: Record<string, boolean>
  /** Whether the resource-history recorder runs. It is the only thing Maison
   *  measures with nobody watching, so it is the only thing with an off switch —
   *  see the Recording card on the Resources page. */
  metrics_history: boolean
  /** How a tile click opens an app: a new tab (the default) or the current one.
   *  Opening in place is what makes the browser's Back button return to the
   *  dashboard instead of leaving a tab behind per app. */
  open_in_new_tab: boolean
  /** Mail transport. It lived under the backup configuration until it moved here,
   *  because it is a property of the box rather than of the backup schedule. */
  smtp?: SmtpConfig
  /** Settings the deployment owns, by name → owner (e.g. `{ smtp: 'mesh' }`). A
   *  managed setting is read-only here: the server drops any edit of it, and the
   *  deployment re-asserts it on every self-check. */
  managed?: Record<string, string>
}

/** A password or token the server holds for us: the API returns `${secret:name}`
 *  (or a mask) in its place, and sending it back unchanged keeps it. */
export const isStoredSecret = (v?: string) => !!v && (/^\$\{secret:[a-z][a-z0-9_]*\}$/.test(v) || v === '********')

const DEFAULTS: Settings = {
  wallpaper: '/wallpapers/default_wallpaper.jpg',
  language: 'en_us',
  widgets: { clock: true, system: true, storage: true },
  metrics_history: true,
  open_in_new_tab: true,
}

export const settings = writable<Settings>({ ...DEFAULTS })

let loaded = false
let saveTimer: number | undefined

/** Load persisted settings from the server (call once on startup). */
export async function loadSettings(): Promise<void> {
  try {
    const s = await api.get<Settings>('/api/settings')
    settings.set({ ...DEFAULTS, ...s })
  } catch {
    /* keep defaults */
  } finally {
    loaded = true
  }
}

// Keep the active locale in sync, and persist changes (debounced) after load.
settings.subscribe((s) => {
  locale.set(s.language)
  if (!loaded) return
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => {
    api.put('/api/settings', s).catch(() => {})
  }, 400)
})

export const loadRouting = () => api.get<RoutingView>('/api/settings/routing')

/** Render the preview for a setting being edited, without saving it. */
export const previewRouting = (s: RoutingSetting) => api.post<RoutingView>('/api/settings/routing/preview', s)

/**
 * Replace the routing setting — null goes back to the box's default.
 *
 * Routing has its own endpoint rather than riding the debounced save above: the
 * server rewrites every app's override and recreates its containers to pick it up,
 * which is not something to fire off between keystrokes — which is also why the
 * editor batches an editing session behind an explicit apply. Resolves once the
 * setting is saved; the republish continues in the background, and the tiles show it.
 */
export const saveRouting = (s: RoutingSetting | null) => api.put<RoutingView>('/api/settings/routing', s)

export const wallpaper = derived(settings, ($s) => $s.wallpaper)

/**
 * The deployment's .env.app — the variables Maison forwards into every app.
 *
 * It travels as text, not as a key/value list: the file's comments are its
 * documentation and its empty values are meaningful ("the deployment does not have
 * this"), so a round-trip through a map would destroy most of it. `ignored` names
 * the keys the text sets that Maison computes per app anyway and will overwrite.
 */
export interface AppEnvFile {
  text: string
  /** Where the file lives inside the server container — it follows STATE_DIR,
   *  so the server reports it rather than the UI describing it. */
  path?: string
  ignored?: string[]
}

export const loadAppEnv = () => api.get<AppEnvFile>('/api/settings/appenv')

/**
 * Replace .env.app. Rejects (400) rather than saving text Maison would read back
 * differently — a line that isn't KEY=VALUE, a bad name, a duplicate key.
 *
 * Nothing restarts: unlike routing, which rewrites every app's override, these variables are
 * ensured into each app's own .env on its next start. The editor says so.
 */
export const saveAppEnv = (text: string) => api.put<AppEnvFile>('/api/settings/appenv', { text })
