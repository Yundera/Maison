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
  /** Whether this engine's backups are encrypted at rest. A property of the engine:
   *  false for the local one, whose archives are plain folders and zips on the data
   *  disk — which the settings page has to say, because an encryption-key card sitting
   *  above a list of them reads as a promise that covers them. */
  encrypted: boolean
  /** Whether a key for this engine actually exists on the box. Separate from
   *  `encrypted`: a repository engine on a box that has never been provisioned still
   *  encrypts, it just has nothing to encrypt with yet. */
  has_key: boolean
  /** What this engine has been told to keep, resolved for it alone. */
  retention?: EngineRetention
  /** Whether the rollback point an update takes lands here. Always and only the local
   *  engine: rolling back has to be a rename, and restoring from a repository is a
   *  download the app stays broken for. Not a setting — a property of the mechanism. */
  receives_rollback: boolean
  /** Whether the nightly run writes here. */
  receives_schedule: boolean
  /** Whether an uninstall archives here. */
  receives_uninstall: boolean
  /** When this engine last actually took a backup, and whether it is failing.
   *
   *  Per engine because the run's own verdict cannot describe a night where one
   *  destination succeeded and another did not — which is exactly what the page's
   *  per-engine rows exist to show. `at` is the last SUCCESSFUL write, so an engine
   *  that has been failing for a week does not look recent for having been tried. */
  last_run?: { at: string; failed: boolean }
}

/** How retention resolved for one engine.
 *
 *  Per engine rather than once for the box because retention genuinely differs by
 *  engine: a repository expires snapshots incrementally under its own policy, while a
 *  local archive is a full second copy of the app folder and is counted instead. The
 *  server resolves the layers (engine override → box → provisioned → compiled) and
 *  sends the answer, so the page never has to reproduce that walk. */
export interface EngineRetention {
  /** 'smart' | 'custom' | 'count' | 'age' | 'all'. */
  mode: string
  keep: Retention
  count?: number
  max_age_days?: number
  /** Which layer decided it: 'default' | 'provisioned' | 'box' | 'engine'. Lets the
   *  page say "your deployment chose this" rather than presenting a number the user
   *  never typed as though they had. */
  source: string
  /** Fields the deployment pinned. Rendered disabled with a reason rather than
   *  accepted and then silently reverted overnight. */
  locked?: string[]
  /** Whether the engine applies this itself. The difference between "the repository
   *  enforces it" and "Maison deletes them". */
  self_expiring: boolean
  /** Whether grandfather-father-son is sound for this engine's storage. False where a
   *  backup is a full second copy rather than incremental history — 7 daily + 4 weekly
   *  + 12 monthly would be twenty-three complete copies of an app on one disk — and
   *  the server collapses tiers to a count there. The page reads it so it never offers
   *  a choice the server is going to rewrite. */
  tiered: boolean
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

  /* The retention block. PUT /api/backup/config replaces the WHOLE document with no
     merge, so every field the server knows about has to be declared here — a config
     rebuilt from a narrower type silently clears whatever it left out. That is why
     these four are listed even though the page edits them only through the per-engine
     tabs below. */

  /** Box-wide retention intent: '' (follow the deployment) | 'smart' | 'custom' |
   *  'count' | 'age' | 'all'. */
  mode?: string
  /** Read under mode 'count'. */
  count?: number
  /** Read under mode 'age'. */
  max_age_days?: number
  /** Per-engine overrides, keyed by the engine's permanent ID. Entries for engines
   *  this build has never heard of are carried through untouched. */
  engines?: Record<string, EngineSettings>
}

/** One engine's own settings. Every field is optional; an unset one defers to the
 *  box-wide layer, then to the deployment, then to the compiled default. */
export interface EngineSettings {
  mode?: string
  keep?: Retention
  count?: number
  max_age_days?: number
  upload_limit_mb?: number
  unmanaged?: boolean
  /** Which triggers this engine receives.
   *
   *  Deliberately optional rather than defaulted: absent means "nobody has said", and a
   *  box that has never opened this page must keep writing where it was provisioned to
   *  rather than reading as "no destination for anything". Writing either of them for
   *  any engine switches the whole box off that fallback. */
  schedule?: boolean
  uninstall?: boolean
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

  /** What each destination made of this target.
   *
   *  One row per app rather than per (app, engine), because a row is one stop window:
   *  the app is stopped once and every engine writes inside it. The status above is
   *  derived from these — failed if any engine failed — so a target that half-succeeded
   *  reads as failed while still saying which copy landed. */
  engines?: EngineOutcome[]
}

/** One destination's outcome for one target. */
export interface EngineOutcome {
  engine: string
  status: 'done' | 'failed'
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

  /** When the schedule last ran, and whether it was failing. Absent on a box that
   *  has never run one.
   *
   *  Not derivable from `run`: that lives in the server's memory, is reset wholesale
   *  at the start of the next run, and is empty after a restart. This is the answer
   *  the protection line needs, and it comes off disk. */
  last_run?: { at: string; failed: boolean }
  /** When the schedule will fire next, including this box's jitter offset. Absent
   *  when the schedule is off — a real state, and one worth saying out loud. */
  next_run?: string
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
