import { api } from '../api/client'

/** One archive of an app, under ${DATA_ROOT}/AppData/.backups/<app>/.
 *
 *  Archives are made two ways — as the side effect of an uninstall, and on demand
 *  from an app's Backups tab — and nothing distinguishes them afterwards. A backup
 *  is a backup, whatever created it. */
export interface Backup {
  app: string // the compose project it belongs to
  name: string // on-disk base name, e.g. 2026-07-10_153045 or that + .zip
  stamp: string // the name without its extension, YYYY-MM-DD_HHMMSS
  date: string // YYYY-MM-DD, the stamp's day
  zip: boolean // compressed archive rather than a plain folder
  size: number // bytes; see below on when this is measured
  /** Where it actually is: on the data disk, in a backup engine's repository, or
   *  both. Restore and delete follow this, not whichever engine is selected now —
   *  so switching engines never orphans what the previous one wrote. */
  tier: 'local' | 'remote' | 'both'
  /** Which engine holds it ("local", "kopia", …). */
  engine?: string
}

/** One app's archives, as the global Backups page groups them. */
export interface AppBackups {
  app: string
  /** No folder at AppData/<app>: uninstalled, or never installed here. Its
   *  archives have no tile to hang off, so this page is the only way to reach
   *  them — which is the reason the page exists. */
  orphan: boolean
  backups: Backup[]
  total: number
}

/** What a backup would cost, for the confirmation dialog. */
export interface Estimate {
  size: number // measured size of the app folder
  needed: number // free space required, including headroom
  free: number // free space on the data filesystem; -1 when unreadable
  enough: boolean
  zip: boolean // which headroom `needed` was computed with
}

export interface GlobalBackups {
  apps: AppBackups[]
  free?: number
  total?: number
}

/** One app's archives, sized — folder archives included, which costs the server a
 *  tree walk each. Fine for a tab opened by hand; the store's install-click path
 *  deliberately uses an unmeasured list instead. */
export function fetchBackups(app: string): Promise<Backup[]> {
  return api.get<Backup[]>(`/api/apps/${encodeURIComponent(app)}/backups`)
}

/** Every app that has archives, with orphans marked and folder archives measured,
 *  plus the data filesystem's free space. Deliberately the expensive read: it is
 *  what answers "what is eating the disk", and it is opened by hand. */
export function fetchAllBackups(): Promise<GlobalBackups> {
  return api.get<GlobalBackups>('/api/backups')
}

/** Whether this app can be backed up right now, and what it would take. Walks the
 *  app folder, so call it when a dialog opens — not on a poll. */
export function estimateBackup(app: string, zip: boolean): Promise<Estimate> {
  return api.get<Estimate>(`/api/apps/${encodeURIComponent(app)}/backups/estimate?zip=${zip}`)
}

/** Kick off a detached backup. Resolves once the server has *started* it; progress
 *  then arrives on the live "apps" channel as the tile's bar, exactly like an
 *  install. Rejects up front on an unknown app or too little free space. */
export function startBackup(app: string, zip: boolean): Promise<void> {
  return api.post(`/api/apps/${encodeURIComponent(app)}/backup?zip=${zip}`)
}

/** Restore an archive over the live app, detached like a backup. The app's current
 *  state is archived first, so this is reversible. Also works for an orphan, where
 *  there is simply nothing to archive first — and once the folder lands, the app
 *  has a tile again. */
export function restoreBackup(app: string, name: string): Promise<void> {
  return api.post(`/api/backups/${encodeURIComponent(app)}/restore`, { name })
}

/** Delete one archive. The only call in Maison that destroys user data. */
export function deleteBackup(app: string, name: string): Promise<void> {
  return api.del(`/api/backups/${encodeURIComponent(app)}/${encodeURIComponent(name)}`)
}

/** "2026-07-10 15:30" from a YYYY-MM-DD_HHMMSS stamp.
 *
 *  Parsed by hand rather than through Date: the stamp is written in the server's
 *  local time and carries no zone, so constructing a Date would reinterpret it as
 *  the browser's and silently shift the displayed time. */
export function renderStamp(stamp: string): string {
  const m = /^(\d{4}-\d{2}-\d{2})_(\d{2})(\d{2})\d{2}$/.exec(stamp)
  return m ? `${m[1]} ${m[2]}:${m[3]}` : stamp
}
