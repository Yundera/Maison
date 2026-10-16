<script lang="ts">
  import { clickOutside } from '../actions'
  import { settings } from '../stores/settings'
  import { t, languages } from '../i18n'
  import { openSettings } from '../route'
  import {
    incidents,
    loadIncidents,
    subscribeIncidents,
    ackIncident,
    resolveIncident,
    incidentLabel,
  } from '../stores/incidents'
  import { canInstall, installed, installHelp, isIOSSafari, promptInstall } from '../stores/pwa'
  import { feedback, feedbackOpen, loadFeedback } from '../stores/feedback'

  let open = $state(false)
  let drawer = $state(false)
  let busy = $state(false)

  // The health pill is the register's primary surface, and deliberately so: mail leaves the
  // box through a relay nobody here can see the far end of, and a PCS alert filed as
  // spam fails silently. This does not.
  //
  // Seeded from one GET and then followed live. The hub only produces a channel while
  // somebody is subscribed, so holding a subscription for the whole session is exactly
  // what that design is trying to avoid — but this channel is event-driven and pushes
  // nothing on a tick, which is what makes it affordable to hold.
  loadIncidents()
  $effect(() => subscribeIncidents())

  const alerts = $derived($incidents.open.filter((i) => !i.acked))
  const worst = $derived(alerts.some((i) => i.severity === 'critical') ? 'critical' : 'warning')

  // Unacked first: those are what the pill is counting. Seen ones stay listed, dimmed,
  // because acknowledging hides the badge, not the problem.
  const listed = $derived([...$incidents.open].sort((x, y) => Number(!!x.acked) - Number(!!y.acked)))

  async function act(fn: () => Promise<void>) {
    busy = true
    try {
      await fn()
    } finally {
      busy = false
    }
  }

  // The drawer is a shortcut, not a second notifications page: anything beyond
  // Seen / Dismiss goes to the place that can fix it. A backup problem is fixed on
  // the Backups page (the key form, the Resume button), so its row links straight
  // there; everything else is one click away through "All notifications".
  function go(section: 'backups' | 'notifications') {
    drawer = false
    openSettings(section)
  }

  function when(iso: string): string {
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
  }

  // Language, wallpaper and widgets stay here rather than moving to the settings
  // page: they are instant and previewed against the dashboard behind them, and
  // sending someone to a full-screen page to pick a wallpaper they can no longer
  // see would be worse. Anything that configures the box itself lives on the page.
  function more() {
    open = false
    drawer = false
    openSettings()
  }

  // Installing belongs in this menu by the same rule as the three above: it is
  // instant, it is browser-local, and it changes nothing about the box. It is shown
  // only when it can actually do something — the argument the bell below makes for
  // itself. On iOS there is no install API at all, only instructions, so that
  // branch opens the dialog instead.
  function install() {
    open = false
    if ($canInstall) promptInstall()
    else installHelp.set(true)
  }

  const wallpapers = [
    '/wallpapers/default_wallpaper.jpg',
    '/wallpapers/wallpaper01.jpg',
    '/wallpapers/wallpaper02.jpg',
  ]

  function setWallpaper(w: string) {
    settings.update((s) => ({ ...s, wallpaper: w }))
  }
  function setLanguage(code: string) {
    settings.update((s) => ({ ...s, language: code }))
  }
  // Where an app opens is a per-box preference like the others here, and it is
  // previewed by the very next tile click — so it belongs in this menu rather than
  // on the settings page, which configures the box itself.
  function toggleNewTab() {
    settings.update((s) => ({ ...s, open_in_new_tab: !s.open_in_new_tab }))
  }
  // Feedback goes to whoever operates this box, and only exists when they asked for
  // it (internal/feedback). It sits in this menu because it is about the dashboard
  // the user is looking at, not a setting of the box.
  loadFeedback()
  function openFeedback() {
    open = false
    feedbackOpen.set(true)
  }

  function toggleWidget(key: string) {
    settings.update((s) => ({ ...s, widgets: { ...s.widgets, [key]: !s.widgets[key] } }))
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === 'Escape') {
      open = false
      drawer = false
    }
  }}
/>

<!-- No full-width bar: the desktop is wallpaper and dark glass, and a light strip
     across the top was the one surface that did not belong to it. What the bar held
     is one glass capsule, placed at the right of the app section's header. -->
<div class="island">
  <div class="wrap" use:clickOutside={() => (drawer = false)}>
    <button
      class="health"
      class:warning={alerts.length > 0 && worst === 'warning'}
      class:critical={alerts.length > 0 && worst === 'critical'}
      title={$t('notifications')}
      aria-label={alerts.length > 0 ? $t('status_attention', { count: String(alerts.length) }) : $t('status_all_good')}
      aria-expanded={drawer}
      onclick={() => {
        drawer = !drawer
        open = false
      }}
    >
      <span class="dot"></span>
      <span class="label">
        {alerts.length > 0 ? $t('status_attention', { count: String(alerts.length) }) : $t('status_all_good')}
      </span>
      {#if alerts.length > 0}<span class="count">{alerts.length}</span>{/if}
    </button>

    {#if drawer}
      <div class="drawer popover">
        <h3>{$t('notifications')}</h3>
        {#if listed.length === 0}
          <p class="empty">{$t('notifications_all_clear')}</p>
        {:else}
          <ul>
            {#each listed as inc (inc.id)}
              <li class:acked={inc.acked}>
                <span class="sev" class:critical={inc.severity === 'critical'}></span>
                <div class="body">
                  <p class="title">{incidentLabel($t, inc)}</p>
                  {#if inc.detail}<p class="detail">{inc.detail}</p>{/if}
                  <p class="meta">{$t('notifications_since', { when: when(inc.since) })}</p>
                  <div class="row-actions">
                    {#if inc.kind.startsWith('backup.')}
                      <button class="primary" onclick={() => go('backups')}>{$t('notifications_open_backups')}</button>
                    {/if}
                    {#if !inc.acked}
                      <button disabled={busy} onclick={() => act(() => ackIncident(inc.id))}>{$t('notifications_ack')}</button>
                    {/if}
                    <button disabled={busy} onclick={() => act(() => resolveIncident(inc.id))}>{$t('notifications_dismiss')}</button>
                  </div>
                </div>
              </li>
            {/each}
          </ul>
        {/if}
        <button class="more" onclick={() => go('notifications')}>
          <span>{$t('notifications_see_all')}</span>
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M9 6l6 6-6 6" />
          </svg>
        </button>
      </div>
    {/if}
  </div>

  <div class="wrap" use:clickOutside={() => (open = false)}>
    <button
      class="picon"
      title={$t('settings')}
      aria-label={$t('settings')}
      aria-expanded={open}
      onclick={() => {
        open = !open
        drawer = false
      }}
    >
      <svg viewBox="0 0 24 24" width="19" height="19" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" />
      </svg>
    </button>

      {#if open}
        <div class="dropdown popover">
          <h3>{$t('settings')}</h3>

          <label class="field">
            <span>{$t('language')}</span>
            <select value={$settings.language} onchange={(e) => setLanguage((e.target as HTMLSelectElement).value)}>
              {#each languages as l}<option value={l.code}>{l.name}</option>{/each}
            </select>
          </label>

          <div class="field">
            <span>{$t('wallpaper')}</span>
            <div class="thumbs">
              {#each wallpapers as w}
                <button class="thumb" class:active={$settings.wallpaper === w} style:background-image={`url(${w})`} aria-label="wallpaper" onclick={() => setWallpaper(w)}></button>
              {/each}
            </div>
          </div>

          <div class="field">
            <span>{$t('widgets')}</span>
            <div class="toggles">
              {#each ['clock', 'system', 'storage'] as key}
                <label class="toggle">
                  <input type="checkbox" checked={$settings.widgets[key] ?? true} onchange={() => toggleWidget(key)} />
                  {$t(key === 'system' ? 'system_status' : key)}
                </label>
              {/each}
            </div>
          </div>

          <div class="field">
            <span>{$t('apps')}</span>
            <div class="toggles">
              <label class="toggle">
                <input type="checkbox" checked={$settings.open_in_new_tab} onchange={toggleNewTab} />
                {$t('open_in_new_tab')}
              </label>
            </div>
          </div>

          {#if !$installed && ($canInstall || isIOSSafari)}
            <button class="action" onclick={install}>
              <span>{$t('install_app')}</span>
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M12 4v10M8 11l4 4 4-4M5 19h14" />
              </svg>
            </button>
          {/if}

          {#if $feedback.enabled}
            <button class="action" onclick={openFeedback}>
              <span>{$t('feedback_send_menu')}</span>
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M5 5h14v10H9l-4 4z" />
              </svg>
            </button>
          {/if}

          <button class="more" onclick={more}>
            <span>{$t('all_settings')}</span>
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M9 6l6 6-6 6" />
            </svg>
          </button>
        </div>
      {/if}
  </div>
</div>

<style>
  /* Sits at the right end of the app section's header (AppSection.svelte), after
     the "+". Its own stacking context keeps the menus above the tiles below and
     the glass pseudo-element (z-index -1) behind the buttons. */
  .island {
    position: relative;
    z-index: 20;
    display: flex;
    align-items: center;
    gap: 2px;
    padding: 3px;
    border-radius: var(--r-pill);
  }
  /* The glass lives on a pseudo-element: a backdrop-filter on .island itself would
     become the backdrop root of the menus inside it, and they could then blur
     nothing but the capsule. */
  .island::before {
    content: '';
    position: absolute;
    inset: 0;
    z-index: -1;
    border-radius: inherit;
    background: var(--glass-color);
    backdrop-filter: var(--glass-blur);
    -webkit-backdrop-filter: var(--glass-blur);
    box-shadow: var(--glass-shadow);
  }
  .wrap {
    position: relative;
  }
  .picon,
  .health {
    height: 2.25rem;
    border: none;
    border-radius: var(--r-pill);
    background: transparent;
    color: #fff;
    cursor: pointer;
    transition:
      background var(--dur-fast) var(--ease-ios),
      transform var(--dur-fast) var(--ease-ios);
  }
  .picon {
    display: grid;
    place-items: center;
    width: 2.25rem;
  }
  .picon:hover,
  .health:hover,
  .picon[aria-expanded='true'],
  .health[aria-expanded='true'] {
    background: rgb(255 255 255 / 0.14);
  }
  .picon:active,
  .health:active {
    transform: scale(0.94);
  }
  .picon:focus-visible,
  .health:focus-visible {
    outline: 2px solid rgb(255 255 255 / 0.85);
    outline-offset: 1px;
  }
  /* Always there, unlike the bell it replaces. An always-present bell that is
     usually empty trains people not to look at it; a pill that says "All good"
     tells them something every time they do, so the day it turns amber is noticed. */
  .health {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0 0.9rem 0 0.8rem;
    font-size: 0.8rem;
    font-weight: 600;
    white-space: nowrap;
  }
  .dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background: var(--dot-ok);
    box-shadow: 0 0 0 3px rgb(52 199 89 / 0.25);
  }
  .warning .dot {
    background: var(--dot-warn);
    box-shadow: 0 0 0 3px rgb(255 204 0 / 0.28);
  }
  .critical .dot {
    background: var(--dot-error);
    box-shadow: 0 0 0 3px rgb(255 59 48 / 0.3);
  }
  /* The count stands in for the sentence on a phone, where the capsule has to stay small. */
  .count {
    display: none;
  }
  @media (max-width: 768px) {
    .health .label {
      display: none;
    }
    .health {
      padding: 0 0.8rem;
    }
    .count {
      display: inline;
    }
  }
  .drawer,
  .dropdown {
    position: absolute;
    right: 0;
    top: calc(100% + 9px);
    border-radius: var(--r-lg);
    color: var(--grey-800);
    transform-origin: top right;
  }
  .drawer {
    width: min(22rem, calc(100vw - 2rem));
    max-height: min(32rem, calc(100vh - 6rem));
    overflow-y: auto;
    padding: 0.85rem 0.6rem 0.5rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .drawer h3 {
    margin: 0 0.4rem 0.1rem;
    font-size: 0.95rem;
    font-weight: 600;
  }
  .drawer .empty {
    margin: 0 0.4rem 0.4rem;
    font-size: 0.85rem;
    color: var(--grey-600);
  }
  .drawer ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .drawer li {
    display: flex;
    gap: 0.6rem;
    padding: 0.6rem 0.4rem;
  }
  .drawer li + li {
    border-top: 1px solid rgb(0 0 0 / 0.06);
  }
  .drawer li.acked {
    opacity: 0.6;
  }
  .sev {
    flex: none;
    width: 0.5rem;
    height: 0.5rem;
    margin-top: 0.4rem;
    border-radius: 50%;
    background: var(--warning, #d29922);
  }
  .sev.critical {
    background: var(--red);
  }
  .body {
    min-width: 0;
  }
  .body p {
    margin: 0;
    line-height: 1.4;
    overflow-wrap: anywhere;
  }
  .title {
    font-size: 0.85rem;
    font-weight: 600;
  }
  .detail,
  .meta {
    font-size: 0.75rem;
    color: var(--grey-600);
  }
  .row-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
    margin-top: 0.45rem;
  }
  .row-actions button {
    border: none;
    border-radius: var(--r-pill);
    padding: 0.2rem 0.7rem;
    font-size: 0.75rem;
    font-weight: 600;
    background: rgb(118 118 128 / 0.14);
    color: var(--grey-800);
    cursor: pointer;
  }
  .row-actions button:hover:not(:disabled) {
    background: rgb(118 118 128 / 0.22);
  }
  .row-actions button.primary {
    background: var(--primary);
    color: var(--text-on-accent);
  }
  .row-actions button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .dropdown {
    width: 17rem;
    padding: 1rem;
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  .dropdown h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    font-size: 0.8rem;
    color: var(--grey-600);
  }
  select {
    background: #fff;
    border: 1px solid #cfcfcf;
    border-radius: var(--r-sm);
    color: var(--grey-800);
    padding: 0.4rem 0.5rem;
    font-size: 0.85rem;
  }
  .thumbs {
    display: flex;
    gap: 0.5rem;
  }
  .thumb {
    width: 3.4rem;
    height: 2.1rem;
    border-radius: var(--r-sm);
    background-size: cover;
    background-position: center;
    border: 2px solid transparent;
    cursor: pointer;
    padding: 0;
  }
  .thumb.active {
    border-color: var(--primary);
  }
  .toggles {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .toggle {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    color: var(--grey-800);
    font-size: 0.85rem;
  }
  /* Way out of the dropdown, into the settings page. */
  /* Shares .more's box but keeps its divider: .more is the way OUT of the menu and
     should stay the only thing below the rule. */
  .action {
    display: flex;
    align-items: center;
    justify-content: space-between;
    width: 100%;
    padding: 0.5rem;
    border: none;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--grey-800);
    font-size: 0.85rem;
    cursor: pointer;
  }
  .action:hover {
    background: rgba(0, 0, 0, 0.05);
  }
  .more {
    display: flex;
    align-items: center;
    justify-content: space-between;
    width: 100%;
    margin-top: -0.25rem;
    padding: 0.5rem 0.5rem;
    border: none;
    border-top: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 0 0 6px 6px;
    background: transparent;
    color: var(--grey-800);
    font-size: 0.85rem;
    cursor: pointer;
  }
  .more:hover {
    background: rgba(0, 0, 0, 0.04);
  }
  .more svg {
    color: var(--grey-600);
  }
</style>
