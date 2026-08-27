<script lang="ts">
  import { onMount } from 'svelte'
  import Wallpaper from './lib/components/Wallpaper.svelte'
  import TopBar from './lib/components/TopBar.svelte'
  import SideBar from './lib/components/SideBar.svelte'
  import AppSection from './lib/components/AppSection.svelte'
  import StorePanel from './lib/components/store/StorePanel.svelte'
  import SettingsPage from './lib/components/settings/SettingsPage.svelte'
  import AppSettingsModal from './lib/components/AppSettingsModal.svelte'
  import TipsModal from './lib/components/TipsModal.svelte'
  import MonitorPanel from './lib/components/MonitorPanel.svelte'
  import UninstallDialog from './lib/components/UninstallDialog.svelte'
  import { get } from 'svelte/store'
  import { live } from './lib/live/ws'
  import { subscribeSystem } from './lib/stores/system'
  import { apps } from './lib/stores/apps'
  import {
    storeOpen,
    settingsOpen,
    settingsApp,
    tipsApp,
    uninstallTarget,
    monitorOpen,
  } from './lib/stores/ui'
  import { openStore, start as startRouter } from './lib/route'
  import { loadSettings } from './lib/stores/settings'

  onMount(() => {
    live.connect()
    loadSettings()
    const off = subscribeSystem()
    // The launch page deep-links back here to open an app's Settings or Logs
    // (?app=<id>&panel=logs|settings). Read it before startRouter(), which
    // rewrites the URL to "/" and would drop the query.
    const params = new URLSearchParams(location.search)
    const deepApp = params.get('app')
    const deepPanel = params.get('panel')
    // The path drives the store view (/store, /store/<app>); the older hash links
    // are still honoured once, at mount.
    const stopRouter = startRouter()
    const h = location.hash
    if (h === '#store') openStore()
    else if (h.startsWith('#settings:')) {
      const id = h.slice('#settings:'.length)
      settingsApp.set({ id, name: id, managed: true })
    } else if (deepApp && (deepPanel === 'logs' || deepPanel === 'settings')) {
      openDeepLink(deepApp, deepPanel === 'logs' ? 'logs' : undefined)
    }
    return () => {
      stopRouter()
      off()
    }
  })

  // Open an app's Settings modal from a launch-page deep-link. The app's real
  // name/managed flag come from the live list; because that list loads
  // asynchronously, we open best-effort now and refine once it arrives.
  function openDeepLink(id: string, tab?: string) {
    const cur = get(apps).find((a) => a.id === id)
    settingsApp.set({ id, name: cur?.name ?? id, managed: cur?.managed ?? true, tab })
    if (cur) return
    const unsub = apps.subscribe((list) => {
      const a = list.find((x) => x.id === id)
      if (a) {
        settingsApp.set({ id, name: a.name, managed: a.managed, tab })
        unsub()
      }
    })
    setTimeout(unsub, 5000)
  }
</script>

<Wallpaper />
<TopBar />

<div class="contents">
  <div class="container">
    <div class="columns">
      <div class="col-left">
        <SideBar />
      </div>
      <div class="col-main">
        <AppSection />
      </div>
    </div>
  </div>
</div>

{#if $storeOpen}
  <StorePanel />
{/if}

{#if $settingsOpen}
  <SettingsPage />
{/if}

{#if $settingsApp}
  <AppSettingsModal
    id={$settingsApp.id}
    name={$settingsApp.name}
    managed={$settingsApp.managed}
    initialTab={$settingsApp.tab}
    onclose={() => settingsApp.set(null)}
  />
{/if}

{#if $tipsApp}
  <TipsModal target={$tipsApp} />
{/if}

{#if $monitorOpen}
  <MonitorPanel />
{/if}

{#if $uninstallTarget}
  <UninstallDialog target={$uninstallTarget} />
{/if}

<style>
  .contents {
    position: absolute;
    inset: 0;
    top: 3.25rem;
    overflow-y: auto;
    padding: 1.5rem 0 4rem;
  }
  /*
   * Bulma-parity centered container, matching casa-img's plain `.container`:
   * fluid below desktop, then stepped max-widths of (breakpoint − 2·$gap),
   * with $gap = 32px → 1024−64=960, 1216−64=1152, 1368−64=1304.
   * Source: CasaOS-UI Home.vue + Bulma _variables.scss breakpoints.
   */
  .container {
    width: auto;
    margin: 0 auto;
    padding: 0 0.75rem;
  }
  @media (min-width: 1024px) {
    .container {
      max-width: 960px;
    }
  }
  @media (min-width: 1216px) {
    .container {
      max-width: 1152px;
    }
  }
  @media (min-width: 1368px) {
    .container {
      max-width: 1304px;
    }
  }
  /* `.columns is-variable is-2` → 1rem gutter, 25/75 split (slider/main-content). */
  .columns {
    display: flex;
    gap: var(--grid-gap);
    align-items: flex-start;
  }
  .col-left {
    flex: 0 0 25%;
    min-width: var(--sidebar-min);
  }
  .col-main {
    flex: 1 1 auto;
    min-width: 0;
  }
  /*
   * Below widescreen the sidebar pins to 18rem and main takes the rest
   * (casa-img: `.main-content { width: calc(100% − 18rem) }` until-widescreen).
   */
  @media (max-width: 1215px) {
    .col-left {
      flex: 0 0 var(--sidebar-min);
    }
  }
  /*
   * Phone / narrow "reactive" mode (≤768px): stack into a single column with the
   * app grid first and the widgets panel moved to the end of the page. Apps stay
   * a 2-up grid here (the ≤1024 touch tier in AppSection already gives 2 columns).
   */
  @media (max-width: 768px) {
    .columns {
      flex-direction: column;
    }
    /* Both panels span the full page width once stacked (align-items:flex-start
       on .columns would otherwise shrink them to content width). */
    .col-main {
      order: 1;
      width: 100%;
    }
    .col-left {
      order: 2;
      flex: 0 0 auto;
      width: 100%;
    }
  }
</style>
