import { api } from '../api/client'

// Settings › Resources › Storage — see internal/server/storage.go.

export interface DiskEntry {
  name: string
  bytes: number
}

/** A walk of the data root (internal/diskuse.Result). While a rescan runs, the
 *  totals are the previous scan's and progress_bytes counts the new one. */
export interface DataScan {
  status: 'idle' | 'running' | 'done' | 'error'
  started_at?: string
  finished_at?: string
  error?: string
  bytes: number
  files: number
  progress_bytes?: number
  top: DiskEntry[]
  apps: DiskEntry[]
  skipped?: string[]
  unreadable?: number
}

export interface DockerUsage {
  images_bytes: number
  images_count: number
  containers_bytes: number
  containers_count: number
  volumes_bytes: number
  volumes_count: number
  build_cache_bytes: number
  total_bytes: number
  reclaimable_cache_bytes: number
}

export interface StorageUsage {
  data_root: string
  size_bytes: number
  used_bytes: number
  avail_bytes: number
  data_own_filesystem: boolean
  data: DataScan
  docker: DockerUsage | null
  docker_error?: string
  /** Docker's figures are being measured; poll until it clears. */
  docker_pending: boolean
}

/** A Go zero time ("0001-01-01…") arrives for "never"; treat it as absent. */
export function validTime(iso?: string): Date | null {
  if (!iso) return null
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? null : d
}

export interface OrphanContainer {
  id: string
  name: string
  service?: string
  state: string
  image: string
}

export interface Orphan {
  key: string
  project?: string
  reason: 'folder_gone' | 'standalone'
  working_dir?: string
  running: boolean
  containers: OrphanContainer[]
}

export interface CleanupPlan {
  images: { id: string; tags: string[] | null; bytes: number }[]
  images_bytes: number
  networks: { id: string; name: string }[]
  orphans: Orphan[]
  images_kept: number
}

export interface CleanupRun {
  status: 'idle' | 'running' | 'done' | 'error'
  kind?: 'clean' | 'orphans'
  started_at?: string
  finished_at?: string
  images: number
  networks: number
  containers: number
  build_cache_bytes: number
  freed_bytes: number
  errors?: string[]
}

export interface CleanupState {
  plan: CleanupPlan
  reclaimable_cache_bytes: number
  run: CleanupRun
  blocked?: string
}

export const fetchStorage = () => api.get<StorageUsage>('/api/system/storage')
export const startScan = () => api.post<DataScan>('/api/system/storage/scan')
export const fetchCleanup = () => api.get<CleanupState>('/api/system/cleanup')
export const runCleanup = () => api.post<CleanupRun>('/api/system/cleanup')
export const removeOrphans = (keys: string[]) =>
  api.post<CleanupRun>('/api/system/cleanup/orphans', { keys })
