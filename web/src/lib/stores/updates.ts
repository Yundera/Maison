import { derived, writable } from 'svelte/store'
import { api } from '../api/client'
import { live } from '../live/ws'

/** Settings › Updates: every app's update status in one list, and "update all".
 *  The server does the checking and runs the queue (internal/server/updates.go);
 *  this is its report, followed live on the event-driven "updates" channel. */

/** One app's place on the page. `untracked` is a managed app with no store to
 *  update from; `unmanaged` is a stack Maison only discovered. `error` means the
 *  check could not be completed — never "up to date". */
export type UpdateState = 'available' | 'current' | 'error' | 'untracked' | 'unmanaged'

/** A service whose image the update changes — the version line. `from` is absent
 *  for a service the update adds, `to` for one it removes. */
export interface ImageChange {
  service: string
  from?: string
  to?: string
}

/** A store app an untracked app looks like. Only ever offered: linking replaces the
 *  app's compose on its next update, so the operator confirms it. */
export interface Suggestion {
  ref: string
  name: string
  store_name?: string
  /** The store app runs an image the installed one runs — a name-only match is a
   *  weaker guess, and the page says so. */
  images_match: boolean
}

export interface AppUpdate {
  id: string
  name: string
  icon?: string
  protected?: boolean
  state: UpdateState
  ref?: string
  store_name?: string
  images?: ImageChange[]
  suggestion?: Suggestion
  error?: string
}

export type RunStatus = 'queued' | 'updating' | 'done' | 'failed'

export interface RunItem {
  id: string
  status: RunStatus
  applied?: boolean
  rolled_back?: boolean
  warning?: string
  error?: string
  /** Refused: no rollback point could be taken. Nothing changed; the app can be
   *  updated on its own without a backup. */
  no_rollback?: boolean
}

export interface UpdateRun {
  running: boolean
  items: RunItem[] | null
  started?: string
  finished?: string
}

export interface UpdatesReport {
  /** Go's zero time when the box has never checked. */
  checked_at?: string
  checking: boolean
  apps: AppUpdate[] | null
  run: UpdateRun
}

export interface Preflight {
  apps: string[]
  /** Apps whose rollback point will not fit on the disk. They will be refused,
   *  untouched; each can then be updated on its own without a backup. */
  no_rollback?: string[]
}

export const updates = writable<UpdatesReport | null>(null)

/** Read the report. The server checks first when it never has. */
export async function loadUpdates(): Promise<void> {
  try {
    updates.set(await api.get<UpdatesReport>('/api/updates'))
  } catch {
    /* keep what is on screen */
  }
}

/** Follow the report live. Returns an unsubscribe fn. */
export function subscribeUpdates(): () => void {
  return live.subscribe('updates', (d) => updates.set(d as UpdatesReport))
}

/** Start a fresh check; the result arrives on the channel. */
export const checkUpdates = () => api.post('/api/updates/check')

/** What a run of `ids` (none = update all) would update, and which of those could
 *  not be rolled back. Measures app folders, so call it when a dialog opens. */
export const preflightUpdates = (ids: string[] = []) =>
  api.get<Preflight>(`/api/updates/preflight${ids.length ? `?ids=${ids.map(encodeURIComponent).join(',')}` : ''}`)

/** Start a run. None = every available app except system apps. */
/** `noBackup` is accepted for exactly one app — the retry after a refusal. */
export const runUpdates = (ids: string[] = [], noBackup = false) =>
  api.post('/api/updates/run', noBackup ? { ids, noBackup } : { ids })

/** What the settings rail badges: updates waiting in the ordinary grid, plus checks
 *  that failed. System apps are not counted — they are not in "update all". */
export const updateCount = derived(updates, ($u) =>
  ($u?.apps ?? []).filter((a) => (a.state === 'available' && !a.protected) || a.state === 'error').length,
)
