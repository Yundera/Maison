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
