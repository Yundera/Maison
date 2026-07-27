<script lang="ts">
  import { clickOutside } from '../actions'
  import { settings } from '../stores/settings'
  import { t, languages } from '../i18n'
  import { openSettings } from '../route'

  let open = $state(false)

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
