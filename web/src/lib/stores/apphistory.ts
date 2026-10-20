import { api } from '../api/client'
import type { MinAvgMax } from './resources'

/** One bucket of an app's recorded usage, summed over its containers. CPU is a
 *  share of the whole box (0-100 across every core); mem is bytes. */
export interface AppSpan {
  at: string
  cpu: MinAvgMax
  mem: MinAvgMax
  disk_read: number
  disk_write: number
  containers: number
  points: number
}

/** GET /api/apps/{id}/history. Same window and gap contract as the host history
 *  (empty buckets omitted, step_ms to break the line on), plus one folder-size
 *  point a day. `recording` false means this box records sizes only — `reason`
 *  says why, ready to show. */
export interface AppHistory {
  from: number
  to: number
  step_ms: number
  spans: AppSpan[]
  disk: { at: string; bytes: number }[]
  enabled: boolean
  recording: boolean
  reason?: string
}

export const fetchAppHistory = (id: string, from: number, to: number, points = 500) =>
  api.get<AppHistory>(
    `/api/apps/${encodeURIComponent(id)}/history?from=${from}&to=${to}&points=${points}`,
  )
