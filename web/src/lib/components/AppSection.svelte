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

  let items = $state<TileData[]>([])
  let dragging = $state(false)
  let addMenu = $state(false)
  let showLinkModal = $state(false)

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
  function saveOrder(ids: string[]) {
    localStorage.setItem('maison.order', JSON.stringify(ids))
  }

  const STORE_TILE: TileData = { kind: 'system', id: '__store', name: 'App Store' }

  function buildOrdered(a: App[], l: Link[]): TileData[] {
    const tiles: TileData[] = [
      ...a.map((app) => ({ kind: 'app', id: 'app:' + app.id, app }) as TileData),
      ...l.map((link) => ({ kind: 'link', id: link.id, link }) as TileData),
    ]
    const order = loadOrder()
    const rank = (id: string) => {
      const i = order.indexOf(id)
      return i < 0 ? Number.MAX_SAFE_INTEGER : i
    }
    tiles.sort((x, y) => rank(x.id) - rank(y.id))
    // App Store system tile is always pinned first.
    return [STORE_TILE, ...tiles]
  }

  // Rebuild the grid when apps/links change, except while a drag is in flight.
  $effect(() => {
    const a = $apps
    const l = $links
    if (dragging) return
    items = buildOrdered(a, l)
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
    saveOrder(rest.map((t) => t.id))
    // A mouse drop fires a trailing `click` on the tile right after this handler.
    // Clear on the next tick so that click is swallowed (see tileDragging) while
    // genuine clicks — which never trigger a drag — still open the app.
    setTimeout(() => tileDragging.set(false), 0)
  }
</script>

<section class="app-section">
  <header class="section-header">
    <h1>{$t('app')}</h1>
    <div class="add-wrap">
      <button class="add" title="Add" aria-label="Add" onclick={() => (addMenu = !addMenu)}>+</button>
      {#if addMenu}
        <div class="add-menu" use:clickOutside={() => (addMenu = false)}>
          <button
            onclick={() => {
              addMenu = false
              showLinkModal = true
            }}>{$t('add_external_link')}</button
          >
        </div>
      {/if}
    </div>
  </header>

  <div
    class="app-list"
    use:dndzone={{ items, flipDurationMs: 200, dropTargetStyle: {} }}
    onconsider={onConsider}
    onfinalize={onFinalize}
  >
    {#each items as tile (tile.id)}
      <div class="cell" class:grabbing={dragging}><Tile {tile} /></div>
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
    color: var(--grey-100);
    margin: 0.25rem 0.25rem 0.75rem;
  }
  h1 {
    font-size: 1.5rem;
    font-weight: 600;
    margin: 0;
  }
  .add-wrap {
    position: relative;
  }
  .add {
    width: 1.75rem;
    height: 1.75rem;
    border-radius: 8px;
    border: none;
    background: rgba(255, 255, 255, 0.12);
    color: var(--grey-100);
    font-size: 1.1rem;
    line-height: 1;
  }
  .add:hover {
    background: rgba(255, 255, 255, 0.2);
  }
  .add-menu {
    position: absolute;
    right: 0;
    top: 2.1rem;
    z-index: 10;
    background: #fff;
    border-radius: 10px;
    padding: 4px;
    box-shadow: 0 8px 24px rgba(0, 0, 0, 0.25);
  }
  .add-menu button {
    display: block;
    width: 100%;
    text-align: left;
    white-space: nowrap;
    height: 2rem;
    padding: 0 0.6rem;
    border: none;
    background: none;
    border-radius: 5px;
    font-size: 0.875rem;
    color: var(--grey-800);
  }
  .add-menu button:hover {
    background: hsla(208, 16%, 96%, 1);
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
