import { api } from '../api/client'
import { refQuery, type StoreRef } from '../storeref'

export interface StoreApp {
  id: string
  name: string
  tagline: string
  description: string
  icon: string
  thumbnail: string
  screenshots: string[]
  category: string
  developer: string
  author: string
  min_memory?: number
  store: string
  apps_path?: string
}

export interface StoreData {
  apps: StoreApp[]
  categories: string[]
  recommend: string[]
}

export function fetchStore(): Promise<StoreData> {
  return api.get<StoreData>('/api/store')
}

/** A reference's locator pins a lookup to one store — which may be a store the
 *  user has not added (deep links can carry one). Without one the merged catalog
 *  answers. The id stays a single path segment and the rest of the reference rides
 *  in the query, because two of these endpoints have segments after the id. */
export function fetchStoreApp(ref: StoreRef): Promise<StoreApp> {
  return api.get<StoreApp>(`/api/store/app/${encodeURIComponent(ref.id)}${refQuery(ref)}`)
}

/** One archive of an app, as offered when installing over existing data. Maison
 *  never deletes app data — uninstall moves the folder into
 *  `.backups/<app>/<stamp>` — so a previously removed app can be reinstalled on
 *  top of it.
 *
 *  Deliberately narrower than the `Backup` in stores/backups.ts: this list comes
 *  from the store's install-click path, which skips the per-archive tree walk that
 *  sizing a folder archive costs. */
export interface Backup {
  name: string // on-disk base name, e.g. 2026-07-10_153045 or that + .zip
  date: string // YYYY-MM-DD
  zip: boolean // compressed archive rather than a plain renamed folder
  size: number // bytes; only known for zips (0 for folders)
  tier: 'local' | 'remote' // where it is; a backup belongs to one engine — see stores/backups.ts
  engine?: string // which engine holds it
}

/** The backups of a store app. The server resolves the compose project name (it
 *  can come from the compose file's own `name:`), so this is not derivable client-
 *  side from the catalog id alone. */
export function fetchStoreBackups(
  ref: StoreRef,
): Promise<{ project: string; backups: Backup[] }> {
  return api.get<{ project: string; backups: Backup[] }>(
    `/api/store/${encodeURIComponent(ref.id)}/backups${refQuery(ref)}`,
  )
}

/** Kick off a detached install. Resolves once the server has *started* it (not
 *  when it finishes) with the app's compose project id; progress then arrives on
 *  the live "apps" channel as the tile's download/start bars.
 *
 *  fromBackup names one of the app's backups (see fetchStoreBackups): it is
 *  restored as the app's folder first, so the app returns with its old data and
 *  .env instead of a clean slate. */
export function installApp(
  ref: StoreRef,
  fromBackup?: string,
): Promise<{ status: string; id: string }> {
  return api.post<{ status: string; id: string }>(
    `/api/store/${encodeURIComponent(ref.id)}/install${refQuery(ref)}`,
    fromBackup ? { from_backup: fromBackup } : undefined,
  )
}

/** The source list after a change.
 *
 *  `warning` means the list *was* applied but at least one store could not be
 *  fetched — a mistyped URL, an origin that is down. It is not a thrown error
 *  because the change stuck and the list on screen must still update; callers
 *  show it next to the list instead of replacing it. An outright failure (the ⟳
 *  button) arrives as a rejected promise, the usual way. */
export interface StoreSources {
  sources: string[]
  warning?: string
}

export function fetchStoreSources(): Promise<StoreSources> {
  return api.get<StoreSources>('/api/store/sources')
}
export function addStoreSource(url: string): Promise<StoreSources> {
  return api.post<StoreSources>('/api/store/sources', { url })
}
export function removeStoreSource(url: string): Promise<StoreSources> {
  return api.del<StoreSources>('/api/store/sources', { url })
}
export function refreshStoreSource(url: string): Promise<StoreSources> {
  return api.post<StoreSources>('/api/store/sources/refresh', { url })
}
