import { writable, derived } from 'svelte/store'
import { api } from '../api/client'
import { locale } from '../i18n'

// An additional domain every app is published on, on top of the primary one its
// compose already routes. `domain` stays templated (`${APP_PUBLIC_IP_DASH}.sslip.io`):
// it goes into the app's Caddy label as-is and is resolved by compose, so the
// route follows the box changing IP.
export interface Domain {
  name: string
  domain: string
  directives?: Record<string, string>
}

// Operator preferences, persisted server-side via /api/settings so they follow
// the server rather than a single browser.
export interface Settings {
  wallpaper: string
  language: string
  widgets: Record<string, boolean>
  domains: Domain[]
}

const DEFAULTS: Settings = {
  wallpaper: '/wallpapers/default_wallpaper.jpg',
  language: 'en_us',
  widgets: { clock: true, system: true, storage: true },
  domains: [],
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

/**
 * The domains editor's payload: the additional domains, plus the primary domain
 * they are added to.
 *
 * The primary one travels with the list because the list alone does not say what
 * it is a list *of* — an extra name is meaningless without the name it is extra
 * to. It is read-only here: it comes from each app's own compose route
 * (`myapp-${APP_DOMAIN}`), which Maison never edits.
 */
export interface DomainsView {
  /** The placeholder an app's own route is written with, e.g. `${APP_DOMAIN}`. */
  primaryToken: string
  /** What that placeholder resolves to on this box; '' when the box has no domain. */
  primaryHost: string
  domains: Domain[]
  /** The .env.app routing variables a host template may interpolate, so the
   *  editor can resolve a preview before the value is written onto every app. */
  vars: Record<string, string>
}

export const loadDomains = () => api.get<DomainsView>('/api/settings/domains')

/**
 * Replace the additional domains apps are published on.
 *
 * Domains have their own endpoint rather than riding the debounced save above:
 * the server rewrites every app's Caddy labels and recreates its containers to
 * pick them up, which is not something to fire off between keystrokes — which is
 * also why the editor batches an editing session behind an explicit apply rather
 * than saving each field. Resolves once the settings are saved; the republish
 * continues in the background, and the tiles show it.
 */
export async function saveDomains(list: Domain[]): Promise<DomainsView> {
  const view = await api.put<DomainsView>('/api/settings/domains', list)
  settings.update((s) => ({ ...s, domains: view?.domains ?? [] }))
  return view
}

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
 * Nothing restarts: unlike domains, which rewrite Caddy labels, these variables are
 * ensured into each app's own .env on its next start. The editor says so.
 */
export const saveAppEnv = (text: string) => api.put<AppEnvFile>('/api/settings/appenv', { text })
