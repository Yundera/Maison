<script lang="ts">
  import { onMount } from 'svelte'
  import { storeApp } from '../../stores/ui'
  import { backToCatalog, closeStore, openStoreApp } from '../../route'
  import { fetchStore, type StoreApp, type StoreData } from '../../stores/store'
  import { apps } from '../../stores/apps'
  import { sanitizeProject } from '../../project'
  import { t } from '../../i18n'
  import AppDetail from './AppDetail.svelte'
  import InstallButton from './InstallButton.svelte'
  import StoreSources from './StoreSources.svelte'

  let data = $state<StoreData | null>(null)
  let loading = $state(true)
  let error = $state('')
  let category = $state('All')
  let developer = $state('All')
  let store = $state('All')
  let search = $state('')
  // Which app's detail page is showing lives in the URL (/store/<app>), not here:
  // the route owns it so deep links, Back and the in-panel back arrow all agree.
  const selected = $derived($storeApp)

  onMount(load)
  function load() {
    loading = true
    fetchStore()
      .then((d) => (data = d))
      .catch((e) => (error = String(e)))
      .finally(() => (loading = false))
  }
  async function refresh() {
    try {
      data = await fetchStore()
    } catch (e) {
      error = String(e)
    }
  }

  const installedIds = $derived(new Set($apps.map((a) => a.id)))
  const isInstalled = (a: StoreApp) => installedIds.has(sanitizeProject(a.id))

  const developers = $derived(
    data ? [...new Set(data.apps.map((a) => a.developer).filter(Boolean))].sort() : [],
  )
  // Distinct store URLs across the merged catalog; the "All" default keeps every
  // store merged into a single browse.
  const stores = $derived(
    data ? [...new Set(data.apps.map((a) => a.store).filter(Boolean))].sort() : [],
  )
  // Short "owner/repo" (or host) label for a store URL — mirrors StoreSources.
  function storeLabel(u: string): string {
    try {
      const p = new URL(u)
      const seg = p.pathname.split('/').filter(Boolean)
      return seg.length >= 2 ? `${seg[0]}/${seg[1]}` : p.hostname
    } catch {
      return u
    }
  }
  const browsing = $derived(
    category === 'All' && developer === 'All' && store === 'All' && !search.trim(),
  )

  const filtered = $derived.by(() => {
    if (!data) return [] as StoreApp[]
    const q = search.trim().toLowerCase()
    return data.apps.filter((a) => {
      if (category !== 'All' && a.category !== category) return false
      if (developer !== 'All' && a.developer !== developer) return false
      if (store !== 'All' && a.store !== store) return false
      if (q && !`${a.name} ${a.tagline} ${a.category}`.toLowerCase().includes(q)) return false
      return true
    })
  })

  const featured = $derived.by(() => {
    if (!data) return [] as StoreApp[]
    const byId = new Map(data.apps.map((a) => [a.id.toLowerCase(), a]))
    // Dedupe: a store's recommend-list may repeat an id, which would otherwise
    // produce duplicate keys in the featured {#each} (Svelte each_key_duplicate).
    const seen = new Set<string>()
    const out: StoreApp[] = []
    for (const id of data.recommend) {
      const a = byId.get(id.toLowerCase())
      if (a && !seen.has(a.id)) {
        seen.add(a.id)
        out.push(a)
      }
    }
    return out
  })
</script>

<div class="backdrop" onclick={closeStore} role="presentation">
  <div class="panel" onclick={(e) => e.stopPropagation()} role="presentation">
    <header class="head">
      <h3 class="title">{$t('app_store')}</h3>
      <button class="close" aria-label="Close" onclick={closeStore}>✕</button>
    </header>

    <div class="body">
      <!-- The detail page renders straight away, without waiting for the catalog:
           a deep link can pin an app to a store the user has not added, so the app
           may not be in `data` at all and installed-ness comes from the tiles. -->
      {#if selected}
        <AppDetail
          id={selected.app}
          store={selected.store}
          installed={installedIds.has(sanitizeProject(selected.app))}
          onback={backToCatalog}
        />
      {:else if loading}
        <p class="muted">{$t('loading')}</p>
      {:else if error}
        <p class="error">{error}</p>
      {:else if data}
        {#if browsing && featured.length}
          <section>
            <h4 class="section-title">{$t('featured')}</h4>
            <div class="featured-row">
              {#each featured as app (app.id)}{@render hero(app)}{/each}
            </div>
          </section>
        {/if}

        <div class="toolbar">
          <select bind:value={category} aria-label={$t('category')}>
            <option value="All">{$t('category')}: {$t('all')}</option>
            {#each data.categories as c}<option value={c}>{c}</option>{/each}
          </select>
          <select bind:value={developer} aria-label={$t('developer')}>
            <option value="All">{$t('developer')}: {$t('all')}</option>
            {#each developers as d}<option value={d}>{d}</option>{/each}
          </select>
          {#if stores.length > 1}
            <select bind:value={store} aria-label={$t('store')}>
              <option value="All">{$t('store')}: {$t('all')}</option>
              {#each stores as s}<option value={s}>{storeLabel(s)}</option>{/each}
            </select>
          {/if}
          <input class="search" placeholder={$t('search_apps')} bind:value={search} />
          <div class="spacer"></div>
          <StoreSources count={data.apps.length} onchanged={refresh} />
        </div>

        <section>
          {#if !browsing}
            <h4 class="section-title">{filtered.length} apps</h4>
          {/if}
          <div class="grid">
            {#each filtered as app (app.id)}{@render card(app)}{/each}
          </div>
        </section>
      {/if}
    </div>
  </div>
</div>

{#snippet hero(app: StoreApp)}
  <div class="hero" onclick={() => openStoreApp(app.id, app.store)} role="presentation">
    <div class="thumb" style:background-image={app.thumbnail ? `url(${app.thumbnail})` : undefined}></div>
    <div class="hero-body">
      <img class="plate" src={app.icon} alt="" loading="lazy" />
      <div class="hero-meta">
        <span class="name one-line">{app.name}</span>
        <span class="tag one-line">{app.tagline}</span>
      </div>
      <InstallButton id={app.id} store={app.store} installed={isInstalled(app)} />
    </div>
  </div>
{/snippet}

{#snippet card(app: StoreApp)}
  <div class="app-item" onclick={() => openStoreApp(app.id, app.store)} role="presentation">
    <div class="row1">
      <img class="plate" src={app.icon} alt="" loading="lazy" />
      <div class="meta">
        <span class="name one-line">{app.name}</span>
        <span class="tag two-line">{app.tagline}</span>
      </div>
    </div>
    <div class="row2">
      <span class="cat">{app.category}</span>
    </div>
    <!-- Install pill sits on its own row below the app; while installing it
         shows a compact progress pill (the two-bar detail lives on the tile). -->
    <div class="installbar">
      <InstallButton id={app.id} store={app.store} installed={isInstalled(app)} />
    </div>
  </div>
{/snippet}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 90;
    background: var(--scrim);
    display: grid;
    place-items: center;
  }
  /* White CasaOS store modal. */
  .panel {
    width: min(95vw, 81rem);
    height: min(94vh, 900px);
    background: #fff;
    border-radius: 10px;
    color: var(--grey-800);
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: hsla(208, 16%, 94%, 1);
    padding: 1rem 1.25rem 0.9rem 1.5rem;
    border-bottom: 1px solid hsla(208, 16%, 94%, 1);
  }
  .head .title {
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
    overflow-y: auto;
    padding: 1rem 1.5rem 2rem;
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    margin: 0.25rem 0 1.5rem;
    position: sticky;
    top: -1rem;
    background: #fff;
    padding: 0.75rem 0;
    z-index: 5;
  }
  .spacer {
    flex: 1;
  }
  select,
  .search {
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    background: #fff;
    color: var(--grey-800);
    font-size: 0.875rem;
    padding: 0 0.55rem;
  }
  .search {
    width: 12.5rem;
    max-width: 30vw;
  }
  .section-title {
    font-size: 1rem;
    font-weight: 400;
    margin: 0 0 0.9rem;
    color: #29343d;
  }
  section {
    margin-bottom: 1.75rem;
  }

  /* Icon plate — CasaOS .icon-shadow */
  .plate {
    width: 64px;
    height: 64px;
    flex: none;
    object-fit: cover;
    border-radius: 18.75%;
    background: linear-gradient(180deg, #f7fafc 0%, #f0f2f5 100%);
    box-shadow: 1px 2px 4px rgba(0, 0, 0, 0.2);
  }

  /* Featured hero carousel */
  .featured-row {
    display: flex;
    gap: 1.5rem;
    overflow-x: auto;
    padding-bottom: 0.5rem;
    scroll-snap-type: x mandatory;
  }
  .hero {
    flex: 0 0 calc(33.333% - 1rem);
    min-width: 300px;
    scroll-snap-align: start;
    cursor: pointer;
  }
  .thumb {
    aspect-ratio: 16 / 9;
    border-radius: 8px;
    background-size: cover;
    background-position: center;
    background-color: #e9edf1;
    background-image: linear-gradient(135deg, hsla(216, 90%, 54%, 0.25), rgba(0, 0, 0, 0.06));
  }
  .hero-body {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding-top: 1rem;
  }
  .hero-meta {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  /* App card grid */
  .grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 0.75rem 1.25rem;
  }
  @media (max-width: 1366px) {
    .grid {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
  }
  @media (max-width: 1024px) {
    .grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .hero {
      flex-basis: calc(50% - 0.75rem);
    }
  }
  @media (max-width: 560px) {
    .grid {
      grid-template-columns: 1fr;
    }
    .hero {
      flex-basis: 85%;
    }
  }
  .app-item {
    border-radius: 8px;
    padding: 0.6rem;
    cursor: pointer;
    transition: background 0.2s;
  }
  .app-item:hover {
    background: hsl(0, 0%, 97%);
  }
  .row1 {
    display: flex;
    gap: 1rem;
    align-items: flex-start;
  }
  .meta {
    min-width: 0;
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .row2 {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin: 0.4rem 0 0 calc(64px + 1rem);
  }
  .installbar {
    display: flex;
    justify-content: flex-end;
    margin-top: 0.55rem;
  }
  .name {
    font-size: 1rem;
    font-weight: 600;
    color: #29343d;
  }
  .tag {
    font-size: 0.75rem;
    color: hsl(0, 0%, 45%);
    line-height: 1.05rem;
  }
  .two-line {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .hero .name {
    font-weight: 600;
  }
  .hero .tag {
    color: hsl(0, 0%, 45%);
  }
  .cat {
    font-size: 0.75rem;
    color: hsl(0, 0%, 71%);
  }
  .muted {
    color: var(--text-subtle);
  }
  .error {
    color: var(--red);
  }
</style>
