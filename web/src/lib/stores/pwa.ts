/**
 * Installing Maison as an app on this device.
 *
 * Everything here is a property of THIS BROWSER on THIS DEVICE, which is why none
 * of it is persisted — not in `usersettings` (those follow the server, and one
 * phone's install has nothing to say to a desktop that cannot act on it) and not
 * in localStorage either. `matchMedia('(display-mode: standalone)')` and the
 * `beforeinstallprompt` event ARE the state, live and authoritative; a stored copy
 * could only ever be wrong.
 */
import { writable } from 'svelte/store'

/**
 * The event the browser fires when it is willing to install the app, stashed
 * because `prompt()` can only be called on the event object itself.
 *
 * Captured at module scope rather than in a component's onMount: it fires once,
 * unprompted, as soon as the page becomes installable, and there is no way to ask
 * for it again. A listener attached after mount can simply miss it.
 */
let deferred: BeforeInstallPromptEvent | null = null

/** True while the browser is offering to install. Chromium only. */
export const canInstall = writable(false)

/** True when this is already the installed app rather than a browser tab. */
export const installed = writable(isStandalone())

/** Drives the iOS "Add to Home Screen" instructions dialog. */
export const installHelp = writable(false)

addEventListener('beforeinstallprompt', (e) => {
  // Suppress Chrome's own mini-infobar so the only way in is Maison's own UI —
  // the user asked for no automatic popup.
  e.preventDefault()
  deferred = e
  canInstall.set(true)
})

addEventListener('appinstalled', () => {
  deferred = null
  canInstall.set(false)
  installed.set(true)
})

/**
 * Show the browser's install dialog. The event is single-use — the browser will
 * fire a fresh one if the user declines and the page stays installable.
 */
export async function promptInstall(): Promise<void> {
  const e = deferred
  if (!e) return
  deferred = null
  canInstall.set(false)
  try {
    await e.prompt()
    await e.userChoice
  } catch {
    // A prompt that fails (already installed in another window, gesture expired)
    // leaves the button gone until the browser offers again, which is correct.
  }
}

/** Whether the page is running as an installed app rather than in a tab. */
export function isStandalone(): boolean {
  return (
    matchMedia('(display-mode: standalone)').matches ||
    matchMedia('(display-mode: window-controls-overlay)').matches ||
    // iOS predates the display-mode media query and only ever had this.
    (navigator as { standalone?: boolean }).standalone === true
  )
}

/**
 * The first user-agent sniffing in Maison, and it is here because there is no
 * alternative. iOS Safari never fires `beforeinstallprompt` and exposes no install
 * API at all, so the only thing that helps an iPhone owner is being told which
 * gesture to make — and telling an Android user to "tap Share" is worse than
 * saying nothing. The detection is deliberately narrow, and nothing functional
 * branches on it: it only ever chooses between two pieces of copy.
 */
const isIOS =
  /iP(hone|od|ad)/.test(navigator.platform) ||
  // iPadOS 13+ reports itself as a Mac; the touch points are what give it away.
  (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)

/** Safari on iOS — the only browser there that can add to the home screen. */
export const isIOSSafari = isIOS && !/CriOS|FxiOS|EdgiOS/.test(navigator.userAgent)

/** The origin an install would be pinned to. Shown to the user; see below. */
export const installHost = location.host

/**
 * Whether this is one of the fallback addresses rather than the box's own domain.
 *
 * A PCS answers on its domain plus nip.io and sslip.io addresses derived from its
 * current public IP (see internal/server/onboarding.go's requestOrigin). An
 * installed app is pinned to the exact origin it was installed from, so installing
 * from a fallback produces a home-screen icon that dies the day the IP changes —
 * and, because origin is identity, it is a SEPARATE install from the domain one,
 * so a user can end up with two Maison icons that disagree.
 *
 * Not a hard block: a box that has no domain yet has nothing else to offer.
 */
export const isFallbackHost = /\.(nip|sslip)\.io$/.test(location.hostname)

/**
 * The Chromium-only install event. Neither it nor its two window events are in
 * lib.dom, so they are declared here rather than in a shared .d.ts — nothing else
 * in the app touches them.
 */
interface BeforeInstallPromptEvent extends Event {
  prompt(): Promise<void>
  readonly userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}

declare global {
  interface WindowEventMap {
    beforeinstallprompt: BeforeInstallPromptEvent
    appinstalled: Event
  }
}
