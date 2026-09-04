<script lang="ts">
  import { onMount } from 'svelte'
  import { storeApp } from '../../stores/ui'
  import { backToCatalog, closeStore, openSettings, openStoreApp } from '../../route'
  import { bare, refOf, refPath } from '../../storeref'
  import { fetchStore, type StoreApp, type StoreData } from '../../stores/store'
  import { apps } from '../../stores/apps'
  import { sanitizeProject } from '../../project'
  import { t } from '../../i18n'
  import AppDetail from './AppDetail.svelte'
  import InstallButton from './InstallButton.svelte'

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

  const installedIds = $derived(new Set($apps.map((a) => a.id)))
  const isInstalled = (a: StoreApp) => installedIds.has(sanitizeProject(a.id))

  // The merged catalog: one entry per app, the copy that won the id collision.
  // The payload carries every store's copy — see StoreApp.primary — so anything
  // that must show each app once reads this rather than data.apps.
  const catalog = $derived(data ? data.apps.filter((a) => a.primary !== false) : [])
  // Developers across the whole payload, not just the merged catalog: the browse
  // is grouped by store and shows every store's own copy, so a developer that only
  // appears in a non-primary copy must still be selectable.
  const developers = $derived(
    [...new Set((data?.apps ?? []).map((a) => a.developer).filter(Boolean))].sort(),
  )
  // The configured stores, in the order the box has them — from the payload when
  // the server sends them, else recovered from the apps themselves (older server).
  // Order matters here: it is the order the groups appear in.
  const stores = $derived.by(() => {
    if (data?.sources?.length) return data.sources
    const byURL = new Map<string, string>()
    for (const a of data?.apps ?? []) {
      if (a.store && !byURL.has(a.store)) byURL.set(a.store, a.store_name || a.store)
    }
    return [...byURL].map(([url, name]) => ({ url, name }))
  })
  // What to call a store: the name it declares in store.json, else its address
  // minus the implied scheme. Never "owner/repo" derived from the path — that is
  // one forge's layout, meaningless elsewhere, and it collapses two refs of the
  // same repository (main and a test branch) into one label you cannot tell apart.
  // Same rule the server states in appstore.readStoreName. Two stores that declare
  // the *same* name hit that problem anyway — two branches of one repo share their
  // store.json — so a name used twice falls back to the address for both.
  const storeLabels = $derived.by(() => {
    const uses = new Map<string, number>()
    for (const s of stores) {
      const n = s.name && s.name !== s.url ? s.name : ''
      if (n) uses.set(n, (uses.get(n) ?? 0) + 1)
    }
    const out = new Map<string, string>()
    for (const s of stores) {
      const n = s.name && s.name !== s.url ? s.name : ''
      out.set(s.url, n && uses.get(n) === 1 ? n : bare(s.url))
    }
    return out
  })
  const storeLabel = (url: string): string => storeLabels.get(url) ?? bare(url)
  const browsing = $derived(
    category === 'All' && developer === 'All' && store === 'All' && !search.trim(),
  )

  // Everything matching the toolbar, across every store. The store is NOT filtered
  // here — it decides which *group* an app lands in, below.
  const filtered = $derived.by(() => {
    if (!data) return [] as StoreApp[]
    const q = search.trim().toLowerCase()
    return data.apps.filter((a) => {
      if (category !== 'All' && a.category !== category) return false
      if (developer !== 'All' && a.developer !== developer) return false
      if (q && !`${a.name} ${a.tagline} ${a.category}`.toLowerCase().includes(q)) return false
      return true
    })
  })

  // The browse, grouped by store rather than merged. Stores are never folded into
  // one another: an app shipped by two of them appears under each, with that
  // store's own version and metadata, and installing from a tile installs the copy
  // it belongs to. Picking a store in the toolbar narrows this to that one group —
  // the heading stays, so what you are looking at is always named.
  //
  // A store contributing nothing to the current filter drops out; a store with no
  // apps at all never appears. The toolbar is not repeated per group.
  const groups = $derived(
    stores
      .filter((s) => store === 'All' || s.url === store)
      .map((s) => ({
        url: s.url,
        name: storeLabel(s.url),
        apps: filtered.filter((a) => a.store === s.url),
      }))
      .filter((g) => g.apps.length > 0),
  )

  const featured = $derived.by(() => {
    if (!data) return [] as StoreApp[]
    const byId = new Map(catalog.map((a) => [a.id.toLowerCase(), a]))
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

  // A thumbnail goes into a CSS url() token, which — unlike an <img src> — is
  // parsed by the CSS tokenizer: quotes, parentheses and whitespace in the value
  // end the token and break the rule. An asset served from this box carries a
  // query string (the store it came from), and one named as a URL is whatever the
  // store wrote, so the value is quoted and the two characters that could still
  // escape those quotes are escaped.
  function cssUrl(src: string | undefined): string | undefined {
    if (!src) return undefined
    return `url("${src.replace(/["\\]/g, '\\$&')}")`
  }
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
          ref={selected}
          installed={installedIds.has(sanitizeProject(selected.id))}
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
              {#each stores as s (s.url)}
                <option value={s.url} title={s.url}>{storeLabel(s.url)}</option>
              {/each}
            </select>
          {/if}
          <input class="search" placeholder={$t('search_apps')} bind:value={search} />
          <div class="spacer"></div>
          <span class="count-all">{$t('store_apps_count', { count: String(data.apps.length) })}</span>
          <!-- Managing sources is box configuration, not browsing: it lives in
               settings, and this is the way there. openSettings navigates, which
               unmounts this panel — so re-opening the store re-fetches the catalog
               and a store added over there shows up without any invalidation here. -->
          <button class="manage" onclick={() => openSettings('store')}>{$t('app_stores_manage')} →</button>
        </div>

        {#each groups as g (g.url)}
          <section>
            <h4 class="section-title" title={g.url}>
              {g.name}
              {#if !browsing}<span class="count">{g.apps.length}</span>{/if}
            </h4>
            <div class="grid">
              {#each g.apps as app (refPath(refOf(app)))}{@render card(app)}{/each}
            </div>
          </section>
        {/each}
      {/if}
    </div>
  </div>
</div>

{#snippet hero(app: StoreApp)}
  <div class="hero" onclick={() => openStoreApp(refOf(app))} role="presentation">
    <div class="thumb" style:background-image={cssUrl(app.thumbnail)}></div>
    <div class="hero-body">
      <img class="plate" src={app.icon} alt="" loading="lazy" />
      <div class="hero-meta">
        <span class="name one-line">{app.name}</span>
        <span class="tag one-line">{app.tagline}</span>
      </div>
      <InstallButton ref={refOf(app)} installed={isInstalled(app)} />
    </div>
  </div>
{/snippet}

{#snippet card(app: StoreApp)}
  <div class="app-item" onclick={() => openStoreApp(refOf(app))} role="presentation">
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
      <InstallButton ref={refOf(app)} installed={isInstalled(app)} />
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
  .count-all {
    font-size: 0.8rem;
    color: #8b98a5;
    white-space: nowrap;
  }
  .manage {
    border: none;
    background: none;
    padding: 0;
    font-size: 0.8rem;
    color: var(--primary);
    white-space: nowrap;
    cursor: pointer;
  }
  .manage:hover {
    text-decoration: underline;
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
  .section-title .count {
    margin-left: 0.5rem;
    font-size: 0.8rem;
    color: #8b98a5;
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
