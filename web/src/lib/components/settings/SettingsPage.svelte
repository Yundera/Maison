<script lang="ts">
  // The box-wide settings page: a rail of sections on the left, the selected
  // section's panel on the right.
  //
  // It is a full-screen view rather than a modal because it is meant to grow — the
  // TopBar dropdown it replaces is 17rem wide and was already too small for the
  // domains list alone. Like StorePanel, it layers over the dashboard rather than
  // replacing it, which keeps the tiles' WebSocket subscriptions alive underneath
  // instead of tearing them down and rebuilding on every visit.
  //
  // Adding a section: an entry in route.ts's SETTINGS_SECTIONS (that is the URL
  // vocabulary), plus its label/icon/panel in the three switches below.
  import { SETTINGS_SECTIONS, openSettings, closeSettings, type SettingsSection } from '../../route'
  import { settingsSection } from '../../stores/ui'
  import { t } from '../../i18n'
  import DomainSection from './DomainSection.svelte'
  import AppEnvSection from './AppEnvSection.svelte'

  // route.ts guarantees the store holds a real section (it normalises the URL), so
  // this cast only re-states what the router already checked.
  const current = $derived($settingsSection as SettingsSection)

  const labelKey = (s: SettingsSection) => (s === 'domain' ? 'domains' : 'app_env')

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') closeSettings()
  }
</script>

<svelte:window {onkeydown} />

<div class="page">
  <header class="head">
    <h2 class="title">{$t('settings')}</h2>
    <button class="close" aria-label={$t('back')} onclick={closeSettings}>✕</button>
  </header>

  <div class="body">
    <nav class="rail" aria-label={$t('settings')}>
      {#each SETTINGS_SECTIONS as s (s)}
        <button class="item" class:active={current === s} aria-current={current === s ? 'page' : undefined} onclick={() => openSettings(s)}>
          <span class="ico" aria-hidden="true">
            {#if s === 'domain'}
              <!-- globe -->
              <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.7">
                <circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3a14 14 0 0 1 0 18a14 14 0 0 1 0-18" />
              </svg>
            {:else}
              <!-- terminal -->
              <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round">
                <rect x="3" y="4" width="18" height="16" rx="2" /><path d="M7 9l3 3-3 3M13 15h4" />
              </svg>
            {/if}
          </span>
          <span>{$t(labelKey(s))}</span>
        </button>
      {/each}
    </nav>

    <main class="panel">
      {#if current === 'domain'}
        <DomainSection />
      {:else if current === 'env'}
        <AppEnvSection />
      {/if}
    </main>
  </div>
</div>

<style>
  .page {
    position: fixed;
    inset: 0;
    z-index: 90;
    background: #fff;
    color: var(--grey-800);
    display: flex;
    flex-direction: column;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: hsla(208, 16%, 94%, 1);
    padding: 1rem 1.25rem 0.9rem 1.5rem;
    border-bottom: 1px solid hsla(208, 16%, 90%, 1);
    flex: none;
  }
  .title {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
    color: #29343d;
  }
  .close {
    width: 1.6rem;
    height: 1.6rem;
    border: none;
    border-radius: 4px;
    background: transparent;
    color: #4a5560;
    font-size: 0.9rem;
    cursor: pointer;
  }
  .close:hover {
    background: rgba(0, 0, 0, 0.06);
  }

  .body {
    flex: 1;
    min-height: 0;
    display: flex;
  }
  .rail {
    flex: 0 0 15rem;
    border-right: 1px solid hsla(208, 16%, 90%, 1);
    padding: 1rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    overflow-y: auto;
  }
  .item {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    width: 100%;
    padding: 0.55rem 0.75rem;
    border: none;
    border-radius: 6px;
    background: transparent;
    color: var(--grey-800);
    font-size: 0.9rem;
    text-align: left;
    cursor: pointer;
  }
  .item:hover {
    background: rgba(0, 0, 0, 0.04);
  }
  .item.active {
    background: hsla(208, 16%, 94%, 1);
    font-weight: 600;
    color: #29343d;
  }
  .ico {
    display: grid;
    place-items: center;
    color: var(--grey-600);
    flex: none;
  }
  .item.active .ico {
    color: var(--primary);
  }

  .panel {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
    padding: 1.75rem 2rem 3rem;
  }

  /* Narrow: the rail becomes a scrollable strip above the panel. */
  @media (max-width: 768px) {
    .body {
      flex-direction: column;
    }
    .rail {
      flex: none;
      flex-direction: row;
      border-right: none;
      border-bottom: 1px solid hsla(208, 16%, 90%, 1);
      padding: 0.5rem 0.75rem;
      overflow-x: auto;
    }
    .item {
      width: auto;
      white-space: nowrap;
    }
    .panel {
      padding: 1.25rem 1rem 3rem;
    }
  }
</style>
