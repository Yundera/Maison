<script lang="ts">
  /**
   * The per-app usage table: one sortable row per compose project.
   *
   * Shared by the two places that show it — the monitor modal behind the System
   * status widget, and the Apps tab of the Resources page. Both read the same
   * "appstats" channel, so the only thing that would differ if this were written
   * twice is the details, which is exactly the kind of difference a user notices
   * and cannot explain.
   *
   * It owns the sort and the join against the apps store; the surrounding totals,
   * heading and chrome belong to each caller.
   */
  import { appStats } from '../stores/appstats'
  import { apps } from '../stores/apps'
  import { renderSize } from '../format'
  import { t } from '../i18n'

  let { onopen = undefined as ((id: string) => void) | undefined } = $props()

  type SortKey = 'cpu' | 'mem'
  let sort = $state<SortKey>('cpu')

  const snap = $derived($appStats)

  // The apps whose tiles exist, keyed by the compose project the sampler reports.
  const byId = $derived(new Map($apps.map((a) => [a.id, a])))

  const rows = $derived(
    [...(snap?.apps ?? [])]
      .map((s) => ({
        ...s,
        app: byId.get(s.id),
        name: byId.get(s.id)?.name ?? s.id,
      }))
      // The backend already orders by CPU; re-sorting here is what makes the
      // column headers switchable without another round trip.
      .sort((a, b) =>
        sort === 'cpu'
          ? b.cpu_percent - a.cpu_percent || b.mem_usage - a.mem_usage
          : b.mem_usage - a.mem_usage || b.cpu_percent - a.cpu_percent,
      ),
  )

  function open(id: string) {
    if (onopen && byId.get(id)) onopen(id)
  }
</script>

<div class="head">
  <span class="col-app">{$t('app')}</span>
  <button class="col-num" class:active={sort === 'cpu'} onclick={() => (sort = 'cpu')}>CPU</button>
  <button class="col-num" class:active={sort === 'mem'} onclick={() => (sort = 'mem')}>RAM</button>
</div>

<div class="rows">
  {#if !snap}
    <p class="hint">{$t('loading')}</p>
  {:else if !rows.length}
    <p class="hint">{$t('monitor_empty')}</p>
  {:else}
    {#each rows as r (r.id)}
      <button class="row" onclick={() => open(r.id)} disabled={!onopen || !r.app}>
        <span class="icon">
          {#if r.app?.icon}
            <img src={r.app.icon} alt="" />
          {:else}
            <span class="letter">{r.name.slice(0, 1).toUpperCase()}</span>
          {/if}
        </span>
        <span class="name">
          {r.name}
          {#if r.containers > 1}
            <span class="svc">{$t('monitor_services', { count: String(r.containers) })}</span>
          {/if}
        </span>
        <span class="metric">
          <span class="bar"><span class="fill" style:width={`${r.cpu_percent}%`}></span></span>
          <span class="num">{r.cpu_percent.toFixed(1)}%</span>
        </span>
        <span class="metric">
          <span class="bar">
            <span class="fill mem" style:width={`${r.mem_percent}%`}></span>
          </span>
          <span class="num">{renderSize(r.mem_usage)}</span>
        </span>
      </button>
    {/each}
  {/if}
</div>

<style>
  .head {
    display: grid;
    grid-template-columns: 1fr 8rem 8rem;
    align-items: center;
    gap: 0.5rem;
    padding: 0 0.5rem 0.35rem;
    border-bottom: 1px solid var(--border);
  }
  .col-app {
    font-size: 0.75rem;
    color: var(--text-muted);
  }
  .col-num {
    background: none;
    border: none;
    padding: 0;
    text-align: right;
    font-size: 0.75rem;
    color: var(--text-muted);
    cursor: pointer;
  }
  .col-num.active {
    color: var(--primary);
    font-weight: 600;
  }
  .rows {
    overflow-y: auto;
    margin: 0 -0.25rem;
    padding: 0.25rem;
  }
  .row {
    display: grid;
    grid-template-columns: 1.75rem 1fr 8rem 8rem;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
    padding: 0.45rem 0.5rem;
    background: none;
    border: none;
    border-radius: 8px;
    text-align: left;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
  .row:hover:not(:disabled) {
    background: var(--surface-3);
  }
  .row:disabled {
    cursor: default;
  }
  .icon {
    width: 1.75rem;
    height: 1.75rem;
    display: grid;
    place-items: center;
  }
  .icon img {
    width: 100%;
    height: 100%;
    object-fit: contain;
    border-radius: var(--radius-thumb);
  }
  .letter {
    width: 100%;
    height: 100%;
    display: grid;
    place-items: center;
    border-radius: var(--radius-thumb);
    background: var(--primary);
    color: var(--text-on-accent);
    font-size: 0.75rem;
    font-weight: 600;
  }
  .name {
    font-size: 0.875rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .svc {
    margin-left: 0.35rem;
    font-size: 0.7rem;
    color: var(--text-subtle);
  }
  .metric {
    display: grid;
    grid-template-columns: 1fr auto;
    align-items: center;
    gap: 0.4rem;
  }
  .bar {
    height: 6px;
    border-radius: 3px;
    background: var(--surface-3);
    overflow: hidden;
  }
  .fill {
    display: block;
    height: 100%;
    max-width: 100%;
    background: var(--primary);
    border-radius: 3px;
    transition: width 0.5s ease;
  }
  .fill.mem {
    background: var(--turquoise);
  }
  .num {
    font-size: 0.75rem;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    min-width: 3.75rem;
    text-align: right;
  }
  .hint {
    margin: 1rem 0;
    text-align: center;
    color: var(--text-subtle);
    font-size: 0.85rem;
  }
  @media (max-width: 560px) {
    .head {
      grid-template-columns: 1fr 5rem 5rem;
    }
    .row {
      grid-template-columns: 1.75rem 1fr 5rem 5rem;
    }
    .metric .bar {
      display: none;
    }
  }
</style>
