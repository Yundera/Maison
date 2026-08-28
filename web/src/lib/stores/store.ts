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
  /** What the store calls itself (store.json), falling back to its URL. */
  store_name?: string
  /** True for the copy that answers the bare id. Two stores can ship the same app
   *  folder and only one of them wins that; the others are still in the payload,
   *  found by naming their store. Absent means primary (an older server). */
  primary?: boolean
}

export interface StoreData {
  /** Every configured store's copy of every app — NOT merged. An app shipped by
   *  two stores appears twice, once per store, each carrying its own version and
   *  metadata; the catalog is browsed grouped by store. `primary` still marks the
   *  copy a bare id resolves to. */
  apps: StoreApp[]
  categories: string[]
  recommend: string[]
  /** The configured stores, in the order the box has them, each named as it names
   *  itself in store.json (falling back to its URL). The grouping follows this. */
  sources?: StoreSource[]
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

/** One backup of an app, as offered when installing over existing data. Maison
 *  never deletes app data — uninstalling backs the app up and archives its folder —
 *  so a previously removed app can be reinstalled on top of one of its backups.
 *
 *  Deliberately narrower than the `Backup` in stores/backups.ts: this list comes
 *  from the store's install-click path, which skips the per-archive tree walk that
 *  sizing a folder archive costs. */
export interface Backup {
  name: string // on-disk base name, e.g. 2026-07-10_153045 or that + .zip
  stamp: string // the canonical YYYY-MM-DD_HHMMSS identity, without any .zip suffix
  date: string // YYYY-MM-DD
  zip: boolean // compressed archive rather than a plain renamed folder
  size: number // bytes; only known for zips (0 for folders)
  tier: 'local' | 'remote' // where it is; a backup belongs to one engine — see stores/backups.ts
  engine?: string // which engine holds it
}

/** One engine's group in the install-from-backup picker.
 *
 *  Grouped rather than merged, matching the Backups page's tab-per-engine: a stamp
 *  held by two engines is two backups, written at different times to different
 *  places, and installing on one is not installing on the other. Only engines that
 *  actually hold something for this app are returned. */
export interface BackupEngine {
  engine: string
  /** The deployment's name for it; absent when nobody named it, and the client then
   *  falls back to `engineLabel` in stores/backups.ts. */
  name?: string
  offsite: boolean
  backups: Backup[]
}

/** The backups of a store app, grouped by the engine holding each. The server
 *  resolves the compose project name (it can come from the compose file's own
 *  `name:`), so this is not derivable client-side from the catalog id alone. */
export function fetchStoreBackups(
  ref: StoreRef,
): Promise<{ project: string; engines: BackupEngine[] }> {
  return api.get<{ project: string; engines: BackupEngine[] }>(
    `/api/store/${encodeURIComponent(ref.id)}/backups${refQuery(ref)}`,
  )
}

/** Kick off a detached install. Resolves once the server has *started* it (not
 *  when it finishes) with the app's compose project id; progress then arrives on
 *  the live "apps" channel as the tile's download/start bars.
 *
 *  `from` names one of the app's backups and the engine holding it (see
 *  fetchStoreBackups): it is restored as the app's folder first — downloaded first
 *  if that engine is a repository — so the app returns with its old data and .env
 *  instead of a clean slate. The engine is sent because two engines can hold the
 *  same stamp and they are different backups. */
export function installApp(
  ref: StoreRef,
  from?: { name: string; engine?: string },
): Promise<{ status: string; id: string }> {
  return api.post<{ status: string; id: string }>(
    `/api/store/${encodeURIComponent(ref.id)}/install${refQuery(ref)}`,
    from ? { from_backup: from.name, engine: from.engine ?? '' } : undefined,
  )
}

/** The source list after a change.
 *
 *  `warning` means the list *was* applied but at least one store could not be
 *  fetched — a mistyped URL, an origin that is down. It is not a thrown error
 *  because the change stuck and the list on screen must still update; callers
 *  show it next to the list instead of replacing it. An outright failure (the ⟳
 *  button) arrives as a rejected promise, the usual way. */
/** One configured store. `name` is what it calls itself in store.json, falling
 *  back to its URL — never derived from the URL's shape, which only means
 *  anything on the one forge whose layout it copies. */
export interface StoreSource {
  url: string
  name: string
}

export interface StoreSources {
  sources: StoreSource[]
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
