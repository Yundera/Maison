<script lang="ts">
  import { clickOutside } from '../actions'
  import { settings } from '../stores/settings'
  import { t, languages } from '../i18n'
  import { openSettings } from '../route'
  import { incidents, loadIncidents, subscribeIncidents } from '../stores/incidents'

  let open = $state(false)

  // The bell is the register's primary surface, and deliberately so: mail leaves the
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

  // Language, wallpaper and widgets stay here rather than moving to the settings
  // page: they are instant and previewed against the dashboard behind them, and
  // sending someone to a full-screen page to pick a wallpaper they can no longer
  // see would be worse. Anything that configures the box itself lives on the page.
  function more() {
    open = false
    openSettings()
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
  function toggleWidget(key: string) {
    settings.update((s) => ({ ...s, widgets: { ...s.widgets, [key]: !s.widgets[key] } }))
  }
</script>

<header class="topbar">
  <div class="left">
    <div class="menu-wrap">
      <button class="picon" title={$t('settings')} aria-label={$t('settings')} onclick={() => (open = !open)}>
        <!-- control / sliders (CasaOS control-outline) -->
        <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
          <line x1="4" y1="7" x2="20" y2="7" /><circle cx="9" cy="7" r="2.2" fill="#fff" />
          <line x1="4" y1="17" x2="20" y2="17" /><circle cx="15" cy="17" r="2.2" fill="#fff" />
        </svg>
      </button>

      {#if open}
        <div class="dropdown" use:clickOutside={() => (open = false)}>
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

          <button class="more" onclick={more}>
            <span>{$t('more')}</span>
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M9 6l6 6-6 6" />
            </svg>
          </button>
        </div>
      {/if}
    </div>
  </div>

  <div class="spacer"></div>

  {#if alerts.length > 0}
    <button
      class="picon bell"
      title={$t('notifications')}
      aria-label={$t('notifications')}
      onclick={() => openSettings('notifications')}
    >
      <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">
        <path d="M9 17a3 3 0 0 0 6 0M12 6v1M8 17V12a4 4 0 0 1 8 0v5" />
      </svg>
      <span class="count" class:critical={worst === 'critical'}>{alerts.length}</span>
    </button>
  {/if}
</header>

<style>
  /* White CasaOS-style navbar with left icon cluster. */
  .topbar {
    position: fixed;
    top: 0;
    left: 0;
    right: 0;
    height: 3.25rem;
    z-index: 20;
    display: flex;
    align-items: center;
    padding: 0 1rem;
    background: #fff;
    border-bottom: 1px solid hsla(208, 16%, 90%, 1);
  }
  .left {
    display: flex;
    align-items: center;
    gap: 0.25rem;
  }
  .spacer {
    flex: 1;
  }
  .picon {
    display: grid;
    place-items: center;
    width: 2.25rem;
    height: 2.25rem;
    border-radius: 6px;
    background: transparent;
    border: none;
    color: #363636;
    cursor: pointer;
    transition: background 0.15s;
  }
  .picon:hover {
    background: rgba(0, 0, 0, 0.05);
  }
  /* Only rendered when there is something to say, so it needs no resting state: an
     always-present bell that is usually empty trains people not to look at it. */
  .bell {
    position: relative;
    margin-right: 0.35rem;
  }
  .count {
    position: absolute;
    top: 0.15rem;
    right: 0.1rem;
    min-width: 1rem;
    padding: 0 0.2rem;
    border-radius: 999px;
    background: var(--warning, #d29922);
    color: #fff;
    font-size: 0.65rem;
    font-weight: 700;
    line-height: 1rem;
  }
  .count.critical {
    background: var(--red, #d1242f);
  }
  .menu-wrap {
    position: relative;
  }
  .dropdown {
    position: absolute;
    left: 0;
    top: 2.75rem;
    width: 17rem;
    background: #fff;
    border-radius: 12px;
    padding: 1rem;
    box-shadow: 0 12px 30px rgba(0, 0, 0, 0.18);
    color: var(--grey-800);
    display: flex;
    flex-direction: column;
    gap: 1rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
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
    border-radius: 6px;
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
    border-radius: 6px;
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
