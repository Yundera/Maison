import { writable } from 'svelte/store'
import { live } from '../live/ws'
import { api } from '../api/client'

// ─── the live host breakdown (internal/system.Detailed) ────────────────────────

export interface MemStat {
  total_bytes: number
  used_bytes: number
  available_bytes: number
  cached_bytes: number
  used_percent: number
  swap_total_bytes: number
  swap_used_bytes: number
  swap_percent: number
}

export interface NetIfRate {
  iface: string
  rx_bytes: number
  tx_bytes: number
  rx_bps: number
  tx_bps: number
}

export interface DiskIORate {
  device: string
  read_bytes: number
  write_bytes: number
  read_bps: number
  write_bps: number
}

export interface FilesystemStat {
  /** The host's name for the device, e.g. /dev/sda1 — not the path we measured. */
  device: string
  /** The host's mountpoint. On a PCS the data root is a bind of a subdirectory of
   *  the root filesystem, so this usually reads "/" while local_path is "/DATA". */
  mountpoint: string
  fstype: string
  local_path: string
  size_bytes: number
  used_bytes: number
  avail_bytes: number
  used_percent: number
}

export interface ProcStat {
  pid: number
  user: string
  command: string
  cpu_percent: number
  mem_percent: number
  mem_bytes: number
}

export interface Detailed {
  at: number
  uptime: number
  cpu_count: number
  cpu_percent: number
  cpu_temp_c: number
  load1: number
  load5: number
  load15: number
  mem: MemStat
  nets: NetIfRate[]
  disks: DiskIORate[]
  filesystems: { mounts: FilesystemStat[]; unmeasured: number }
  top_processes: ProcStat[]
  /** False when the host's /proc is not mounted: the network table and the
   *  process list are then unavailable, and the page says so rather than showing
   *  this container's own figures as the box's. */
  host_proc: boolean
}

export const resources = writable<Detailed | null>(null)

/** Begin receiving the host breakdown. Returns an unsubscribe fn — call it when
 *  the page closes: the backend samples this channel only while someone is
 *  subscribed, and a round walks the whole process table. */
export function subscribeResources(): () => void {
  resources.set(null)
  return live.subscribe('resources', (d) => resources.set(d as Detailed))
}

// ─── recorded history ─────────────────────────────────────────────────────────

export interface MinAvgMax {
  min: number
  avg: number
  max: number
}

export interface Span {
  at: string
  cpu: MinAvgMax
  mem: MinAvgMax
  load1: number
  swap: number
  disk: number
  net_rx: number
  net_tx: number
  disk_read: number
  disk_write: number
  points: number
}

export interface History {
  from: number
  to: number
  /** Bucket width in ms. Points further apart than this are separated by a gap in
   *  the recording, which the graphs draw as a break rather than a straight line
   *  across a period when the box was off. */
  step_ms: number
  spans: Span[]
  enabled: boolean
  retention_ms: number
  bytes: number
}

export function fetchHistory(from: number, to: number, points = 500): Promise<History> {
  return api.get<History>(`/api/system/history?from=${from}&to=${to}&points=${points}`)
}

export function deleteHistory(): Promise<unknown> {
  return api.del('/api/system/history')
}

// ─── benchmarks ───────────────────────────────────────────────────────────────

export interface DiskResult {
  write_bps: number
  read_bps: number
  write_seconds: number
  read_seconds: number
  size_bytes: number
  target: string
  /** False when the filesystem refused O_DIRECT, which means the read figure
   *  measured the page cache rather than the disk. Shown, not hidden. */
  direct: boolean
}

export interface NetworkResult {
  download_bps: number
  upload_bps: number
  download_seconds: number
  upload_seconds: number
  size_bytes: number
  target: string
}

export type BenchStatus = 'idle' | 'running' | 'ok' | 'error'

export interface BenchState<T> {
  status: BenchStatus
  result?: T
  ran_at?: string
  error?: string
}

export interface BenchResults {
  disk: BenchState<DiskResult>
  network: BenchState<NetworkResult>
}

export const fetchBench = () => api.get<BenchResults>('/api/system/bench')
export const startDiskBench = () => api.post<BenchState<DiskResult>>('/api/system/bench/disk')
export const startNetworkBench = () => api.post<BenchState<NetworkResult>>('/api/system/bench/network')
