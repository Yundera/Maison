<script lang="ts">
  import { get } from 'svelte/store'
  import { clickOutside } from '../actions'
  import { appAction, appProgress, dismissAppError, openApp, appUrl, type App } from '../stores/apps'
  import { removeLink, type Link } from '../stores/links'
  import { settingsApp, tipsApp, uninstallTarget, tileDragging } from '../stores/ui'
  import { openStore } from '../route'
  import { t } from '../i18n'

  export type TileData =
    | { kind: 'system'; id: string; name: string }
    | { kind: 'app'; id: string; app: App }
    | { kind: 'link'; id: string; link: Link }

  let { tile }: { tile: TileData } = $props()

  let menuOpen = $state(false)
  let imgFailed = $state(false)

  const name = $derived(
    tile.kind === 'app' ? tile.app.name : tile.kind === 'link' ? tile.link.name : tile.name,
  )
  const icon = $derived(
    tile.kind === 'app' ? tile.app.icon : tile.kind === 'link' ? (tile.link.icon ?? '') : '',
  )
  const stopped = $derived(tile.kind === 'app' && tile.app.status === 'stopped')
  // A lifecycle operation is running on this stack: the tile is greyed with a
  // "…" overlay and its burger menu is suppressed until the op settles.
  const busy = $derived(tile.kind === 'app' && tile.app.busy === true)
  // An install or an uninstall is in flight: the tile is greyed and shows ONE
  // progress bar for the step currently running, coloured by which operation it
  // is (blue download, green start, red uninstall). Same source as the store's
  // install pill, so the two always agree.
  const progress = $derived(tile.kind === 'app' ? appProgress(tile.app) : null)
  const progressing = $derived(progress !== null)
  const failure = $derived(
    tile.kind === 'app' ? (tile.app.install_error ?? '') || (tile.app.uninstall_error ?? '') : '',
  )
  // While installing or uninstalling the tile is not clickable.
  const locked = $derived(busy || progressing)
  // An app is openable only when we can build a click URL for it. Apps without a
  // reachable address (no gateway route and no published port) can't be opened —
  // the tile is greyed and its click is disabled. Stopped apps that DO have a URL
  // stay clickable: opening one lands on the launch gate, which starts it.
  const openable = $derived(tile.kind !== 'app' || appUrl(tile.app) !== '')

  function open() {
    // A drag that just dropped fires a trailing click on this tile; swallow it so
    // reordering never opens the app. Genuine clicks never set this flag.
    if (get(tileDragging)) return
    // Go through the router, not storeOpen: opening the store must push a /store
    // history entry, otherwise Back from an app's detail page lands on whatever
    // was below it (the dashboard) instead of the catalog.
    if (tile.kind === 'system') openStore()
    else if (tile.kind === 'app') {
      if (!openable) return
      openApp(tile.app)
    } else window.open(tile.link.url, '_blank', 'noopener')
  }

  async function act(action: 'start' | 'stop' | 'restart') {
    menuOpen = false
    if (tile.kind === 'app') await appAction(tile.app.id, action)
  }

  function remove() {
    menuOpen = false
    if (tile.kind === 'app') {
      uninstallTarget.set({ id: tile.app.id, name })
    } else if (tile.kind === 'link') {
      removeLink(tile.link.id)
    }
  }
</script>

<div class="tile" class:stopped class:busy={locked} class:unavailable={!openable}>
  <div class="glass"></div>

  {#if tile.kind !== 'system' && !locked}
    <button
      class="burger"
      aria-label="Menu"
      onclick={(e) => {
        e.stopPropagation()
        menuOpen = !menuOpen
      }}
    >
      ⋮
    </button>
  {/if}

  {#if busy && !progressing}
    <span class="spinner" title="Working…">…</span>
  {/if}

  {#if failure}
    <button
      class="failed"
      title={`${failure}\n\n${$t('dismiss')}`}
      aria-label={$t('dismiss')}
      onclick={(e) => {
        e.stopPropagation()
        if (tile.kind === 'app') dismissAppError(tile.app.id)
      }}>!</button
    >
  {/if}

  {#if menuOpen && !locked}
    <div class="menu" use:clickOutside={() => (menuOpen = false)}>
      <button disabled={!openable} onclick={() => { menuOpen = false; open() }}>{$t('open')}</button>
      {#if tile.kind === 'app'}
        <button
          onclick={() => {
            menuOpen = false
            if (tile.kind === 'app')
              settingsApp.set({ id: tile.app.id, name: tile.app.name, managed: tile.app.managed })
          }}>{$t('settings')}</button
        >
        {#if tile.app.managed}
          <button
            onclick={() => {
              menuOpen = false
              if (tile.kind === 'app') tipsApp.set({ id: tile.app.id, name: tile.app.name })
            }}>{$t('tips')}</button
          >
        {/if}
        {#if tile.app.status === 'stopped'}
          <button onclick={() => act('start')}>{$t('start')}</button>
        {:else}
          <button onclick={() => act('restart')}>{$t('restart')}</button>
          <button onclick={() => act('stop')}>{$t('stop')}</button>
        {/if}
        {#if !tile.app.protected}
          <button class="danger" onclick={remove}>{$t('uninstall')}</button>
        {/if}
      {:else if tile.kind === 'link'}
        <button class="danger" onclick={remove}>{$t('remove')}</button>
      {/if}
    </div>
  {/if}

  <button class="body" onclick={open} disabled={!openable || locked} title={openable ? '' : 'No reachable web address'}>
    <div class="icon">
      {#if tile.kind === 'system'}
        <img src="/img/appstore.svg" alt="" />
      {:else if icon && !imgFailed}
        <img src={icon} alt="" onerror={() => (imgFailed = true)} />
      {:else}
        <span class="letter">{name.charAt(0).toUpperCase()}</span>
      {/if}
    </div>
    <span class="title one-line">{name}</span>
  </button>

  {#if progress}
    <div class="progress">
      <span class="plabel one-line">{$t(progress.label)} {Math.round(progress.pct)}%</span>
      <div class="pbar">
        <span class="pfill {progress.kind}" style:width={`${progress.pct}%`}></span>
      </div>
    </div>
  {/if}

  {#if tile.kind === 'app' && !tile.app.managed && !progressing}
    <span class="badge">{$t('unmanaged')}</span>
  {/if}
  {#if tile.kind === 'app' && tile.app.health}
    <span
      class="dot"
      class:unhealthy={tile.app.health !== 'healthy'}
      title={tile.app.health}
    ></span>
  {/if}
</div>

<style>
  .tile {
    position: relative;
    aspect-ratio: 1 / 1;
    border-radius: var(--radius-card);
    transition: box-shadow 0.2s;
  }
  .tile:hover {
    box-shadow: 0 0 17px 0 rgba(0, 0, 0, 0.2);
  }
  .body {
    position: relative;
    z-index: 1;
    width: 100%;
    height: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 0.75rem;
    background: none;
    border: none;
    color: inherit;
    padding: 0.5rem;
    /* Inherit the .cell grab/grabbing cursor: the tile drags to reorder and a
       plain click opens it. Disabled/busy states override this below. */
    cursor: inherit;
  }
  .icon {
    width: 64px;
    height: 64px;
    border-radius: var(--radius-icon);
    display: grid;
    place-items: center;
    overflow: hidden;
    color: #fff;
  }
  .icon img {
    width: 100%;
    height: 100%;
    object-fit: cover;
    border-radius: var(--radius-icon);
  }
  .stopped .icon img,
  .stopped .icon,
  .busy .icon img,
  .busy .icon {
    filter: grayscale(1);
    opacity: 0.7;
  }
  /* Busy overlay — a large pulsing "…" filling the tile over the greyed icon,
     matching CasaOS's lifecycle-operation look. */
  .spinner {
    position: absolute;
    inset: 0;
    z-index: 4;
    display: grid;
    place-items: center;
    color: #fff;
    font-size: 4rem;
    line-height: 1;
    letter-spacing: 0.05em;
    text-shadow: 0 2px 6px rgba(0, 0, 0, 0.4);
    pointer-events: none;
    animation: blink 1.2s ease-in-out infinite;
  }
  @keyframes blink {
    0%,
    100% {
      opacity: 0.35;
    }
    50% {
      opacity: 1;
    }
  }
  .busy .body {
    cursor: progress;
  }
  /* Progress overlay — one bar for the step running now; its colour says which
     operation that is (see appProgress in lib/stores/apps.ts). */
  .progress {
    position: absolute;
    left: 0.6rem;
    right: 0.6rem;
    bottom: 0.55rem;
    z-index: 4;
    display: flex;
    flex-direction: column;
    gap: 3px;
    pointer-events: none;
  }
  .plabel {
    font-size: 0.6rem;
    color: var(--grey-100);
    text-align: center;
    font-variant-numeric: tabular-nums;
    text-shadow: 0 1px 2px rgba(0, 0, 0, 0.5);
    max-width: 100%;
  }
  .pbar {
    height: 5px;
    border-radius: 3px;
    background: rgba(255, 255, 255, 0.28);
    overflow: hidden;
  }
  .pfill {
    display: block;
    height: 100%;
    border-radius: 3px;
    transition: width 0.3s ease;
  }
  .pfill.download {
    background: var(--progress-download);
  }
  .pfill.install {
    background: var(--progress-install);
  }
  .pfill.uninstall {
    background: var(--progress-uninstall);
  }
  .pfill.backup {
    background: var(--progress-backup);
  }
  .failed {
    position: absolute;
    top: 0.5rem;
    right: 0.5rem;
    z-index: 4;
    width: 1.3rem;
    height: 1.3rem;
    display: grid;
    place-items: center;
    padding: 0;
    border: none;
    border-radius: 50%;
    background: var(--red);
    color: #fff;
    font-size: 0.85rem;
    font-weight: 700;
    cursor: pointer;
  }
  .unavailable .icon img,
  .unavailable .icon {
    filter: grayscale(1);
    opacity: 0.45;
  }
  .unavailable .title {
    opacity: 0.5;
  }
  .unavailable .body {
    cursor: not-allowed;
  }
  .menu button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  /* No-icon fallback: the blue plate lives on the letter, not on .icon, so real
     icons render against the tile with no backdrop. */
  .letter {
    width: 100%;
    height: 100%;
    display: grid;
    place-items: center;
    border-radius: var(--radius-icon);
    background: var(--primary);
    font-size: 2rem;
    font-weight: 600;
  }
  .title {
    color: var(--grey-100);
    max-width: 90%;
    text-align: center;
  }
  .burger {
    position: absolute;
    top: 0.5rem;
    right: 0.5rem;
    z-index: 3;
    width: 1.6rem;
    height: 1.6rem;
    border: none;
    border-radius: 6px;
    background: rgba(0, 0, 0, 0.25);
    color: #fff;
    font-size: 1rem;
    line-height: 1;
    opacity: 0;
    transition: opacity 0.15s;
  }
  .tile:hover .burger {
    opacity: 1;
  }
  .menu {
    position: absolute;
    top: 2rem;
    right: 0.5rem;
    z-index: 5;
    background: #fff;
    border-radius: 10px;
    padding: 4px;
    min-width: 8.5rem;
    box-shadow: 0 8px 24px rgba(0, 0, 0, 0.25);
    display: flex;
    flex-direction: column;
  }
  .menu button {
    text-align: left;
    height: 2rem;
    padding: 0 0.6rem;
    border: none;
    background: none;
    border-radius: 5px;
    font-size: 0.875rem;
    color: var(--grey-800);
  }
  .menu button:hover {
    background: hsla(208, 16%, 96%, 1);
  }
  .menu button.danger {
    color: var(--red);
  }
  .menu button.danger:hover {
    background: hsla(18, 98%, 94%, 1);
  }
  .badge {
    position: absolute;
    bottom: 0.4rem;
    left: 0.5rem;
    z-index: 2;
    font-size: 0.6rem;
    color: var(--grey-100);
    background: rgba(0, 0, 0, 0.35);
    border-radius: 4px;
    padding: 0 0.3rem;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .dot {
    position: absolute;
    top: 0.6rem;
    left: 0.6rem;
    z-index: 2;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--status-running);
  }
  .dot.unhealthy {
    background: var(--yellow);
  }
</style>
