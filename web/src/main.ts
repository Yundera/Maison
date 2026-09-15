import './styles/tokens.css'
import './styles/app.css'
import { mount } from 'svelte'
import App from './App.svelte'
import { BRAND } from './lib/brand'

// index.html carries the name too, for the moment before the bundle runs; this is
// what actually names the tab, and it is the single source (see lib/brand).
document.title = BRAND

// The service worker caches the bundle, the font and the wallpapers so a warm
// start does not refetch over a megabyte. It deliberately does NOT cache the app
// shell — see the header of internal/server/pwa.go for why serving a cached
// document would be a way around the onboarding gate.
//
// There is no update-available prompt, and none is needed: navigations are
// network-only, so a new release reaches the browser on the next load with no
// worker involvement. The worker's lifecycle only decides when the PREVIOUS
// release's asset files are dropped.
//
// Production only. Under `vite dev` the worker is proxied from Go while the
// modules are served unhashed by Vite, so caching them would be stale-module hell.
if (import.meta.env.PROD && 'serviceWorker' in navigator) {
  // On load rather than immediately: the worker's install step fetches the offline
  // page, and that should not compete with the dashboard's first paint.
  addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {})
  })
}

const app = mount(App, { target: document.getElementById('app')! })

export default app
