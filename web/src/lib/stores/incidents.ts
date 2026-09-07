import { writable } from 'svelte/store'
import { api } from '../api/client'
import { live } from '../live/ws'

/** Two levels, not five. The only decision severity drives is how the badge is
 *  painted, and a scale nobody can apply consistently is worse than no scale. */
export type Severity = 'warning' | 'critical'

/** One thing that is wrong with the box, or was.
 *
 *  An incident is a STATE, not an event: the same problem asserted twice is one
 *  record, however far apart the assertions are. `id` is what makes that true, so it
 *  is also the key to use in an {#each}. */
export interface Incident {
  id: string
  /** The class of problem — `disk.full`, `app.unhealthy`. Drives the localised
   *  label (`incident_<kind>`) and the per-kind mute. */
  kind: string
  severity: Severity
  /** The server's own one-line summary, in English. Shown when there is no
   *  translation for `kind`, which is what a kind added by another PCS component
   *  through the inbound API will normally be. */
  title: string
  detail?: string
  /** Interpolation for the localised label — `{app}`, `{percent}`, `{free}`. */
  args?: Record<string, string>
  since: string
  updated: string
  count: number
  /** Absent while the incident is open. It is absent rather than a zero date on
   *  purpose: Go serialises a zero time as year 0001, which is truthy here, so the
   *  server sends nothing at all rather than a date that would render. */
  resolved?: string
  acked?: boolean
}

export interface IncidentSnapshot {
  open: Incident[]
  recent: Incident[]
  muted: Record<string, boolean>
  /** Whether a relay is configured at all. The register works without one — the
   *  badge is the primary surface — but the page has to be able to say so. */
  mail_configured: boolean
  critical: number
  warnings: number
}

const EMPTY: IncidentSnapshot = {
  open: [],
  recent: [],
  muted: {},
  mail_configured: false,
  critical: 0,
  warnings: 0,
}

export const incidents = writable<IncidentSnapshot>(EMPTY)

/** Read the register once. The badge seeds from this rather than holding a live
 *  subscription open for the whole session: the hub only produces a channel while
 *  somebody is subscribed to it, and a permanent subscriber defeats that. */
export async function loadIncidents(): Promise<void> {
  try {
    incidents.set(await api.get<IncidentSnapshot>('/api/incidents'))
  } catch {
    /* a box that cannot answer is not a box with no incidents — keep what we have */
  }
}

/** Follow the register live. Returns an unsubscribe fn.
 *
 *  Event-driven, unlike every other channel here: nothing pushes on a tick, because
 *  an incident changes a handful of times a week on a healthy box. */
export function subscribeIncidents(): () => void {
  return live.subscribe('incidents', (d) => incidents.set(d as IncidentSnapshot))
}

async function apply(p: Promise<unknown>): Promise<void> {
  await p
  await loadIncidents()
}

/** Hide an open incident's badge without resolving it. The problem is still there,
 *  and its recovery notice will still be sent. */
export const ackIncident = (id: string) =>
  apply(api.post(`/api/incidents/${encodeURIComponent(id)}/ack`))

export const resolveIncident = (id: string) =>
  apply(api.del(`/api/incidents/${encodeURIComponent(id)}`))

/** Stop a whole class of incident being mailed. It is still recorded and still
 *  listed — a mute that erased the evidence would be indistinguishable from the
 *  detector being broken. */
export const muteKind = (kind: string, muted: boolean) =>
  apply(api.put('/api/incidents/mute', { kind, muted }))

/** Send a real alert through the real relay and wait for the answer, so a broken
 *  SMTP configuration reports its own error rather than a cheerful "sent". */
export const sendTestNotification = () => api.post('/api/notifications/test')

/** The kinds this build can raise, for the mute list.
 *
 *  Listed rather than derived from what is currently open, because the useful time to
 *  mute something is before it has ever happened. Kinds raised by other PCS components
 *  through the inbound API are not here and cannot be pre-muted; they become mutable
 *  once seen. */
export const KNOWN_KINDS = [
  'backup.failed',
  'backup.stale',
  'backup.engine',
  'disk.full',
  'app.unhealthy',
  'app.partial',
  'app.crashloop',
  'app.install',
  'app.update',
  'app.stackup',
  'store.source',
] as const
