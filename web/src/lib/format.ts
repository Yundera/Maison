/** Human-readable byte size, matching CasaOS's "7.76 GB" / "386.43 GB" style. */
export function renderSize(bytes: number): string {
  if (!bytes || bytes < 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let i = 0
  let n = bytes
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  const dec = i >= 2 ? 2 : i === 1 ? 1 : 0
  return `${n.toFixed(dec)} ${units[i]}`
}

/** Transfer rate, e.g. "38.4 MB/s". Empty when there is nothing to report — a rate
 *  is only known once an engine has counted bytes for long enough to divide them by
 *  something, and "0 B/s" reads as a stalled backup rather than as "not yet". */
export function renderRate(bytesPerSecond?: number): string {
  if (!bytesPerSecond || bytesPerSecond <= 0) return ''
  return `${renderSize(bytesPerSecond)}/s`
}

/** A duration in seconds as an approximate, human-sized string: "45s", "3 min",
 *  "2 h 10 min".
 *
 *  Deliberately coarse above a minute. An ETA is an estimate whose error is measured
 *  in minutes, and rendering it as "2 h 10 min 43 s" claims a precision that the
 *  number does not have — which is how a progress indicator loses the user's trust
 *  even when it is roughly right. */
export function renderDuration(seconds?: number): string {
  if (!seconds || seconds <= 0) return ''
  if (seconds < 60) return `${Math.round(seconds)}s`
  const mins = Math.round(seconds / 60)
  if (mins < 60) return `${mins} min`
  const hours = Math.floor(mins / 60)
  const rest = mins % 60
  return rest ? `${hours} h ${rest} min` : `${hours} h`
}

/** Elapsed time between two instants, for a run in flight. */
export function renderElapsed(from?: string, to?: string): string {
  if (!from) return ''
  const start = Date.parse(from)
  if (Number.isNaN(start)) return ''
  const end = to ? Date.parse(to) : Date.now()
  return renderDuration(Math.max(0, (end - start) / 1000))
}

/** A percentage, e.g. "37.4%". */
export function renderPercent(value?: number | null): string {
  if (value == null || !Number.isFinite(value)) return '—'
  return `${value.toFixed(1)}%`
}

/** Link speed in megabits, which is how uplinks are sold and measured — "94.2
 *  Mbps" rather than the 11.8 MB/s it works out to. 1 Mbps = 125,000 bytes/s. */
export function renderMbps(bytesPerSecond?: number | null): string {
  if (bytesPerSecond == null || !Number.isFinite(bytesPerSecond)) return '—'
  const mbps = (bytesPerSecond * 8) / 1_000_000
  return `${mbps < 10 ? mbps.toFixed(2) : mbps < 100 ? mbps.toFixed(1) : mbps.toFixed(0)} Mbps`
}

/** Uptime as a coarse "12d 4h" / "4h 20min" / "18min". Deliberately two units:
 *  nobody reads the seconds of an uptime, and showing them makes the number look
 *  like it is still moving. */
export function renderUptime(seconds?: number | null): string {
  if (!seconds || seconds <= 0) return '—'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}min`
  return `${m}min`
}

/** A throughput, always signed with a rate — unlike renderRate, which returns ''
 *  for zero because a backup showing "0 B/s" reads as stalled. A live network
 *  graph showing nothing is not stalled, it is idle, and "0 B/s" is the truth. */
export function renderThroughput(bytesPerSecond?: number | null): string {
  if (bytesPerSecond == null || !Number.isFinite(bytesPerSecond)) return '—'
  return `${renderSize(Math.max(0, bytesPerSecond))}/s`
}
