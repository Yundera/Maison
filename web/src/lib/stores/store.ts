import { api } from '../api/client'
import { refQuery, type StoreRef } from '../storeref'
import type { Localized } from '../i18n'

export interface StoreApp {
  id: string
  /** The en_us title, else the catalog id — show `title` through $loc first. */
  name: string
  /** The app's title, tagline and description in every locale it declares. */
  title?: Localized
  tagline: Localized
  description: Localized
  icon: string
  thumbnail: string
  screenshots: string[]
  category: string
  /** The catalog id of the app this one extends, from its x-compose-app `parent`.
   *  Unverified: the named app may be in another store or in none, in which case
   *  this app is browsed and installed like any other. */
  parent?: string
  developer: string
  min_memory?: number
  /** What the app needs from the box (x-compose-app architectures / requires /
   *  resources), and how this box measures up (`compat`). See requirements.ts. */
  architectures?: string[]
  requires?: string[]
  resources?: Resources
  compat?: Compat
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
  /** What this box offers apps. Absent on an older server. */
  host?: Host
}

/** One tier of an app's `resources`; sizes are bytes, cpu is cores. */
export interface ResourceSet {
  cpu?: number
  memory?: number
  disk?: number
}

export interface Resources {
  min?: ResourceSet
  recommended?: ResourceSet
}

/** One requirement the box does not meet. `code` is a capability name, or
 *  arch / cpu / memory / disk; need and have are display values. */
export interface Reason {
  code: string
  need?: string
  have?: string
}

/** How an app fits this box. Absent fields mean nothing short. */
export interface Compat {
  /** Built for other architectures: never installable here. */
  blocked?: boolean
  /** Capabilities the box lacks (and the architecture, when blocked). */
  unsupported?: Reason[]
  below_min?: Reason[]
  below_recommended?: Reason[]
  /** Required capabilities Maison could not determine. */
  uncertain?: string[]
}

export type Tri = 'yes' | 'no' | 'unknown'

export interface Capability {
  name: string
  detected: { value: Tri; source?: string }
  setting: 'auto' | 'yes' | 'no'
  effective: Tri
}

export interface Host {
  arch: string
  cpu: number
  memory: number
  disk_free: number
  capabilities: Capability[]
}

/** The store hides an app the box cannot run, unless "show all" is on. */
export function isHidden(c?: Compat): boolean {
  return !!c && (!!c.blocked || (c.unsupported?.length ?? 0) > 0)
}

/** Installing takes an explicit acknowledgement. */
export function needsAck(c?: Compat): boolean {
  return !!c && ((c.unsupported?.length ?? 0) > 0 || (c.below_min?.length ?? 0) > 0)
}

/** Settings › App stores › Capabilities. */
export interface CapabilitiesView extends Host {
  managedBy?: string
}

export function fetchCapabilities(refresh = false): Promise<CapabilitiesView> {
  return api.get<CapabilitiesView>('/api/settings/capabilities' + (refresh ? '?refresh=1' : ''))
}

/** Replaces the overrides; `auto` (or absent) is no override. */
export function saveCapabilities(overrides: Record<string, string>): Promise<CapabilitiesView> {
  return api.put<CapabilitiesView>('/api/settings/capabilities', overrides)
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
  signal?: AbortSignal,
): Promise<{ project: string; engines: BackupEngine[] }> {
  return api.get<{ project: string; engines: BackupEngine[] }>(
    `/api/store/${encodeURIComponent(ref.id)}/backups${refQuery(ref)}`,
    signal,
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
  acceptRequirements = false,
): Promise<{ status: string; id: string }> {
  const body: Record<string, unknown> = {}
  if (from) {
    body.from_backup = from.name
    body.engine = from.engine ?? ''
  }
  if (acceptRequirements) body.accept_requirements = true
  return api.post<{ status: string; id: string }>(
    `/api/store/${encodeURIComponent(ref.id)}/install${refQuery(ref)}`,
    Object.keys(body).length ? body : undefined,
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
  /** True when `name` is the operator's own, set in settings, rather than the
   *  store's self-declared one. Absent means it is the store's. */
  custom?: boolean
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
/** Rename one store, for display only. An empty name drops the custom name and
 *  the store goes back to calling itself whatever its store.json says. Nothing is
 *  re-downloaded: the rename applies to the catalog already in memory. */
export function renameStoreSource(url: string, name: string): Promise<StoreSources> {
  return api.put<StoreSources>('/api/store/sources/name', { url, name })
}
