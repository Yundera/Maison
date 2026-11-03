import { writable } from 'svelte/store'

import type { StoreRef } from '../storeref'
import type { App } from './apps'
import type { Account, Compat, Cost, Input } from './store'

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

// The app whose credentials modal is open (null = closed). `launch` is true when
// the modal stands in front of opening the app (a click on a tile whose
// credentials are still pending): it then offers "Open app". From the tile menu
// it is false — the modal is only a reminder.
export const credentialsApp = writable<{ app: App; launch: boolean } | null>(null)

// The app pending uninstall confirmation (null = no dialog).
export const uninstallTarget = writable<{ id: string; name: string } | null>(null)

// True while a tile is being dragged (and for one tick after the drop). A mouse
// drop still fires a trailing `click` on the tile, which would otherwise open the
// app; tiles consult this flag in their click handler so a reorder never opens.
export const tileDragging = writable(false)

// The install waiting on the operator's acknowledgement that this box falls short of
// the app's requirements (null = no dialog). confirmInstall opens it and resolves
// with the answer; InstallConfirmDialog is what answers.
export const installConfirm = writable<{
  name: string
  compat: Compat
  resolve: (ok: boolean) => void
} | null>(null)

// The install waiting on the values only the user can give the app (its declared
// inputs), with what it costs and the accounts it needs (null = no dialog).
// askInstallInputs resolves with the values typed, {} when the operator chose to
// set the app up later, or null when they cancelled the install.
export interface InstallNeeds {
  inputs?: Input[]
  cost?: Cost
  accounts?: Account[]
}
export const installInputs = writable<{
  name: string
  needs: InstallNeeds
  resolve: (values: Record<string, string> | null) => void
} | null>(null)

export function askInstallInputs(name: string, needs: InstallNeeds): Promise<Record<string, string> | null> {
  return new Promise((resolve) => {
    installInputs.set({
      name,
      needs,
      resolve: (values) => {
        installInputs.set(null)
        resolve(values)
      },
    })
  })
}

// The installed app whose required inputs are still empty, being set up from its
// tile (null = no dialog). launch opens the app once the values are saved.
export const setupApp = writable<{ app: App; launch: boolean } | null>(null)

export function confirmInstall(name: string, compat: Compat): Promise<boolean> {
  return new Promise((resolve) => {
    installConfirm.set({
      name,
      compat,
      resolve: (ok) => {
        installConfirm.set(null)
        resolve(ok)
      },
    })
  })
}
