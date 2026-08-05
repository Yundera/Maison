// Minimal path router for the URLs Maison exposes:
//
//   /                        the dashboard
//   /store                   the store, browsing the catalog
//   /store/<app>             the store, on that app's detail page
//   /store/<app>?store=<url> ...pinned to one store, which may be a store the
//                            user has not added (AppDetail warns before install)
//   /settings/<section>      the settings page, on that section
//
// The server serves index.html for any unknown path (internal/server/spa.go), so
// these deep links load the SPA directly. The legacy `#store` / `#settings:<id>`
// hash links still work — App.svelte reads them once at mount.
//
// State lives in the `storeOpen` / `storeApp` / `settingsOpen` / `settingsSection`
// ui stores; this module is the only place that reads or writes the URL.
import { storeOpen, storeApp, settingsOpen, settingsSection } from './stores/ui'

// The settings sections, in the order the page's rail lists them. This is the URL
// vocabulary, so it lives here rather than in the component — which means the rail
// and the deep links cannot drift, and adding a section is one entry here plus its
// panel in SettingsPage.
export const SETTINGS_SECTIONS = ['domain', 'env', 'backups', 'cloud'] as const
export type SettingsSection = (typeof SETTINGS_SECTIONS)[number]

const isSection = (s: string): s is SettingsSection =>
  (SETTINGS_SECTIONS as readonly string[]).includes(s)

export type View = 'dashboard' | 'store' | 'settings'

export interface Route {
  view: View
  app: string // store: '' = catalog
  storeURL: string // store: '' = merged catalog
  section: SettingsSection // settings: which section is showing
}

const DASHBOARD: Route = { view: 'dashboard', app: '', storeURL: '', section: 'domain' }

export function parse(url: URL): Route {
  const seg = url.pathname.split('/').filter(Boolean)
  if (seg[0] === 'store') {
    return {
      ...DASHBOARD,
      view: 'store',
      app: seg[1] ? decodeURIComponent(seg[1]) : '',
      storeURL: seg[1] ? (url.searchParams.get('store') ?? '') : '',
    }
  }
  if (seg[0] === 'settings') {
    // A bare /settings, or a section from a link written against a later version,
    // opens the first one — href() then normalises the URL to match what's shown.
    return { ...DASHBOARD, view: 'settings', section: isSection(seg[1] ?? '') ? (seg[1] as SettingsSection) : SETTINGS_SECTIONS[0] }
  }
  return DASHBOARD
}

export function href(r: Route): string {
  if (r.view === 'settings') return `/settings/${r.section}`
  if (r.view !== 'store') return '/'
  if (!r.app) return '/store'
  const q = r.storeURL ? `?store=${encodeURIComponent(r.storeURL)}` : ''
  return `/store/${encodeURIComponent(r.app)}${q}`
}

/** Apply a route to the ui stores (does not touch the URL). */
function apply(r: Route) {
  storeOpen.set(r.view === 'store')
  storeApp.set(r.view === 'store' && r.app ? { store: r.storeURL, app: r.app } : null)
  settingsOpen.set(r.view === 'settings')
  if (r.view === 'settings') settingsSection.set(r.section)
}

// Each history entry we create carries how deep into *our* stack it is. depth 0 is
// the entry the SPA loaded on; the one below it belongs to wherever the user came
// from, so history.back() from depth 0 would leave Maison altogether. Keeping the
// depth in the entry (rather than a counter) keeps it right under Back *and* Forward.
type Entry = Route & { depth: number }

const depth = (): number => (history.state as Entry | null)?.depth ?? 0

/** Navigate: push the route onto the history stack and apply it. */
export function go(r: Route) {
  if (href(r) !== location.pathname + location.search) {
    history.pushState({ ...r, depth: depth() + 1 } satisfies Entry, '', href(r))
  }
  apply(r)
}

export const openStore = () => go({ ...DASHBOARD, view: 'store' })
export const openStoreApp = (app: string, storeURL = '') => go({ ...DASHBOARD, view: 'store', app, storeURL })
export const closeStore = () => go(DASHBOARD)

export const openSettings = (section: SettingsSection = SETTINGS_SECTIONS[0]) =>
  go({ ...DASHBOARD, view: 'settings', section })
export const closeSettings = () => go(DASHBOARD)

/** Back out of an app detail to the catalog. Steps back through history when the
 *  detail page is one we pushed, so the arrow and the browser's Back button agree
 *  — but a deep link opened in a fresh tab has nothing of ours to step back to, so
 *  there it pushes the catalog instead of navigating off the site. */
export function backToCatalog() {
  if (depth() > 0) history.back()
  else openStore()
}

/** Start routing: apply the current URL and follow Back/Forward. Returns an
 *  unsubscribe for onMount. */
export function start(): () => void {
  const onPop = (e: PopStateEvent) => apply((e.state as Entry | null) ?? parse(new URL(location.href)))
  const initial = parse(new URL(location.href))
  history.replaceState({ ...initial, depth: 0 } satisfies Entry, '', href(initial))
  apply(initial)
  addEventListener('popstate', onPop)
  return () => removeEventListener('popstate', onPop)
}
