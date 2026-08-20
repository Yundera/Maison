import { api } from '../api/client'

/** One backup of an app, wherever it lives — an archive under
 *  ${DATA_ROOT}/AppData/.backups/<app>/, or a snapshot in a remote engine's
 *  repository.
 *
 *  Backups are made three ways — as the side effect of an uninstall, on demand from
 *  an app's Backups tab, and by the nightly schedule — and nothing distinguishes
 *  them afterwards. A backup is a backup, whatever created it and whichever engine
 *  holds it. */
export interface Backup {
  app: string // the compose project it belongs to
  name: string // on-disk base name, e.g. 2026-07-10_153045 or that + .zip
  stamp: string // the name without its extension, YYYY-MM-DD_HHMMSS
  date: string // YYYY-MM-DD, the stamp's day
  zip: boolean // compressed archive rather than a plain folder
  size: number // bytes; see below on when this is measured
  /** Where it is: on the data disk, or in a backup engine's repository. A property
   *  of the engine holding it, never a summary across engines — a backup belongs to
   *  exactly one, and two engines holding the same stamp are two backups. */
  tier: 'local' | 'remote'
  /** Which engine holds it ("local", "kopia", …). */
  engine?: string
}

/** One app's backups, as the global Backups page groups them. */
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

/** A restore of the user-data set, in flight or last attempted. */
export interface UserDataRestoreState {
  running: boolean
  stamp?: string
  message?: string
  /** The destructive mode. A failed copy into a new folder has left nothing behind; a
   *  failed in-place restore has not, which is why the two are told apart. */
  in_place: boolean
  /** Sticky: survives until the next attempt, so a failure nobody was watching is still
   *  on the page afterwards. */
  error?: string
  /** An in-place restore that started and did not finish. Read from a marker file, so it
   *  outlives a restart — which is the whole point of it being a file. */
  interrupted: boolean
  interrupted_stamp?: string
}

/** Your files — everything at the data root except AppData, which each app backs up on
 *  its own. It is not an app and deliberately not modelled as one: no compose project,
 *  no containers, no tile. */
export interface UserDataBackups {
  /** False on a box that cannot back this up at all — most often a default install,
   *  where the local engine is selected and would be copying the tree onto the disk it
   *  is meant to survive. `reason` says which case it is, because an empty list
   *  otherwise reads as "nothing to worry about". */
  available: boolean
  reason?: string
  backups: Backup[]
  /** The newest snapshot's size — what the set currently is. Deliberately not a sum
   *  across snapshots: they are incremental, so summing thirty nightly copies of a media
   *  library reports terabytes that were never stored. */
  size: number
  source: string
  /** What the set leaves out, so a restore that did not bring something back is
   *  diagnosable rather than mysterious. */
  excluded: string[]
  restore: UserDataRestoreState
}

export interface GlobalBackups {
  apps: AppBackups[]
  user_data: UserDataBackups
  /** What the app backups cost *on this disk* — not the sum of their sizes, since a
   *  backup that exists only in a repository takes no space here. This is the figure
   *  that belongs next to `free`. */
  local_used: number
  free?: number
  total?: number
}

/** One app's backups across every engine, sized — local folder archives included,
 *  which costs the server a tree walk each. Fine for a tab opened by hand; the
 *  store's install-click path deliberately uses an unmeasured list instead. */
export function fetchBackups(app: string): Promise<Backup[]> {
  return api.get<Backup[]>(`/api/apps/${encodeURIComponent(app)}/backups`)
}

/** Every app with backups in any engine, with orphans marked and local folder
 *  archives measured, plus the data filesystem's free space. Deliberately the
 *  expensive read: it is what answers "what is eating the disk", and it is opened by
 *  hand.
 *
 *  Spanning engines is what makes it usable after a rebuild: on a fresh box with the
 *  repository reconnected, this is the only thing that knows the apps existed. */
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
export function restoreBackup(app: string, name: string, engine?: string): Promise<void> {
  return api.post(`/api/backups/${encodeURIComponent(app)}/restore`, { name, engine: engine ?? '' })
}

/** Delete one backup from ONE engine. The only call in Maison that destroys user
 *  data, and the engine is required rather than defaulted: clearing space on the data
 *  disk must not quietly take the offsite copy with it, because that copy is the
 *  whole reason the offsite engine exists. */
export function deleteBackup(app: string, name: string, engine: string): Promise<void> {
  return api.del(
    `/api/backups/${encodeURIComponent(app)}/${encodeURIComponent(name)}?engine=${encodeURIComponent(engine)}`,
  )
}

/** What to call an engine on screen.
 *
 *  Three sources, in order: the name the deployment provisioned (a PCS calls its
 *  space "Yundera Backup Storage"; a self-hoster's identical engine is not given that
 *  name), then a translation for engines we ship, then the bare ID — which is always
 *  something rather than an empty label, and is what an engine added later shows
 *  until it is translated. */
export function engineLabel(id: string, provided?: string, translate?: (k: string) => string): string {
  if (provided) return provided
  if (translate) {
    const key = `backup_engine_name_${id}`
    const got = translate(key)
    if (got && got !== key) return got
  }
  return id
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

/** Restore the user-data set.
 *
 *  An empty `dest` means **in place**, over the live data root: the destructive mode.
 *  The server takes an undo snapshot first and refuses if it cannot — see
 *  backup.UserData.Restore. Any other `dest` is a directory the files are copied into,
 *  which touches nothing that already exists.
 *
 *  `entries` limits it to named top-level folders ("Documents"); empty means everything
 *  the backup holds. */
export function restoreUserData(name: string, opts: { dest?: string; entries?: string[] } = {}): Promise<void> {
  return api.post('/api/backups/userdata/restore', {
    name,
    dest: opts.dest ?? '',
    entries: opts.entries ?? [],
  })
}
