<script lang="ts">
  /**
   * The graphs on an app's Stats tab: Live, or a recorded window.
   *
   *   live      the frames the tab's per-service stats stream already delivers,
   *             buffered here for ~20 minutes. One service at a time.
   *   recorded  the app's history ring (GET /api/apps/{id}/history): one point a
   *             minute for thirty days, SUMMED over the app's containers — the ring
   *             is fixed-width per app, so there is no per-service line, and the tab
   *             says so rather than labelling the sum as the selected service. Disk
   *             size is one point a day: measuring a folder is a walk.
   *
   * Same split, same reasons as Settings › Resources (docs/resources.md).
   */
  import { untrack } from 'svelte'
  import { fetchAppHistory, type AppHistory } from '../stores/apphistory'
  import { renderSize, renderThroughput } from '../format'
  import Sparkline from './Sparkline.svelte'

  interface Frame {
    cpu_percent: number
    mem_usage: number
    mem_limit: number
    cpu_of_host?: boolean
  }

  let {
    id,
    stat = null,
    service = '',
    multiService = false,
  }: { id: string; stat?: Frame | null; service?: string; multiService?: boolean } = $props()

  type Range = 'live' | '1h' | '24h' | '7d' | '30d'
  const RANGES: { key: Range; label: string; ms: number }[] = [
    { key: 'live', label: 'Live', ms: 0 },
    { key: '1h', label: '1 h', ms: 3_600_000 },
    { key: '24h', label: '24 h', ms: 86_400_000 },
    { key: '7d', label: '7 d', ms: 7 * 86_400_000 },
    { key: '30d', label: '30 d', ms: 30 * 86_400_000 },
  ]
  let range = $state<Range>('live')

  // --- live buffer: the stream's frames, kept for ~20 minutes at 2s ---
  const LIVE_MAX = 600
  type LivePoint = { at: number; cpu: number; mem: number }
  let live = $state<LivePoint[]>([])
  // A different service is a different line: start it over.
  $effect(() => {
    service
    untrack(() => (live = []))
  })
  // Appending reads `live`, so it is untracked: an effect that read and wrote the
  // same state would re-run on its own write.
  $effect(() => {
    const f = stat
    if (!f) return
    untrack(() => {
      const next = live.length >= LIVE_MAX ? live.slice(1) : live.slice()
      next.push({ at: Date.now(), cpu: f.cpu_percent, mem: f.mem_usage })
      live = next
    })
  })

  // --- recorded ---
  let hist = $state<AppHistory | null>(null)
  let loading = $state(false)
  let error = $state('')
  $effect(() => {
    const r = RANGES.find((x) => x.key === range)!
    const appId = id
    if (r.key === 'live') {
      hist = null
      return
    }
    let cancelled = false
    const load = async () => {
      loading = true
      try {
        const to = Date.now()
        const h = await fetchAppHistory(appId, to - r.ms, to)
        if (!cancelled) {
          hist = h
          error = ''
        }
      } catch (e) {
        if (!cancelled) error = e instanceof Error ? e.message : String(e)
      } finally {
        if (!cancelled) loading = false
      }
    }
    load()
    const timer = setInterval(load, 60_000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  })

  const recorded = $derived(range !== 'live')
  const gapMs = $derived(recorded ? (hist?.step_ms ?? 60_000) : 2_000)

  const cpuSeries = $derived(
    recorded
      ? (hist?.spans ?? []).map((s) => ({
          at: Date.parse(s.at),
          value: s.cpu.avg,
          min: s.cpu.min,
          max: s.cpu.max,
        }))
      : live.map((p) => ({ at: p.at, value: p.cpu })),
  )
  const memSeries = $derived(
    recorded
      ? (hist?.spans ?? []).map((s) => ({
          at: Date.parse(s.at),
          value: s.mem.avg,
          min: s.mem.min,
          max: s.mem.max,
        }))
      : live.map((p) => ({ at: p.at, value: p.mem })),
  )
  const readSeries = $derived(
    (hist?.spans ?? []).map((s) => ({ at: Date.parse(s.at), value: s.disk_read })),
  )
  const writeSeries = $derived(
    (hist?.spans ?? []).map((s) => ({ at: Date.parse(s.at), value: s.disk_write })),
  )
  const sizeSeries = $derived(
    (hist?.disk ?? []).map((d) => ({ at: Date.parse(d.at), value: d.bytes })),
  )

  const lastSpan = $derived(hist?.spans.at(-1))
  const peak = (xs: { max?: number; value: number }[]) =>
    xs.reduce((m, x) => Math.max(m, x.max ?? x.value), 0)
  const cpuNow = $derived(recorded ? lastSpan?.cpu.avg : stat?.cpu_percent)
  const memNow = $derived(recorded ? lastSpan?.mem.avg : stat?.mem_usage)
  const pct = (v?: number) => (v == null ? '—' : `${v.toFixed(1)}%`)
  // The Docker fallback reports CPU per core (100% = one core); say which it is.
  const cpuScope = $derived(recorded || stat?.cpu_of_host ? 'of the whole box' : 'of one core')
</script>

<div class="history">
  <div class="ranges" role="group" aria-label="Range">
    {#each RANGES as r (r.key)}
      <button class="range" class:active={range === r.key} onclick={() => (range = r.key)}>
        {r.label}
      </button>
    {/each}
  </div>

  {#if recorded && hist?.reason}
    <p class="hint">{hist.reason}</p>
  {/if}
  {#if recorded && multiService}
    <p class="hint">Recorded history is the whole app — every container summed.</p>
  {/if}
  {#if error}
    <p class="error">{error}</p>
  {/if}

  {#if !recorded || hist?.recording}
    <div class="cards">
      <div class="card">
        <span class="k">CPU</span>
        <span class="v">{pct(cpuNow)}</span>
        <span class="sub">
          {cpuScope}{#if recorded && cpuSeries.length}&nbsp;· peak {pct(peak(cpuSeries))}{/if}
        </span>
        <Sparkline points={cpuSeries} {gapMs} label="CPU" />
      </div>
      <div class="card">
        <span class="k">Memory</span>
        <span class="v">{memNow == null ? '—' : renderSize(memNow)}</span>
        <span class="sub">
          {#if recorded && memSeries.length}
            peak {renderSize(peak(memSeries))}
          {:else if !recorded && stat?.mem_limit}
            of {renderSize(stat.mem_limit)}
          {:else}
            &nbsp;
          {/if}
        </span>
        <Sparkline points={memSeries} {gapMs} color="var(--turquoise)" label="Memory" />
      </div>
      {#if recorded}
        <div class="card">
          <span class="k">Disk read</span>
          <span class="v">{renderThroughput(lastSpan?.disk_read)}</span>
          <span class="sub">peak {renderThroughput(peak(readSeries))}</span>
          <Sparkline points={readSeries} {gapMs} label="Disk read" />
        </div>
        <div class="card">
          <span class="k">Disk write</span>
          <span class="v">{renderThroughput(lastSpan?.disk_write)}</span>
          <span class="sub">peak {renderThroughput(peak(writeSeries))}</span>
          <Sparkline points={writeSeries} {gapMs} color="var(--green)" label="Disk write" />
        </div>
      {/if}
    </div>
  {/if}

  {#if recorded}
    <div class="cards">
      <div class="card">
        <span class="k">Data folder</span>
        <span class="v">{sizeSeries.length ? renderSize(sizeSeries.at(-1)!.value) : '—'}</span>
        <span class="sub">
          {#if sizeSeries.length}
            measured once a day
          {:else}
            not measured yet — once a day, the first time about ten minutes after start
          {/if}
        </span>
        <Sparkline points={sizeSeries} gapMs={86_400_000} color="var(--orange)" label="Data folder" />
      </div>
    </div>
  {/if}

  {#if loading && !hist}
    <p class="hint">Loading…</p>
  {/if}
</div>

<style>
  .history {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .ranges {
    display: flex;
    gap: 0.25rem;
    flex-wrap: wrap;
  }
  .range {
    background: none;
    border: 1px solid transparent;
    border-radius: 6px;
    padding: 0.25rem 0.6rem;
    font-size: 0.78rem;
    color: var(--grey-600);
    cursor: pointer;
  }
  .range:hover,
  .range.active {
    background: hsla(208, 16%, 94%, 1);
  }
  .range.active {
    color: #29343d;
    font-weight: 600;
  }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));
    gap: 0.75rem;
  }
  .card {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    padding: 0.75rem 0.9rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
  }
  .card .k {
    font-size: 0.72rem;
    color: var(--grey-600);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .card .v {
    font-size: 1.15rem;
    font-weight: 600;
    color: #29343d;
    font-variant-numeric: tabular-nums;
  }
  .sub {
    font-size: 0.72rem;
    color: var(--grey-600);
  }
  .hint {
    margin: 0;
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .error {
    margin: 0;
    font-size: 0.8rem;
    color: var(--red, #c0392b);
  }
</style>
