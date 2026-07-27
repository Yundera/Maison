import { api } from '../api/client'

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
}

export interface StoreData {
  apps: StoreApp[]
  categories: string[]
  recommend: string[]
}

export function fetchStore(): Promise<StoreData> {
  return api.get<StoreData>('/api/store')
}

/** `?store=<url>` pins a lookup to one store — which may be a store the user has
 *  not added (deep links can carry one). Omitted, the merged catalog answers. */
function storeQuery(store?: string): string {
  return store ? `?store=${encodeURIComponent(store)}` : ''
}

export function fetchStoreApp(id: string, store?: string): Promise<StoreApp> {
  return api.get<StoreApp>(`/api/store/app/${encodeURIComponent(id)}${storeQuery(store)}`)
}

/** One uninstall archive of an app, still on disk under AppData. Maison never
 *  deletes app data — uninstall renames the folder to `<app>.<date>.archive` — so
 *  a previously removed app can be reinstalled on top of its old data. */
export interface Backup {
  name: string // on-disk base name, e.g. jellyfin.2026-07-10.archive.zip
  date: string // YYYY-MM-DD
  zip: boolean // compressed archive rather than a plain renamed folder
  size: number // bytes; only known for zips (0 for folders)
}

/** The backups of a store app. The server resolves the compose project name (it
 *  can come from the compose file's own `name:`), so this is not derivable client-
 *  side from the catalog id alone. */
export function fetchStoreBackups(
  id: string,
  store?: string,
): Promise<{ project: string; backups: Backup[] }> {
  return api.get<{ project: string; backups: Backup[] }>(
    `/api/store/${encodeURIComponent(id)}/backups${storeQuery(store)}`,
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
  id: string,
  store?: string,
  fromBackup?: string,
): Promise<{ status: string; id: string }> {
  return api.post<{ status: string; id: string }>(
    `/api/store/${encodeURIComponent(id)}/install${storeQuery(store)}`,
    fromBackup ? { from_backup: fromBackup } : undefined,
  )
}

export function fetchStoreSources(): Promise<{ sources: string[] }> {
  return api.get<{ sources: string[] }>('/api/store/sources')
}
export function addStoreSource(url: string): Promise<{ sources: string[] }> {
  return api.post<{ sources: string[] }>('/api/store/sources', { url })
}
export function removeStoreSource(url: string): Promise<{ sources: string[] }> {
  return api.del<{ sources: string[] }>('/api/store/sources', { url })
}
export function refreshStoreSource(url: string): Promise<{ sources: string[] }> {
  return api.post<{ sources: string[] }>('/api/store/sources/refresh', { url })
}
