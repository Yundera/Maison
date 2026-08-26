import { writable } from 'svelte/store'
import { live } from '../live/ws'
import type { RunState } from './backupengine'
import type { UserDataRestoreState } from './backups'

/** The live half of the backup page: the whole-box run, and a user-data restore.
 *
 *  Deliberately its own channel rather than a field on the app list. Neither of these
 *  has a tile — a run's plan (what is still to come) exists nowhere in the app list,
 *  and the user-data set is not an app — and the app payload is expensive to build,
 *  where this one is a struct copy. Tying them together would have made a byte counter
 *  the most expensive thing on the dashboard.
 *
 *  It replaces a 2s poll of /api/backup/status. That endpoint probes every engine's
 *  repository, which for a remote one is a subprocess, so it was never something to
 *  call on a timer — and a progress bar fed at 0.5 Hz looks broken however good the
 *  numbers behind it are. */
export interface BackupLive {
  run: RunState
  restore: UserDataRestoreState
}

export const backupLive = writable<BackupLive | null>(null)

/** Begin receiving run/restore progress. Returns an unsubscribe fn. */
export function subscribeBackup(): () => void {
  return live.subscribe('backup', (d) => backupLive.set(d as BackupLive))
}
