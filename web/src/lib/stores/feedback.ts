import { writable } from 'svelte/store'
import { api } from '../api/client'

/**
 * Whether this box can send feedback, and to whom.
 *
 * The answer comes from the deployment's feedback sink, through Maison's backend —
 * the page never talks to the sink itself (internal/feedback). `enabled: false` is the
 * normal state of a box with no operator, and the menu entry then does not exist.
 */
export interface FeedbackState {
  enabled: boolean
  operator?: string
  privacyNote?: string
  maxLength?: number
  supportUrl?: string
  categories?: string[]
  version?: string
}

export const feedback = writable<FeedbackState>({ enabled: false })

/** Drives the feedback dialog. */
export const feedbackOpen = writable(false)

/** Asked once per page load. A failure is the feature being unavailable, which is
 *  already what the store says. */
export async function loadFeedback() {
  try {
    feedback.set(await api.get<FeedbackState>('/api/feedback'))
  } catch {
    feedback.set({ enabled: false })
  }
}

export function sendFeedback(message: string, category: string, page: string) {
  return api.post('/api/feedback', { message, category, page })
}
