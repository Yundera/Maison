import { writable } from 'svelte/store'

import type { StoreRef } from '../storeref'

// Whether the App Store modal is open. StorePanel (M3) renders when true.
export const storeOpen = writable(false)

// The store app whose detail page is open (null = browsing the catalog), as a
// store reference: the app's id, plus the store and apps folder it is pinned to
// (from /store/<locator>/-/<apps>/<id>). An empty `url` means "whichever store the
// merged catalog resolves the id to". See lib/route.ts and lib/storeref.ts.
export const storeApp = writable<StoreRef | null>(null)

// Whether the (box-wide) settings page is open, and which of its sections is
// showing. Both are driven by lib/route.ts — the page is deep-linkable at
// /settings/<section> — so set them by calling openSettings(), not directly.
//
// `settingsSection` is typed as a plain string rather than route.ts's
// SettingsSection: route.ts imports this module, and importing its type back would
// make the cycle a real one. SettingsPage does the narrowing.
export const settingsOpen = writable(false)
export const settingsSection = writable<string>('domain')

// The app whose Settings modal is open (null = closed). `tab` optionally selects
// which panel opens first — set by the launch page's deep-links (?panel=logs).
export const settingsApp = writable<{
  id: string
  name: string
  managed: boolean
  tab?: string
} | null>(null)

// Whether the per-app monitor is open — the breakdown behind the System status
// widget's arrow. MonitorPanel subscribes to the "appstats" channel while it is
// true, and nothing samples it while it is false.
export const monitorOpen = writable(false)

// The app whose rendered-Tips modal is open (null = closed).
export const tipsApp = writable<{ id: string; name: string } | null>(null)

// The app pending uninstall confirmation (null = no dialog).
export const uninstallTarget = writable<{ id: string; name: string } | null>(null)

// True while a tile is being dragged (and for one tick after the drop). A mouse
// drop still fires a trailing `click` on the tile, which would otherwise open the
// app; tiles consult this flag in their click handler so a reorder never opens.
export const tileDragging = writable(false)
