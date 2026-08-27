<script lang="ts">
  /**
   * Settings › Resources — what this box is doing, and what it has been doing.
   *
   * Two data sources, deliberately different in kind:
   *
   *   live      the "resources" channel, 2s, sampled by the backend ONLY while
   *             this page is mounted. Carries the full breakdown: per interface,
   *             per device, per filesystem, per process.
   *   recorded  a fixed-size ring file on disk, one point a minute, thirty days.
   *             Fetched once per range change over REST, never pushed. Carries
   *             summed rates only — the record is fixed-width, which is what lets
   *             thirty days fit in 1.32 MiB with no database. Per-interface and
   *             per-device detail therefore exists in the live view alone, and the
   *             page says so rather than quietly showing one interface's line
   *             labelled as the box's.
   */
  import { onMount } from 'svelte'
  import {
    resources,
    subscribeResources,
    fetchHistory,
    deleteHistory,
    fetchBench,
    startDiskBench,
    startNetworkBench,
    type History,
    type BenchResults,
  } from '../../stores/resources'
  import { appStats, subscribeAppStats } from '../../stores/appstats'
  import { settings } from '../../stores/settings'
  import {
    renderSize,
    renderPercent,
    renderThroughput,
    renderMbps,
    renderUptime,
    renderDuration,
  } from '../../format'
  import { t } from '../../i18n'
  import Sparkline from '../Sparkline.svelte'
  import AppUsageRows from '../AppUsageRows.svelte'

  type Tab = 'cpu' | 'network' | 'disk' | 'apps'
  type Range = 'live' | '1h' | '24h' | '7d' | '30d'

  const RANGES: { key: Range; ms: number }[] = [
    { key: 'live', ms: 0 },
    { key: '1h', ms: 3_600_000 },
    { key: '24h', ms: 86_400_000 },
    { key: '7d', ms: 7 * 86_400_000 },
    { key: '30d', ms: 30 * 86_400_000 },
  ]

  let tab = $state<Tab>('cpu')
  let range = $state<Range>('live')

  const d = $derived($resources)

  // ─── the live buffer ────────────────────────────────────────────────────────
  //
  // Held here rather than on the server: it is twenty minutes of points that exist
  // only while this page is open, and shipping it back and forth would cost more
  // than keeping it. Slimmed on arrival — retaining 600 whole snapshots would mean
  // 600 process lists.
  interface LivePoint {
    at: number
    cpu: number
    mem: number
    net: Record<string, [number, number]>
    disk: Record<string, [number, number]>
  }
  const LIVE_MAX = 600 // ~20 min at the channel's 2s cadence
  let livePoints = $state<LivePoint[]>([])

  // ─── recorded history ───────────────────────────────────────────────────────
  let hist = $state<History | null>(null)
  let histError = $state('')
  let loadingHist = $state(false)

  // ─── benchmarks ─────────────────────────────────────────────────────────────
  let bench = $state<BenchResults | null>(null)
  let benchError = $state('')

  onMount(() => {
    const stopChannel = subscribeResources()

    // Appended from the store subscription rather than an $effect on purpose: an
    // effect that copies livePoints and assigns the copy back would read and write
    // the same state, which re-triggers itself. A subscription callback is not
    // reactive, so the read is free.
    const unsub = resources.subscribe((s) => {
      if (!s) return
      const net: Record<string, [number, number]> = {}
      for (const n of s.nets) net[n.iface] = [n.rx_bps, n.tx_bps]
      const disk: Record<string, [number, number]> = {}
      for (const x of s.disks) disk[x.device] = [x.read_bps, x.write_bps]
      livePoints = [...livePoints, { at: s.at, cpu: s.cpu_percent, mem: s.mem.used_percent, net, disk }].slice(
        -LIVE_MAX,
      )
    })

    void loadBench()

    // A run is asynchronous on the server — the request only starts it — so poll
    // while either side is working. A plain interval rather than an $effect keyed
    // on the status, which would tear down and rebuild the timer on every poll.
    const benchPoll = setInterval(() => {
      if (bench?.disk.status === 'running' || bench?.network.status === 'running') void loadBench()
    }, 2000)

    return () => {
      unsub()
      stopChannel()
      clearInterval(benchPoll)
    }
  })

  // The per-app channel is a stats read per running container, so it is subscribed
  // only while its own tab is showing — not for the whole visit to this page.
  $effect(() => {
    if (tab !== 'apps') return
    return subscribeAppStats()
  })

  // Bumped to force a refetch of the current range (after deleting the history).
  // Re-assigning `range` to the same value would not do it: an effect sees only
  // the value the state settles on, not the round trip.
  let reloadKey = $state(0)

  // Load history when the range changes, and keep it fresh while it is showing. A
  // recorded point only appears once a minute, so refreshing faster than that would
  // re-fetch the same series.
  $effect(() => {
    reloadKey
    if (range === 'live') {
      hist = null
      return
    }
    const ms = RANGES.find((r) => r.key === range)!.ms
    let cancelled = false
    const load = async () => {
      loadingHist = true
      try {
        const to = Date.now()
        const got = await fetchHistory(to - ms, to)
        if (!cancelled) {
          hist = got
          histError = ''
        }
      } catch (e) {
        if (!cancelled) histError = e instanceof Error ? e.message : String(e)
      } finally {
        if (!cancelled) loadingHist = false
      }
    }
    void load()
    const id = setInterval(load, 60_000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  })

  // ─── series ─────────────────────────────────────────────────────────────────

  const gapMs = $derived(range === 'live' ? 2_000 : (hist?.step_ms ?? 60_000))
  const recorded = $derived(range !== 'live')

  const cpuSeries = $derived(
    recorded
      ? (hist?.spans ?? []).map((s) => ({
          at: Date.parse(s.at),
          value: s.cpu.avg,
          min: s.cpu.min,
          max: s.cpu.max,
        }))
      : livePoints.map((p) => ({ at: p.at, value: p.cpu })),
  )

  const memSeries = $derived(
    recorded
      ? (hist?.spans ?? []).map((s) => ({
          at: Date.parse(s.at),
          value: s.mem.avg,
          min: s.mem.min,
          max: s.mem.max,
        }))
      : livePoints.map((p) => ({ at: p.at, value: p.mem })),
  )

  const loadSeries = $derived(
    recorded ? (hist?.spans ?? []).map((s) => ({ at: Date.parse(s.at), value: s.load1 })) : [],
  )

  /** A recorded rate series: the summed figure, since history has no per-device
   *  columns. */
  const recordedRate = (pick: (s: History['spans'][number]) => number) =>
    (hist?.spans ?? []).map((s) => ({ at: Date.parse(s.at), value: pick(s) }))

  const liveNet = (iface: string, i: 0 | 1) =>
    livePoints.filter((p) => p.net[iface]).map((p) => ({ at: p.at, value: p.net[iface][i] }))

  const liveDisk = (device: string, i: 0 | 1) =>
    livePoints.filter((p) => p.disk[device]).map((p) => ({ at: p.at, value: p.disk[device][i] }))

  // ─── recording card ─────────────────────────────────────────────────────────

  const historyOn = $derived($settings.metrics_history !== false)

  function toggleHistory(on: boolean) {
    settings.update((s) => ({ ...s, metrics_history: on }))
  }

  let deleting = $state(false)
  async function onDeleteHistory() {
    deleting = true
    try {
      await deleteHistory()
      hist = null
      reloadKey++
    } catch (e) {
      histError = e instanceof Error ? e.message : String(e)
    } finally {
      deleting = false
    }
  }

  // ─── benchmarks ─────────────────────────────────────────────────────────────

  async function loadBench() {
    try {
      bench = await fetchBench()
    } catch {
      /* the cards show idle */
    }
  }

  async function runDisk() {
    benchError = ''
    try {
      bench = { ...bench!, disk: await startDiskBench() }
      void loadBench()
    } catch (e) {
      benchError = e instanceof Error ? e.message : String(e)
    }
  }

  async function runNetwork() {
    benchError = ''
    try {
      bench = { ...bench!, network: await startNetworkBench() }
      void loadBench()
    } catch (e) {
      benchError = e instanceof Error ? e.message : String(e)
    }
  }

  const appTotals = $derived({
    cpu: ($appStats?.apps ?? []).reduce((n, r) => n + r.cpu_percent, 0),
    mem: ($appStats?.apps ?? []).reduce((n, r) => n + r.mem_usage, 0),
  })
</script>

<div class="page">
  <header class="top">
    <div>
      <h3>{$t('resources')}</h3>
      <p class="hint">{$t('resources_hint')}</p>
    </div>
    <div class="ranges">
      {#each RANGES as r (r.key)}
        <button class="range" class:active={range === r.key} onclick={() => (range = r.key)}>
          {$t(`resources_range_${r.key}`)}
        </button>
      {/each}
    </div>
  </header>

  <nav class="tabs">
    {#each ['cpu', 'network', 'disk', 'apps'] as const as key (key)}
      <button class="tab" class:active={tab === key} onclick={() => (tab = key)}>
        {$t(`resources_tab_${key}`)}
      </button>
    {/each}
  </nav>

  {#if histError}
    <p class="error">{histError}</p>
  {/if}

  {#if !d}
    <p class="hint pad">{$t('loading')}</p>
  {:else if tab === 'cpu'}
    <div class="cards">
      <div class="card">
        <span class="k">CPU</span>
        <span class="v">{renderPercent(d.cpu_percent)}</span>
        <span class="sub">
          {$t('resources_cores', { count: String(d.cpu_count) })}
          {#if d.cpu_temp_c > 0}· {Math.round(d.cpu_temp_c)}°C{/if}
          · {$t('resources_uptime')} {renderUptime(d.uptime)}
        </span>
        <Sparkline points={cpuSeries} max={100} {gapMs} label="CPU" />
      </div>

      <div class="card">
        <span class="k">{$t('resources_load')}</span>
        <span class="v">{d.load1.toFixed(2)}</span>
        <span class="sub">5m {d.load5.toFixed(2)} · 15m {d.load15.toFixed(2)}</span>
        {#if recorded}
          <Sparkline points={loadSeries} {gapMs} color="var(--orange)" label="load" />
        {/if}
      </div>

      <div class="card">
        <span class="k">{$t('resources_memory')}</span>
        <span class="v">{renderSize(d.mem.used_bytes)} / {renderSize(d.mem.total_bytes)}</span>
        <span class="sub">
          {#if d.mem.swap_total_bytes > 0}
            {$t('resources_swap')}
            {renderSize(d.mem.swap_used_bytes)} / {renderSize(d.mem.swap_total_bytes)}
          {:else}
            {$t('resources_no_swap')}
          {/if}
        </span>
        <Sparkline points={memSeries} max={100} {gapMs} color="var(--turquoise)" label="RAM" />
      </div>
    </div>

    <section class="block">
      <h4>{$t('resources_top_processes')}</h4>
      {#if !d.host_proc}
        <p class="hint">{$t('resources_host_proc_missing')}</p>
      {:else if !d.top_processes.length}
        <p class="hint">{$t('resources_none')}</p>
      {:else}
        <table>
          <thead>
            <tr>
              <th class="num">PID</th>
              <th>{$t('resources_command')}</th>
              <th>{$t('resources_user')}</th>
              <th class="num">CPU</th>
              <th class="num">RAM</th>
            </tr>
          </thead>
          <tbody>
            {#each d.top_processes as p (p.pid)}
              <tr>
                <td class="num mono">{p.pid}</td>
                <td class="mono">{p.command}</td>
                <td>{p.user}</td>
                <td class="num">{p.cpu_percent.toFixed(1)}%</td>
                <td class="num">{renderSize(p.mem_bytes)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </section>
  {:else if tab === 'network'}
    {#if !d.host_proc}
      <p class="hint pad">{$t('resources_host_proc_missing')}</p>
    {:else if recorded}
      <div class="cards">
        <div class="card">
          <span class="k">{$t('resources_download')}</span>
          <span class="v">{renderThroughput(hist?.spans.at(-1)?.net_rx)}</span>
          <span class="sub">{$t('resources_all_interfaces')}</span>
          <Sparkline points={recordedRate((s) => s.net_rx)} {gapMs} label="rx" />
        </div>
        <div class="card">
          <span class="k">{$t('resources_upload')}</span>
          <span class="v">{renderThroughput(hist?.spans.at(-1)?.net_tx)}</span>
          <span class="sub">{$t('resources_all_interfaces')}</span>
          <Sparkline
            points={recordedRate((s) => s.net_tx)}
            {gapMs}
            color="var(--green)"
            label="tx"
          />
        </div>
      </div>
      <p class="hint pad">{$t('resources_summed_note')}</p>
    {:else if !d.nets.length}
      <p class="hint pad">{$t('resources_none')}</p>
    {:else}
      {#each d.nets as n (n.iface)}
        <section class="block">
          <h4>{n.iface}</h4>
          <div class="cards">
            <div class="card">
              <span class="k">{$t('resources_download')}</span>
              <span class="v">{renderThroughput(n.rx_bps)}</span>
              <span class="sub">{$t('resources_total')} {renderSize(n.rx_bytes)}</span>
              <Sparkline points={liveNet(n.iface, 0)} {gapMs} label="rx" />
            </div>
            <div class="card">
              <span class="k">{$t('resources_upload')}</span>
              <span class="v">{renderThroughput(n.tx_bps)}</span>
              <span class="sub">{$t('resources_total')} {renderSize(n.tx_bytes)}</span>
              <Sparkline points={liveNet(n.iface, 1)} {gapMs} color="var(--green)" label="tx" />
            </div>
          </div>
        </section>
      {/each}
    {/if}

    <section class="block">
      <h4>{$t('resources_speed_test')}</h4>
      <p class="hint">{$t('resources_speed_test_hint')}</p>
      <div class="run">
        <button
          class="btn"
          disabled={bench?.network.status === 'running'}
          onclick={runNetwork}
        >
          {bench?.network.status === 'running'
            ? $t('resources_running')
            : $t('resources_speed_test_run')}
        </button>
        {#if bench?.network.result}
          {@const r = bench.network.result}
          <div class="result">
            <span>↓ {renderMbps(r.download_bps)}</span>
            <span>↑ {renderMbps(r.upload_bps)}</span>
            <span class="sub">
              {renderSize(r.size_bytes)} · {renderDuration(r.download_seconds + r.upload_seconds)}
            </span>
          </div>
        {/if}
      </div>
      {#if bench?.network.error}
        <p class="error">{bench.network.error}</p>
      {/if}
    </section>
  {:else if tab === 'disk'}
    {#if recorded}
      <div class="cards">
        <div class="card">
          <span class="k">{$t('resources_read')}</span>
          <span class="v">{renderThroughput(hist?.spans.at(-1)?.disk_read)}</span>
          <span class="sub">{$t('resources_all_devices')}</span>
          <Sparkline points={recordedRate((s) => s.disk_read)} {gapMs} label="read" />
        </div>
        <div class="card">
          <span class="k">{$t('resources_write')}</span>
          <span class="v">{renderThroughput(hist?.spans.at(-1)?.disk_write)}</span>
          <span class="sub">{$t('resources_all_devices')}</span>
          <Sparkline
            points={recordedRate((s) => s.disk_write)}
            {gapMs}
            color="var(--green)"
            label="write"
          />
        </div>
      </div>
      <p class="hint pad">{$t('resources_summed_note')}</p>
    {:else}
      {#each d.disks as x (x.device)}
        <section class="block">
          <h4>{x.device}</h4>
          <div class="cards">
            <div class="card">
              <span class="k">{$t('resources_read')}</span>
              <span class="v">{renderThroughput(x.read_bps)}</span>
              <span class="sub">{$t('resources_total')} {renderSize(x.read_bytes)}</span>
              <Sparkline points={liveDisk(x.device, 0)} {gapMs} label="read" />
            </div>
            <div class="card">
              <span class="k">{$t('resources_write')}</span>
              <span class="v">{renderThroughput(x.write_bps)}</span>
              <span class="sub">{$t('resources_total')} {renderSize(x.write_bytes)}</span>
              <Sparkline
                points={liveDisk(x.device, 1)}
                {gapMs}
                color="var(--green)"
                label="write"
              />
            </div>
          </div>
        </section>
      {/each}
    {/if}

    <section class="block">
      <h4>{$t('resources_filesystems')}</h4>
      {#each d.filesystems.mounts as fs (fs.device + fs.mountpoint)}
        <div class="fs">
          <div class="fsline">
            <span class="mono">{fs.mountpoint}</span>
            <span class="sub">{fs.device} · {fs.fstype}</span>
            <span class="sub right">
              {renderSize(fs.used_bytes)} / {renderSize(fs.size_bytes)}
              ({renderPercent(fs.used_percent)})
            </span>
          </div>
          <div class="bar">
            <span
              class="fill"
              class:warn={fs.used_percent >= 90}
              style:width={`${Math.min(100, fs.used_percent)}%`}
            ></span>
          </div>
          {#if fs.local_path !== fs.mountpoint}
            <span class="sub">{$t('resources_measured_at', { path: fs.local_path })}</span>
          {/if}
        </div>
      {:else}
        <p class="hint">{$t('resources_none')}</p>
      {/each}
      {#if d.filesystems.unmeasured > 0}
        <p class="hint">
          {$t('resources_unmeasured', { count: String(d.filesystems.unmeasured) })}
        </p>
      {/if}
    </section>

    <section class="block">
      <h4>{$t('resources_disk_bench')}</h4>
      <p class="hint">{$t('resources_disk_bench_hint')}</p>
      <div class="run">
        <button class="btn" disabled={bench?.disk.status === 'running'} onclick={runDisk}>
          {bench?.disk.status === 'running' ? $t('resources_running') : $t('resources_disk_bench_run')}
        </button>
        {#if bench?.disk.result}
          {@const r = bench.disk.result}
          <div class="result">
            <span>{$t('resources_read')} {renderThroughput(r.read_bps)}</span>
            <span>{$t('resources_write')} {renderThroughput(r.write_bps)}</span>
            <span class="sub">
              {renderSize(r.size_bytes)}
              {#if !r.direct}· {$t('resources_bench_cached')}{/if}
            </span>
          </div>
        {/if}
      </div>
      {#if bench?.disk.error}
        <p class="error">{bench.disk.error}</p>
      {/if}
    </section>
  {:else}
    <div class="cards">
      <div class="card">
        <span class="k">{$t('resources_apps_cpu')}</span>
        <span class="v">{appTotals.cpu.toFixed(1)}%</span>
        <span class="sub">{$t('resources_cores', { count: String(d.cpu_count) })}</span>
      </div>
      <div class="card">
        <span class="k">{$t('resources_apps_mem')}</span>
        <span class="v">{renderSize(appTotals.mem)}</span>
        <span class="sub">{$t('resources_total')} {renderSize(d.mem.total_bytes)}</span>
      </div>
    </div>
    <section class="block">
      <AppUsageRows />
      <p class="hint">{$t('monitor_hint')}</p>
    </section>
  {/if}

  <section class="block recording">
    <h4>{$t('resources_recording')}</h4>
    <p class="hint">{$t('resources_recording_hint')}</p>
    <div class="run">
      <label class="toggle">
        <input
          type="checkbox"
          checked={historyOn}
          onchange={(e) => toggleHistory(e.currentTarget.checked)}
        />
        <span>{$t('resources_recording_on')}</span>
      </label>
      <span class="sub">
        {$t('resources_retention', {
          days: String(Math.round((hist?.retention_ms ?? 30 * 86_400_000) / 86_400_000)),
        })}
        · {renderSize(hist?.bytes ?? 0)}
      </span>
      <button class="btn subtle" disabled={deleting} onclick={onDeleteHistory}>
        {$t('resources_delete_history')}
      </button>
    </div>
  </section>

  {#if loadingHist}
    <p class="hint pad">{$t('loading')}</p>
  {/if}
  {#if benchError}
    <p class="error">{benchError}</p>
  {/if}
</div>

<style>
  .page {
    max-width: 52rem;
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  .top {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    flex-wrap: wrap;
  }
  h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: #29343d;
  }
  h4 {
    margin: 0;
    font-size: 0.8rem;
    font-weight: 600;
    color: var(--grey-600);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .hint {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
  .pad {
    padding: 0.5rem 0;
  }
  .error {
    margin: 0;
    font-size: 0.8rem;
    color: var(--red, #c0392b);
  }
  .ranges,
  .tabs {
    display: flex;
    gap: 0.25rem;
    flex-wrap: wrap;
  }
  .range,
  .tab {
    background: none;
    border: 1px solid transparent;
    border-radius: 6px;
    padding: 0.25rem 0.6rem;
    font-size: 0.78rem;
    color: var(--grey-600);
    cursor: pointer;
  }
  .range:hover,
  .tab:hover {
    background: hsla(208, 16%, 94%, 1);
  }
  .range.active {
    background: hsla(208, 16%, 94%, 1);
    color: #29343d;
    font-weight: 600;
  }
  .tabs {
    border-bottom: 1px solid hsla(208, 16%, 90%, 1);
    padding-bottom: 0.4rem;
  }
  .tab.active {
    color: var(--primary);
    border-color: hsla(208, 16%, 90%, 1);
    font-weight: 600;
  }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
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
  .right {
    margin-left: auto;
  }
  .block {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    padding: 1rem 1.15rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.8rem;
  }
  th {
    text-align: left;
    font-weight: 600;
    color: var(--grey-600);
    font-size: 0.72rem;
    padding-bottom: 0.3rem;
    border-bottom: 1px solid hsla(208, 16%, 92%, 1);
  }
  td {
    padding: 0.28rem 0;
    border-bottom: 1px solid hsla(208, 16%, 96%, 1);
  }
  .num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  .mono {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.76rem;
  }
  .fs {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .fsline {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
    flex-wrap: wrap;
    font-size: 0.8rem;
  }
  .bar {
    height: 6px;
    border-radius: 3px;
    background: hsla(208, 16%, 92%, 1);
    overflow: hidden;
  }
  .fill {
    display: block;
    height: 100%;
    background: var(--primary);
    border-radius: 3px;
  }
  .fill.warn {
    background: var(--orange);
  }
  .run {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .btn {
    border: 1px solid hsla(208, 16%, 85%, 1);
    background: #fff;
    border-radius: 6px;
    padding: 0.35rem 0.75rem;
    font-size: 0.8rem;
    color: #29343d;
    cursor: pointer;
  }
  .btn:hover:not(:disabled) {
    background: hsla(208, 16%, 96%, 1);
  }
  .btn:disabled {
    opacity: 0.55;
    cursor: default;
  }
  .btn.subtle {
    margin-left: auto;
    color: var(--grey-600);
  }
  .result {
    display: flex;
    align-items: baseline;
    gap: 0.75rem;
    font-size: 0.85rem;
    font-variant-numeric: tabular-nums;
  }
  .toggle {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.8rem;
    cursor: pointer;
  }
</style>
