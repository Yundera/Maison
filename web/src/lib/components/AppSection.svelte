<script lang="ts">
  import { onMount } from 'svelte'
  import { dndzone } from 'svelte-dnd-action'
  import Tile, { type TileData } from './Tile.svelte'
  import AddLinkModal from './AddLinkModal.svelte'
  import { apps, loadApps, subscribeApps, type App } from '../stores/apps'
  import { links, type Link } from '../stores/links'
  import { clickOutside } from '../actions'
  import { tileDragging } from '../stores/ui'
  import { t } from '../i18n'
  import { openStore } from '../route'

  // Which grid is on screen. The platform's own pieces declare `view: system` in
  // their compose and get their own; apps with no web UI (the server sorts them,
  // see `view: service`) get theirs; everything else lands in the ordinary one.
  // Deliberately NOT persisted across reloads: a dashboard that reopens on the
  // System grid reads as "where did my apps go".
  type View = 'apps' | 'service' | 'system'
  let view = $state<View>('apps')

  // Parents whose extensions are currently unfolded. Collapsed by default and
  // deliberately not persisted: an extension is the parent's business, and a grid
  // that reopens unfolded is a grid whose layout moved on its own.
  let expanded = $state(new Set<string>())

  let items = $state<TileData[]>([])
  let dragging = $state(false)
  let addMenu = $state(false)
  let showLinkModal = $state(false)

  // A tab only appears once there is something in it, and the switch only once
  // there is a second tab, so a box with no system apps and no services looks
  // exactly as it did before. Extensions are left out of the count: they render
  // under their parent, so a grid holding only extensions would be empty.
  const LABELS: Record<View, string> = { apps: 'app', service: 'services', system: 'system' }
  const views = $derived(
    (['apps', 'service', 'system'] as View[]).filter(
      (v) => v === 'apps' || $apps.some((a) => a.view === v && !a.parent),
    ),
  )

  onMount(() => {
    loadApps()
    const off = subscribeApps()
    return off
  })

  function loadOrder(): string[] {
    try {
      return JSON.parse(localStorage.getItem('maison.order') ?? '[]')
    } catch {
      return []
    }
  }
  /** Persist the visible grid's order, keeping every id it didn't show.
   *  A drag only ever reorders one view; writing just those ids would drop the
   *  other grid's arrangement the first time either is touched. */
  function saveOrder(ids: string[]) {
    const rest = loadOrder().filter((id) => !ids.includes(id))
    localStorage.setItem('maison.order', JSON.stringify([...ids, ...rest]))
  }

  const STORE_TILE: TileData = { kind: 'system', id: '__store', name: 'App Store' }

  /** The tiles for one view, in the operator's saved order.
   *
   *  External links and the App Store tile belong to the app grid — the store
   *  installs apps, not platform pieces. */
  function buildOrdered(v: View, a: App[], l: Link[]): TileData[] {
    // An app that declares a parent the server could resolve is an extension: it
    // is listed under that app, never beside it. Note the children are NOT
    // filtered by view — an extension follows its parent's tile into whichever
    // grid the parent is in, because "under jellyfin" is where it means anything.
    const extensions = new Map<string, App[]>()
    for (const app of a) {
      if (!app.parent) continue
      const kids = extensions.get(app.parent) ?? []
      kids.push(app)
      extensions.set(app.parent, kids)
    }
    const tiles: TileData[] = a
      .filter((app) => (app.view ?? 'apps') === v && !app.parent)
      .map(
        (app) =>
          ({
            kind: 'app',
            id: 'app:' + app.id,
            app,
            extensions: extensions.get(app.id)?.length ?? 0,
            expanded: expanded.has(app.id),
          }) as TileData,
      )
    if (v === 'apps') tiles.push(...l.map((link) => ({ kind: 'link', id: link.id, link }) as TileData))
    const order = loadOrder()
    const rank = (id: string) => {
      const i = order.indexOf(id)
      return i < 0 ? Number.MAX_SAFE_INTEGER : i
    }
    tiles.sort((x, y) => rank(x.id) - rank(y.id))
    // Unfolded extensions ride immediately behind their parent. They are placed
    // here rather than ordered like everything else, so a drag can shuffle them
    // within the row but never strand one away from the app it belongs to: the
    // next rebuild puts it back.
    const withKids: TileData[] = []
    for (const tile of tiles) {
      withKids.push(tile)
      if (tile.kind !== 'app' || !expanded.has(tile.app.id)) continue
      for (const kid of extensions.get(tile.app.id) ?? []) {
        withKids.push({ kind: 'app', id: 'app:' + kid.id, app: kid, child: true } as TileData)
      }
    }
    // App Store system tile is always pinned first.
    return v === 'apps' ? [STORE_TILE, ...withKids] : withKids
  }

  /** Fold or unfold one parent's extensions. Reassigned rather than mutated so
   *  the grid rebuilds. */
  function toggleExtensions(id: string) {
    const next = new Set(expanded)
    if (!next.delete(id)) next.add(id)
    expanded = next
  }

  // Rebuild the grid when the view or apps/links change, except while a drag is
  // in flight.
  $effect(() => {
    const v = view
    const a = $apps
    const l = $links
    expanded
    if (dragging) return
    items = buildOrdered(v, a, l)
  })

  // The last app of a grid can be uninstalled (or stop belonging to it) while
  // that grid is on screen; fall back rather than leave an empty one showing.
  $effect(() => {
    if (!views.includes(view)) view = 'apps'
  })

  // The switch's light thumb slides under the active view. It is measured rather
  // than laid out on equal tracks because the labels differ in width — and differ
  // again per language — so the thumb is re-measured whenever the control resizes
  // (a language switch, a late web font), not only when the view changes.
  let seg = $state<HTMLElement>()
  let thumb = $state({ x: 0, w: 0 })
  function measure() {
    const on = seg?.querySelector<HTMLElement>('button[aria-selected="true"]')
    if (on) thumb = { x: on.offsetLeft, w: on.offsetWidth }
  }
  $effect(() => {
    view
    views
    measure()
  })
  $effect(() => {
    if (!seg) return
    const ro = new ResizeObserver(measure)
    ro.observe(seg)
    return () => ro.disconnect()
  })

  function onConsider(e: CustomEvent<{ items: TileData[] }>) {
    dragging = true
    tileDragging.set(true)
    items = e.detail.items
  }
  function onFinalize(e: CustomEvent<{ items: TileData[] }>) {
    // Keep the App Store tile pinned first regardless of where it was dropped.
    const rest = e.detail.items.filter((t) => t.id !== '__store')
    items = [STORE_TILE, ...rest]
    dragging = false
    // Extensions are positioned by their parent, so their ids are not part of the
    // saved arrangement — persisting them would leave dead entries behind the
    // first time one was folded away.
    saveOrder(rest.filter((t) => !(t.kind === 'app' && t.child)).map((t) => t.id))
    // A mouse drop fires a trailing `click` on the tile right after this handler.
    // Clear on the next tick so that click is swallowed (see tileDragging) while
    // genuine clicks — which never trigger a drag — still open the app.
    setTimeout(() => tileDragging.set(false), 0)
  }
</script>

<section class="app-section">
  <header class="section-header">
    {#if views.length > 1}
      <!-- Dark glass like the tiles beside it, with the active view on a light thumb:
           dark-on-light stays readable on any wallpaper, where the old dimmed title
           words washed out on a light one. -->
      <div class="views" role="tablist" bind:this={seg}>
        <span class="thumb" style:width="{thumb.w}px" style:transform="translateX({thumb.x}px)"></span>
        {#each views as v (v)}
          <button role="tab" aria-selected={view === v} onclick={() => (view = v)}>{$t(LABELS[v])}</button>
        {/each}
      </div>
    {:else}
      <h1>{$t('app')}</h1>
    {/if}
    <!-- An external link is a shortcut the operator adds to their own grid;
         there is nothing to add to the platform's. -->
    {#if view === 'apps'}
      <div class="add-wrap" use:clickOutside={() => (addMenu = false)}>
        <button class="add" title="Add" aria-label="Add" aria-expanded={addMenu} onclick={() => (addMenu = !addMenu)}>
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" aria-hidden="true">
            <path d="M12 5v14M5 12h14" />
          </svg>
        </button>
        {#if addMenu}
          <div class="add-menu popover">
            <button
              onclick={() => {
                addMenu = false
                openStore()
              }}
            >
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" aria-hidden="true">
                <path d="M5 8h14l-1 12H6z" /><path d="M9 8V6a3 3 0 0 1 6 0v2" />
              </svg>
              {$t('app_store')}
            </button>
            <button
              onclick={() => {
                addMenu = false
                showLinkModal = true
              }}
            >
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
                <path d="M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1" /><path d="M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1" />
              </svg>
              {$t('add_external_link')}
            </button>
          </div>
        {/if}
      </div>
    {/if}
  </header>

  <div
    class="app-list"
    use:dndzone={{ items, flipDurationMs: 200, dropTargetStyle: {} }}
    onconsider={onConsider}
    onfinalize={onFinalize}
  >
    {#each items as tile (tile.id)}
      <div class="cell" class:grabbing={dragging}>
        <Tile {tile} ontoggleextensions={() => tile.kind === 'app' && toggleExtensions(tile.app.id)} />
      </div>
    {/each}
  </div>
</section>

{#if showLinkModal}
  <AddLinkModal onclose={() => (showLinkModal = false)} />
{/if}

<style>
  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    min-height: 2.25rem;
    color: var(--grey-100);
    margin: 0.25rem 0.25rem 0.75rem;
  }
  h1 {
    font-size: 1.5rem;
    font-weight: 600;
    margin: 0;
    /* The heading sits on bare wallpaper, not glass: a light wallpaper erases it. */
    text-shadow: 0 1px 3px rgb(0 0 0 / 0.35);
  }
  /* Desktop glass, not the global light .segmented (that one is for panels). */
  .views {
    position: relative;
    display: inline-flex;
    padding: 3px;
    border-radius: var(--r-pill);
    background: var(--glass-color);
    backdrop-filter: var(--glass-blur);
    -webkit-backdrop-filter: var(--glass-blur);
    box-shadow: var(--glass-shadow);
  }
  .thumb {
    position: absolute;
    top: 3px;
    bottom: 3px;
    left: 0;
    border-radius: var(--r-pill);
    background: rgb(255 255 255 / 0.9);
    box-shadow:
      0 1px 2px rgb(0 0 0 / 0.15),
      0 3px 10px rgb(0 0 0 / 0.12);
    transition:
      transform var(--dur) var(--ease-ios),
      width var(--dur) var(--ease-ios);
  }
  .views button {
    position: relative;
    height: 2.25rem;
    padding: 0 1rem;
    border: none;
    border-radius: var(--r-pill);
    background: none;
    font-size: 0.85rem;
    font-weight: 600;
    color: rgb(255 255 255 / 0.82);
    cursor: pointer;
    transition: color var(--dur-fast) var(--ease-ios);
  }
  .views button:hover:not([aria-selected='true']) {
    color: #fff;
  }
  .views button[aria-selected='true'] {
    color: var(--grey-800);
  }
  .views button:focus-visible,
  .add:focus-visible {
    outline: 2px solid rgb(255 255 255 / 0.85);
    outline-offset: 2px;
  }
  @media (prefers-reduced-motion: reduce) {
    .thumb {
      transition: none;
    }
  }
  .add-wrap {
    position: relative;
  }
  .add {
    display: grid;
    place-items: center;
    width: 2.25rem;
    height: 2.25rem;
    border-radius: var(--r-pill);
    border: none;
    background: var(--glass-color);
    backdrop-filter: var(--glass-blur);
    -webkit-backdrop-filter: var(--glass-blur);
    box-shadow: var(--glass-shadow);
    color: var(--grey-100);
    cursor: pointer;
    transition:
      background var(--dur-fast) var(--ease-ios),
      transform var(--dur-fast) var(--ease-ios);
  }
  .add:hover,
  .add[aria-expanded='true'] {
    background: rgb(60 62 70 / 0.55);
  }
  .add:active {
    transform: scale(0.9);
  }
  .add-menu {
    position: absolute;
    right: 0;
    top: calc(100% + 6px);
    z-index: 10;
    padding: 5px;
    transform-origin: top right;
  }
  .add-menu button {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    width: 100%;
    text-align: left;
    white-space: nowrap;
    height: 2.25rem;
    padding: 0 0.7rem 0 0.6rem;
    border: none;
    background: none;
    border-radius: calc(var(--r-md) - 5px);
    font-size: 0.875rem;
    color: var(--grey-800);
    cursor: pointer;
  }
  .add-menu button svg {
    color: var(--grey-600);
  }
  .add-menu button:hover {
    background: rgb(118 118 128 / 0.14);
  }
  /*
   * App grid column counts ported verbatim from casa-img's `.app-list`:
   * touch (<1024) → 2, desktop (≥1024) → 4, fullhd (≥1368) → 5.
   * Tracks are minmax(0, 1fr) so tiles stay equal-width and never overflow.
   * Source: CasaOS-UI AppSection.vue.
   */
  .app-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--grid-gap);
  }
  /* Tiles are draggable to reorder; a plain click still opens the app. */
  .cell {
    cursor: grab;
  }
  .cell.grabbing {
    cursor: grabbing;
  }
  @media (min-width: 1024px) {
    .app-list {
      grid-template-columns: repeat(4, minmax(0, 1fr));
    }
  }
  @media (min-width: 1368px) {
    .app-list {
      grid-template-columns: repeat(5, minmax(0, 1fr));
    }
  }
</style>
