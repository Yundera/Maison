import type { Compat, Reason, Resources } from './stores/store'

/** A translator, as `$t` is: key + string params. */
type T = (key: string, params?: Record<string, string>) => string

/** The name of a capability or resource, in the viewer's language. */
export function requirementName(code: string, t: T): string {
  return t(`req_${code.replace(/-/g, '_')}`)
}

/** One unmet requirement as a sentence fragment — "a public IP", "8 GB of memory
 *  (this box has 4G)". */
export function reasonText(r: Reason, t: T): string {
  const name = requirementName(r.code, t)
  if (r.code === 'arch') return t('req_arch_reason', { need: r.need ?? '', have: r.have ?? '' })
  if (!r.need) return name
  return t('req_quantity_reason', { name, need: r.need, have: r.have ?? '?' })
}

/** The one-line reason a card shows under a greyed-out or badged app. */
export function compatSummary(c: Compat | undefined, t: T): string {
  if (!c) return ''
  if (c.blocked) return reasonText(c.unsupported![0], t)
  const missing = [...(c.unsupported ?? []), ...(c.below_min ?? [])]
  if (missing.length) return t('req_needs', { list: missing.map((r) => reasonText(r, t)).join(', ') })
  if (c.below_recommended?.length)
    return t('req_below_recommended', { list: c.below_recommended.map((r) => reasonText(r, t)).join(', ') })
  if (c.uncertain?.length)
    return t('req_uncertain', { list: c.uncertain.map((n) => requirementName(n, t)).join(', ') })
  return ''
}

/** "2 cores · 4G memory · 10G disk" for one tier of an app's resources. */
export function resourcesText(set: Resources['min'], t: T): string {
  if (!set) return ''
  const parts: string[] = []
  if (set.cpu) parts.push(t('req_cpu_count', { n: String(set.cpu) }))
  if (set.memory) parts.push(`${formatSize(set.memory)} ${t('req_memory')}`)
  if (set.disk) parts.push(`${formatSize(set.disk)} ${t('req_disk')}`)
  return parts.join(' · ')
}

/** Bytes → "2G", "1.5G", "512M" — the spelling a compose file uses. */
export function formatSize(b: number): string {
  for (const [u, n] of [['T', 2 ** 40], ['G', 2 ** 30], ['M', 2 ** 20], ['K', 2 ** 10]] as const) {
    if (b >= n) {
      const v = b / n
      return `${Number.isInteger(v) ? v : Math.round(v * 10) / 10}${u}`
    }
  }
  return String(b)
}
