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

  // Which grid is on screen. The platform's own pieces declare `view: system` in
  // their compose and get their own; everything else lands in the ordinary one.
  // Deliberately NOT persisted across reloads: a dashboard that reopens on the
  // System grid reads as "where did my apps go".
  type View = 'apps' | 'system'
  let view = $state<View>('apps')

  let items = $state<TileData[]>([])
  let dragging = $state(false)
  let addMenu = $state(false)
  let showLinkModal = $state(false)

  // The switch only appears once there is something to switch to, so a box with
  // no system apps looks exactly as it did before.
  const hasSystem = $derived($apps.some((a) => a.view === 'system'))

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
   *  `hidden` apps are in neither: they are infrastructure with nothing worth
   *  clicking. External links and the App Store tile belong to the app grid —
   *  the store installs apps, not platform pieces. */
  function buildOrdered(v: View, a: App[], l: Link[]): TileData[] {
    const tiles: TileData[] = a
      .filter((app) => (app.view ?? 'apps') === v)
      .map((app) => ({ kind: 'app', id: 'app:' + app.id, app }) as TileData)
    if (v === 'apps') tiles.push(...l.map((link) => ({ kind: 'link', id: link.id, link }) as TileData))
    const order = loadOrder()
    const rank = (id: string) => {
      const i = order.indexOf(id)
      return i < 0 ? Number.MAX_SAFE_INTEGER : i
    }
    tiles.sort((x, y) => rank(x.id) - rank(y.id))
    // App Store system tile is always pinned first.
    return v === 'apps' ? [STORE_TILE, ...tiles] : tiles
  }

  // Rebuild the grid when the view or apps/links change, except while a drag is
  // in flight.
  $effect(() => {
    const v = view
    const a = $apps
    const l = $links
    if (dragging) return
    items = buildOrdered(v, a, l)
  })

  // The last system app can be uninstalled (or stop declaring itself one) while
  // its grid is on screen; fall back rather than leave an empty one showing.
  $effect(() => {
    if (!hasSystem && view === 'system') view = 'apps'
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
    {#if hasSystem}
      <h1 class="views">
        <button class:active={view === 'apps'} aria-pressed={view === 'apps'} onclick={() => (view = 'apps')}
          >{$t('app')}</button
        >
        <button class:active={view === 'system'} aria-pressed={view === 'system'} onclick={() => (view = 'system')}
          >{$t('system')}</button
        >
      </h1>
    {:else}
      <h1>{$t('app')}</h1>
    {/if}
    <!-- An external link is a shortcut the operator adds to their own grid;
         there is nothing to add to the platform's. -->
    {#if view === 'apps'}
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
    {/if}
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
  /* The heading becomes the switch: same type, the inactive view dimmed rather
     than boxed, so the row still reads as a title and not as a toolbar. */
  .views {
    display: flex;
    gap: 1rem;
  }
  .views button {
    padding: 0;
    border: none;
    background: none;
    font: inherit;
    color: inherit;
    opacity: 0.45;
    cursor: pointer;
    transition: opacity 0.15s;
  }
  .views button:hover {
    opacity: 0.75;
  }
  .views button.active {
    opacity: 1;
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
