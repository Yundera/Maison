import { writable } from 'svelte/store'
import { live } from '../live/ws'

/** One app's usage, summed over its containers (internal/appstats.Stat). */
export interface AppStat {
  /** Compose project name — the same id as an entry in the `apps` store, which
   *  is where the row's name and icon come from. */
  id: string
  /** Share of the whole box, 0-100 across every core — not Docker's per-core
   *  figure — so the rows are comparable with the CPU gauge above them. */
  cpu_percent: number
  mem_usage: number
  mem_percent: number
  /** How many of the app's containers were sampled (running ones only). */
  containers: number
}

export interface AppStatsSnapshot {
  apps: AppStat[]
  mem_total: number
  cpu_count: number
}

export const appStats = writable<AppStatsSnapshot | null>(null)

/** Begin receiving per-app usage. Returns an unsubscribe fn — call it when the
 *  monitor closes: the backend only samples while someone is subscribed, and
 *  each round is a stats read per running container. */
export function subscribeAppStats(): () => void {
  appStats.set(null)
  return live.subscribe('appstats', (d) => appStats.set(d as AppStatsSnapshot))
}
