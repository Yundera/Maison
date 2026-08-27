<script lang="ts">
  // The breakdown behind the System status widget: what each app is costing the
  // box right now. Rows come from the "appstats" live channel, which the backend
  // only samples while this is mounted.
  //
  // The table itself lives in AppUsageRows, shared with the Resources page's Apps
  // tab. What is particular to this modal is the framing: the host totals it sits
  // under, and the fact that a row is a way into that app's own Stats tab.
  import { onMount } from 'svelte'
  import { appStats, subscribeAppStats } from '../stores/appstats'
  import { systemStats } from '../stores/system'
  import { apps } from '../stores/apps'
  import { monitorOpen, settingsApp } from '../stores/ui'
  import { openSettings } from '../route'
  import { renderSize } from '../format'
  import { t } from '../i18n'
  import AppUsageRows from './AppUsageRows.svelte'

  onMount(subscribeAppStats)

  const snap = $derived($appStats)
  const host = $derived($systemStats)

  const byId = $derived(new Map($apps.map((a) => [a.id, a])))

  // What the listed apps add up to. It is below the host figures, not equal to
  // them: the host runs things that are not compose apps, and a sample taken a
  // moment apart from the gauges will not agree to the decimal.
  const totalCPU = $derived((snap?.apps ?? []).reduce((n, r) => n + r.cpu_percent, 0))
  const totalMem = $derived((snap?.apps ?? []).reduce((n, r) => n + r.mem_usage, 0))

  function close() {
    monitorOpen.set(false)
  }

  // Each row is a way into that app's own Stats tab, which has the per-service
  // breakdown this list deliberately flattens.
  function openApp(id: string) {
    const a = byId.get(id)
    if (!a) return
    close()
    settingsApp.set({ id, name: a.name, managed: a.managed, tab: 'stats' })
  }

  // The history, the per-interface throughput and the filesystem table are one
  // click away rather than in here: this modal answers "what is busy right now",
  // and that is a different question from "what has this box been doing".
  function openResources() {
    close()
    openSettings('resources')
  }
</script>

<div class="backdrop" onclick={close} role="presentation">
  <div class="dialog" onclick={(e) => e.stopPropagation()} role="presentation">
    <header>
      <h2>{$t('monitor')}</h2>
      <button class="close" aria-label={$t('back')} onclick={close}>✕</button>
    </header>

    <div class="totals">
      <div class="total">
        <span class="k">CPU</span>
        <span class="v">{host ? `${host.cpu_percent}%` : '—'}</span>
        <span class="sub">
          {snap?.cpu_count ? $t('monitor_cores', { count: String(snap.cpu_count) }) : ''}
        </span>
      </div>
      <div class="total">
        <span class="k">RAM</span>
        <span class="v">{host ? `${host.mem_percent}%` : '—'}</span>
        <span class="sub">
          {host ? `${renderSize(host.mem_used)} / ${renderSize(host.mem_total)}` : ''}
        </span>
      </div>
      <div class="total">
        <span class="k">{$t('monitor_apps_total')}</span>
        <span class="v">{totalCPU.toFixed(1)}%</span>
        <span class="sub">{renderSize(totalMem)}</span>
      </div>
    </div>

    <AppUsageRows onopen={openApp} />

    <footer>
      <p class="foot">{$t('monitor_hint')}</p>
      <button class="link" onclick={openResources}>{$t('monitor_open_resources')}</button>
    </footer>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 110;
    background: var(--scrim);
    display: grid;
    place-items: center;
  }
  .dialog {
    width: min(94vw, 42rem);
    max-height: 84vh;
    display: flex;
    flex-direction: column;
    background: var(--surface);
    border-radius: 14px;
    padding: 1.25rem 1.4rem;
    color: var(--text);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 0.9rem;
  }
  h2 {
    margin: 0;
    font-size: 1.1rem;
  }
  .close {
    background: none;
    border: none;
    font-size: 1rem;
    color: var(--text-subtle);
    cursor: pointer;
  }
  .totals {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 0.75rem;
    margin-bottom: 1rem;
  }
  .total {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
    padding: 0.6rem 0.75rem;
    background: var(--surface-2);
    border-radius: 8px;
  }
  .total .k {
    font-size: 0.75rem;
    color: var(--text-muted);
  }
  .total .v {
    font-size: 1.15rem;
    font-weight: 600;
  }
  .total .sub {
    font-size: 0.72rem;
    color: var(--text-subtle);
    min-height: 1em;
  }
  footer {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 1rem;
    margin-top: 0.75rem;
  }
  .foot {
    margin: 0;
    font-size: 0.72rem;
    color: var(--text-subtle);
  }
  .link {
    flex: none;
    background: none;
    border: none;
    padding: 0;
    font-size: 0.75rem;
    color: var(--primary);
    cursor: pointer;
  }
  .link:hover {
    text-decoration: underline;
  }
</style>
