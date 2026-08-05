import { api } from '../api/client'

/** One backup engine this box knows about.
 *
 *  Engines are listed whether or not they are usable: an engine with no repository
 *  still holds whatever it wrote before, and that history has to stay reachable
 *  after the user switches away from it. */
export interface EngineInfo {
  id: string
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

export interface SmtpConfig {
  host: string
  port: number
  user?: string
  pass?: string
  from: string
  to: string
  security?: string
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
  smtp?: SmtpConfig
}

export interface RunResult {
  target: { Kind: string; App: string }
  name?: string
  error?: string
}

export interface RunState {
  running: boolean
  /** False until a run has finished. Do not test the timestamps for truthiness:
   *  Go serialises a zero time as year 0001 rather than omitting it. */
  ran: boolean
  started?: string
  finished?: string
  current?: string
  results?: RunResult[]
  failures: number
  last_error?: string
}

export interface BackupStatus {
  engines?: EngineInfo[]
  active: string
  chosen?: string
  run: RunState
  config: BackupConfig
  targets?: string[]
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
