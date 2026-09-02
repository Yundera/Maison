import { api } from '../api/client'

/** One backup engine this box knows about.
 *
 *  Engines are listed whether or not they are usable: an engine with no repository
 *  still holds whatever it wrote before, and that history has to stay reachable
 *  after the user switches away from it. */
export interface EngineInfo {
  id: string
  /** What to call it on screen, when the deployment provisioned a name. The ID stays
   *  a bare engine name because it is recorded on every backup and is machine
   *  identity; the display name describes the *space* the engine points at, so a PCS
   *  can say "Yundera Backup Storage" while the same engine self-hosted does not. */
  name?: string
  /** False when the engine has nothing to write to — the normal state of a remote
   *  engine on a box whose provisioning has not run. Not an error. */
  connected: boolean
  /** Why it is not connected, for the user to read. */
  detail?: string
  /** Whether its backups survive losing the machine. The local engine's do not. */
  offsite: boolean
}

export interface Retention {
  latest: number
  daily: number
  weekly: number
  monthly: number
  annual: number
}

export interface BackupConfig {
  enabled: boolean
  /** The user's chosen engine, or absent to follow whatever the deployment
   *  provisioned. Only the override is stored, so clearing it resumes tracking. */
  engine?: string
  hour: number
  minute: number
  user_data: boolean
  keep: Retention
  keep_local: number
}

/** One target's place in a run.
 *
 *  The whole list arrives before the first target starts, which is what lets the page
 *  say "3 of 9" and show what is still to come. It carries no display name on
 *  purpose: resolving one server-side means asking Docker about every app on the box,
 *  and the dashboard already holds the names and icons — see targetLabel(). */
export interface TargetState {
  /** "app:jellyfin", or "userdata". */
  id: string
  kind: 'app' | 'userdata'
  /** Compose project, absent for the user-data target. */
  app?: string
  /** The backup this produced, once it has one. */
  name?: string
  /** `skipped` is a target the run deliberately did not attempt — the user-data set
   *  while a restore is rewriting it, or an app somebody is already backing up by
   *  hand. It is not a failure and is not counted as one. */
  status: 'pending' | 'running' | 'done' | 'failed' | 'skipped'
  error?: string
  /** Engine-agnostic step: copy | sync | start | compress | restore. `sync` is the
   *  one where the app is actually stopped. */
  phase?: string
  message?: string
  /** -1 when neither the engine nor the byte counts can say. */
  pct: number
  done?: number
  total?: number
  /** Bytes per second, absent until measurable. */
  rate?: number
  /** Seconds left in this phase, absent until the estimate is worth showing. */
  eta?: number
  started?: string
  finished?: string
}

export interface RunState {
  running: boolean
  /** False until a run has finished. Do not test the timestamps for truthiness:
   *  Go serialises a zero time as year 0001 rather than omitting it. */
  ran: boolean
  started?: string
  finished?: string
  /** ID of the target in flight. Redundant against `targets`, kept for callers that
   *  do not want the whole plan. */
  current?: string
  targets?: TargetState[]
  failures: number
  last_error?: string
}

/** How many targets have finished, successfully or not — the "3" in "3 of 9". */
export function targetsDone(run: RunState): number {
  return (run.targets ?? []).filter((t) => t.status !== 'pending' && t.status !== 'running').length
}

/** When a copy of the encryption key last left the box by mail. Absent means no
 *  copy has ever been mailed — which the page says out loud, because a key nobody
 *  has a copy of is the failure this whole section exists to prevent. */
export interface KeySentRecord {
  sent_at: string
  to?: string
  engine?: string
  /** True when Maison sent it on its own at boot rather than the user asking. */
  auto?: boolean
}

export interface BackupStatus {
  engines?: EngineInfo[]
  active: string
  chosen?: string
  run: RunState
  config: BackupConfig
  targets?: string[]
  /** False on a box whose repository has never been provisioned: there is no key to
   *  show or mail, and the page offers neither. */
  has_key: boolean
  key_sent?: KeySentRecord
}

export function fetchBackupStatus(): Promise<BackupStatus> {
  return api.get<BackupStatus>('/api/backup/status')
}

/** Saves the whole configuration. There is no partial update: a merge is how a
 *  field nobody remembered gets silently reset. */
export function saveBackupConfig(c: BackupConfig): Promise<BackupConfig> {
  return api.put<BackupConfig>('/api/backup/config', c)
}

export function runBackupNow(): Promise<unknown> {
  return api.post('/api/backup/run')
}

/** Mails the repository encryption key. The only copy that exists off the box —
 *  see the warning the settings page shows next to it. */
export function emailBackupKey(): Promise<unknown> {
  return api.post('/api/backup/email-key')
}

/** Reads the repository encryption key for display.
 *
 *  A POST although it only reads: the response body is the key itself, and a GET
 *  would leave it in history, prefetches and anything that shares a URL. */
export function showBackupKey(): Promise<{ key: string }> {
  return api.post<{ key: string }>('/api/backup/key')
}
