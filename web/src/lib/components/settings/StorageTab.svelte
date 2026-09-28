<script lang="ts">
  /**
   * Settings › Resources › Storage — where the disk went, and giving some back.
   *
   * The split is the one an operator asks about: their data (everything under the
   * data root, walked like `du`) against the system (what the rest of the same disk
   * holds — the OS and Docker). On a PCS the data root is a directory on the root
   * disk, which is what makes "used − data" mean "system"; when it is a disk of its
   * own the page says so and does not invent a system share.
   *
   * The cleanup below it is deliberately not `docker system prune --all` — see
   * internal/cleanup. The routine clean is one button because nothing it removes is
   * anyone's state; orphans are listed and ticked, because removing one is final.
   */
  import { onMount } from 'svelte'
  import {
    fetchStorage,
    startScan,
    fetchCleanup,
    runCleanup,
    removeOrphans,
    validTime,
    type StorageUsage,
    type CleanupState,
  } from '../../stores/storage'
  import { renderSize, renderPercent } from '../../format'
  import { t } from '../../i18n'

  /** A scan older than this is refreshed on its own when the tab opens. */
  const STALE_MS = 60 * 60 * 1000
  const APPS_SHOWN = 8

  let usage = $state<StorageUsage | null>(null)
  let clean = $state<CleanupState | null>(null)
  let error = $state('')
  let showAllApps = $state(false)
  let showImages = $state(false)
  let picked = $state<Record<string, boolean>>({})
  let confirming = $state(false)

  async function loadUsage() {
    try {
      usage = await fetchStorage()
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  async function loadCleanup() {
    try {
      clean = await fetchCleanup()
      // Drop ticks for orphans that are gone, so the count on the button is true.
      const keys = new Set(clean.plan.orphans.map((o) => o.key))
      picked = Object.fromEntries(Object.entries(picked).filter(([k, v]) => v && keys.has(k)))
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  onMount(() => {
    void (async () => {
      await Promise.all([loadUsage(), loadCleanup()])
      const done = validTime(usage?.data.finished_at)?.getTime() ?? 0
      if (usage && usage.data.status !== 'running' && Date.now() - done > STALE_MS) void rescan()
    })()

    // The scan, Docker's accounting and a cleanup all run on the server; poll only
    // while one of them is going.
    let wasCleaning = false
    let wasMeasuring = false
    const poll = setInterval(() => {
      const scanning = usage?.data.status === 'running'
      const measuring = !!usage?.docker_pending
      const cleaning = clean?.run.status === 'running'
      if (scanning || measuring || (wasCleaning && !cleaning)) void loadUsage()
      // The build cache's reclaimable size comes from Docker's accounting, so the
      // plan is re-read once that lands.
      if (cleaning || wasCleaning || (wasMeasuring && !measuring)) void loadCleanup()
      wasCleaning = cleaning
      wasMeasuring = measuring
    }, 2000)
    return () => clearInterval(poll)
  })

  async function rescan() {
    error = ''
    try {
      const data = await startScan()
      if (usage) usage = { ...usage, data }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  async function onClean() {
    error = ''
    try {
      const run = await runCleanup()
      if (clean) clean = { ...clean, run }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  async function onRemoveOrphans() {
    if (!confirming) {
      confirming = true
      return
    }
    confirming = false
    error = ''
    try {
      const run = await removeOrphans(pickedKeys)
      picked = {}
      if (clean) clean = { ...clean, run }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  // ─── the split ──────────────────────────────────────────────────────────────

  const scanned = $derived(!!validTime(usage?.data.finished_at))
  const dataBytes = $derived(usage?.data.bytes ?? 0)
  const systemBytes = $derived(usage ? Math.max(0, usage.used_bytes - dataBytes) : 0)
  // Docker lives outside the data root, so it is part of "system" — shown as its own
  // slice only when it fits inside it (it would not on a box whose data root is a
  // separate disk, or before the first scan).
  const dockerBytes = $derived(
    usage?.docker && !usage.data_own_filesystem && scanned && usage.docker.total_bytes <= systemBytes
      ? usage.docker.total_bytes
      : 0,
  )
  const osBytes = $derived(systemBytes - dockerBytes)

  const segments = $derived.by(() => {
    if (!usage || !usage.size_bytes) return []
    const pct = (n: number) => (n / usage!.size_bytes) * 100
    if (usage.data_own_filesystem || !scanned) {
      return [{ key: 'used', bytes: usage.used_bytes, pct: pct(usage.used_bytes), color: 'var(--primary)' }]
    }
    return [
      { key: 'data', bytes: dataBytes, pct: pct(dataBytes), color: 'var(--primary)' },
      { key: 'docker', bytes: dockerBytes, pct: pct(dockerBytes), color: 'var(--turquoise)' },
      { key: 'os', bytes: osBytes, pct: pct(osBytes), color: 'var(--grey-600)' },
    ].filter((s) => s.bytes > 0)
  })

  const topMax = $derived(Math.max(1, ...(usage?.data.top ?? []).map((e) => e.bytes)))
  const appsMax = $derived(Math.max(1, ...(usage?.data.apps ?? []).map((e) => e.bytes)))
  const apps = $derived(
    showAllApps ? (usage?.data.apps ?? []) : (usage?.data.apps ?? []).slice(0, APPS_SHOWN),
  )

  // ─── cleanup ────────────────────────────────────────────────────────────────

  const running = $derived(clean?.run.status === 'running')
  const reclaim = $derived((clean?.plan.images_bytes ?? 0) + (clean?.reclaimable_cache_bytes ?? 0))
  const nothingToClean = $derived(
    !!clean &&
      clean.plan.images.length === 0 &&
      clean.plan.networks.length === 0 &&
      clean.reclaimable_cache_bytes === 0,
  )
  const pickedKeys = $derived(Object.keys(picked).filter((k) => picked[k]))

  const when = (iso?: string) => validTime(iso)?.toLocaleString() ?? ''
</script>

{#if error}
  <p class="error">{error}</p>
{/if}

{#if !usage}
  <p class="hint pad">{$t('loading')}</p>
{:else}
  <section class="block">
    <h4>{$t('storage_disk')}</h4>
    <div class="cards">
      {#if usage.data_own_filesystem}
        <div class="card">
          <span class="k">{$t('storage_data', { path: usage.data_root })}</span>
          <span class="v">{scanned ? renderSize(dataBytes) : '—'}</span>
          <span class="sub">{$t('storage_own_fs')}</span>
        </div>
      {:else}
        <div class="card">
          <span class="k"><i class="dot" style:background="var(--primary)"></i>{$t('storage_data', { path: usage.data_root })}</span>
          <span class="v">{scanned ? renderSize(dataBytes) : '—'}</span>
          <span class="sub">{$t('storage_data_hint')}</span>
        </div>
        <div class="card">
          <span class="k"><i class="dot" style:background="var(--grey-600)"></i>{$t('storage_system')}</span>
          <span class="v">{scanned ? renderSize(systemBytes) : '—'}</span>
          <span class="sub">
            {#if dockerBytes > 0}
              {$t('storage_system_split', { docker: renderSize(dockerBytes), os: renderSize(osBytes) })}
            {:else}
              {$t('storage_system_hint')}
            {/if}
          </span>
        </div>
      {/if}
      <div class="card">
        <span class="k"><i class="dot free"></i>{$t('storage_free')}</span>
        <span class="v">{renderSize(usage.avail_bytes)}</span>
        <span class="sub">
          {$t('storage_of_total', { total: renderSize(usage.size_bytes) })}
          · {renderPercent(usage.size_bytes ? (usage.used_bytes / usage.size_bytes) * 100 : 0)}
          {$t('storage_used')}
        </span>
      </div>
    </div>

    <div class="stack" role="img" aria-label={$t('storage_disk')}>
      {#each segments as s (s.key)}
        <span style:width={`${s.pct}%`} style:background={s.color} title={`${$t(`storage_seg_${s.key}`)}: ${renderSize(s.bytes)}`}></span>
      {/each}
    </div>
    {#if dockerBytes > 0}
      <div class="legend">
        {#each segments as s (s.key)}
          <span><i class="dot" style:background={s.color}></i>{$t(`storage_seg_${s.key}`)} {renderSize(s.bytes)}</span>
        {/each}
      </div>
    {/if}

    <div class="run">
      <button class="btn" disabled={usage.data.status === 'running'} onclick={rescan}>
        {usage.data.status === 'running' ? $t('storage_scanning') : $t('storage_rescan')}
      </button>
      <span class="sub">
        {#if usage.data.status === 'running'}
          {$t('storage_scan_progress', { size: renderSize(usage.data.progress_bytes ?? 0) })}
        {:else if scanned}
          {$t('storage_scanned_at', { when: when(usage.data.finished_at), files: usage.data.files.toLocaleString() })}
        {:else}
          {$t('storage_never_scanned')}
        {/if}
      </span>
    </div>
    {#if usage.data.error}
      <p class="error">{usage.data.error}</p>
    {/if}
  </section>

  {#if usage.data.top.length}
    <section class="block">
      <h4>{$t('storage_whats_in', { path: usage.data_root })}</h4>
      {#each usage.data.top as e (e.name)}
        <div class="row">
          <span class="mono name">{e.name}</span>
          <span class="minibar"><span style:width={`${(e.bytes / topMax) * 100}%`}></span></span>
          <span class="num">{renderSize(e.bytes)}</span>
        </div>
      {/each}
      {#if usage.data.skipped?.length}
        <p class="hint">{$t('storage_skipped', { paths: usage.data.skipped.join(', ') })}</p>
      {/if}
      {#if usage.data.unreadable}
        <p class="hint">{$t('storage_unreadable', { count: String(usage.data.unreadable) })}</p>
      {/if}
    </section>
  {/if}

  {#if usage.data.apps.length}
    <section class="block">
      <h4>{$t('storage_by_app')}</h4>
      {#each apps as e (e.name)}
        <div class="row">
          <span class="mono name">{e.name}</span>
          <span class="minibar"><span style:width={`${(e.bytes / appsMax) * 100}%`}></span></span>
          <span class="num">{renderSize(e.bytes)}</span>
        </div>
      {/each}
      {#if usage.data.apps.length > APPS_SHOWN}
        <button class="link" onclick={() => (showAllApps = !showAllApps)}>
          {showAllApps ? $t('storage_show_less') : $t('storage_show_all', { count: String(usage.data.apps.length) })}
        </button>
      {/if}
    </section>
  {/if}

  <section class="block">
    <h4>{$t('storage_docker')}</h4>
    {#if usage.docker}
      <div class="kv">
        <span>{$t('storage_images', { count: String(usage.docker.images_count) })}</span>
        <span class="num">{renderSize(usage.docker.images_bytes)}</span>
        <span>{$t('storage_containers', { count: String(usage.docker.containers_count) })}</span>
        <span class="num">{renderSize(usage.docker.containers_bytes)}</span>
        <span>{$t('storage_volumes', { count: String(usage.docker.volumes_count) })}</span>
        <span class="num">{renderSize(usage.docker.volumes_bytes)}</span>
        <span>{$t('storage_build_cache')}</span>
        <span class="num">{renderSize(usage.docker.build_cache_bytes)}</span>
      </div>
    {:else if usage.docker_pending}
      <p class="hint">{$t('storage_docker_measuring')}</p>
    {/if}
    {#if usage.docker_error}
      <p class="error">{usage.docker_error}</p>
    {/if}
  </section>

  {#if clean}
    <section class="block">
      <h4>{$t('storage_cleanup')}</h4>
      <p class="hint">{$t('storage_cleanup_hint')}</p>
      <ul class="plan">
        <li>
          {$t('storage_plan_images', { count: String(clean.plan.images.length), size: renderSize(clean.plan.images_bytes) })}
          {#if clean.plan.images.length}
            <button class="link" onclick={() => (showImages = !showImages)}>
              {showImages ? $t('storage_hide') : $t('storage_details')}
            </button>
          {/if}
        </li>
        {#if showImages}
          <li class="details">
            {#each clean.plan.images as im (im.id)}
              <div class="row">
                <span class="mono name">{im.tags?.length ? im.tags.join(', ') : im.id.replace('sha256:', '').slice(0, 12)}</span>
                <span class="num">{renderSize(im.bytes)}</span>
              </div>
            {/each}
          </li>
        {/if}
        <li>{$t('storage_plan_cache', { size: renderSize(clean.reclaimable_cache_bytes) })}</li>
        <li>{$t('storage_plan_networks', { count: String(clean.plan.networks.length) })}</li>
        <li class="kept">{$t('storage_plan_kept', { count: String(clean.plan.images_kept) })}</li>
      </ul>
      <div class="run">
        <button class="btn primary" disabled={running || nothingToClean || !!clean.blocked} onclick={onClean}>
          {#if running && clean.run.kind === 'clean'}
            {$t('storage_cleaning')}
          {:else if nothingToClean}
            {$t('storage_nothing_to_clean')}
          {:else}
            {$t('storage_clean_run', { size: renderSize(reclaim) })}
          {/if}
        </button>
        {#if clean.run.status === 'done' || clean.run.status === 'error'}
          <span class="sub">
            {#if clean.run.kind === 'orphans'}
              {$t('storage_orphans_result', { count: String(clean.run.containers) })}
            {:else}
              {$t('storage_clean_result', {
                images: String(clean.run.images),
                networks: String(clean.run.networks),
              })}
            {/if}
            {#if clean.run.freed_bytes > 0}
              · {$t('storage_freed', { size: renderSize(clean.run.freed_bytes) })}
            {/if}
          </span>
        {/if}
      </div>
      {#if clean.blocked}
        <p class="hint warn">{clean.blocked}</p>
      {/if}
      {#if clean.run.errors?.length && clean.run.status !== 'running'}
        <details class="errors">
          <summary>{$t('storage_errors', { count: String(clean.run.errors.length) })}</summary>
          {#each clean.run.errors as e}
            <p class="mono">{e}</p>
          {/each}
        </details>
      {/if}
    </section>

    <section class="block">
      <h4>{$t('storage_orphans')}</h4>
      <p class="hint">{$t('storage_orphans_hint')}</p>
      {#if !clean.plan.orphans.length}
        <p class="hint">{$t('storage_no_orphans')}</p>
      {:else}
        {#each clean.plan.orphans as o (o.key)}
          <label class="orphan">
            <input type="checkbox" bind:checked={picked[o.key]} disabled={running} />
            <div class="body">
              <div class="line">
                <span class="mono strong">{o.project ?? o.containers[0]?.name}</span>
                {#if o.running}<span class="tag warn">{$t('storage_orphan_running')}</span>{/if}
              </div>
              <span class="sub">
                {o.reason === 'folder_gone'
                  ? $t('storage_orphan_folder_gone', { path: o.working_dir ?? '' })
                  : $t('storage_orphan_standalone')}
              </span>
              {#each o.containers as c (c.id)}
                <span class="sub mono">{c.name} · {c.state} · {c.image}</span>
              {/each}
            </div>
          </label>
        {/each}
        <div class="run">
          <button
            class="btn"
            class:danger={confirming}
            disabled={running || !pickedKeys.length || !!clean.blocked}
            onclick={onRemoveOrphans}
          >
            {#if running && clean.run.kind === 'orphans'}
              {$t('storage_removing')}
            {:else if confirming}
              {$t('storage_orphans_confirm', { count: String(pickedKeys.length) })}
            {:else}
              {$t('storage_orphans_remove', { count: String(pickedKeys.length) })}
            {/if}
          </button>
          {#if confirming}
            <button class="btn subtle" onclick={() => (confirming = false)}>{$t('cancel')}</button>
          {/if}
        </div>
      {/if}
    </section>
  {/if}
{/if}

<style>
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
  .hint.warn {
    color: var(--orange);
  }
  .pad {
    padding: 0.5rem 0;
  }
  .error {
    margin: 0;
    font-size: 0.8rem;
    color: var(--red, #c0392b);
  }
  .block {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    padding: 1rem 1.15rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
  }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
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
    display: flex;
    align-items: center;
    gap: 0.35rem;
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
  .dot {
    display: inline-block;
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    flex: none;
  }
  .dot.free {
    background: var(--grey-300);
    box-shadow: inset 0 0 0 1px var(--grey-400);
  }
  .stack {
    display: flex;
    height: 10px;
    border-radius: 5px;
    overflow: hidden;
    background: var(--grey-300);
  }
  .stack span {
    display: block;
    height: 100%;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 0.9rem;
    font-size: 0.72rem;
    color: var(--grey-600);
  }
  .legend span {
    display: flex;
    align-items: center;
    gap: 0.3rem;
  }
  .row {
    display: grid;
    grid-template-columns: minmax(8rem, 14rem) 1fr auto;
    align-items: center;
    gap: 0.75rem;
    font-size: 0.8rem;
  }
  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .minibar {
    height: 6px;
    border-radius: 3px;
    background: hsla(208, 16%, 94%, 1);
    overflow: hidden;
  }
  .minibar span {
    display: block;
    height: 100%;
    background: var(--primary);
    border-radius: 3px;
  }
  .num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  .mono {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.76rem;
  }
  .strong {
    font-weight: 600;
    color: #29343d;
  }
  .kv {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 0.3rem 1rem;
    font-size: 0.8rem;
    max-width: 24rem;
  }
  .plan {
    margin: 0;
    padding-left: 1.1rem;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    font-size: 0.8rem;
    color: #29343d;
  }
  .plan .kept {
    color: var(--grey-600);
  }
  .plan .details {
    list-style: none;
    margin-left: -1.1rem;
    padding: 0.4rem 0.6rem;
    border-radius: 6px;
    background: hsla(208, 16%, 97%, 1);
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  .plan .details .row {
    grid-template-columns: 1fr auto;
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
  .btn.primary {
    background: var(--primary);
    border-color: var(--primary);
    color: var(--text-on-accent, #fff);
  }
  .btn.primary:hover:not(:disabled) {
    filter: brightness(0.95);
    background: var(--primary);
  }
  .btn.danger {
    background: var(--red, #c0392b);
    border-color: var(--red, #c0392b);
    color: var(--text-on-accent, #fff);
  }
  .btn.subtle {
    color: var(--grey-600);
  }
  .link {
    align-self: flex-start;
    background: none;
    border: none;
    padding: 0;
    margin-left: 0.4rem;
    font-size: 0.76rem;
    color: var(--primary);
    cursor: pointer;
  }
  .orphan {
    display: flex;
    gap: 0.6rem;
    align-items: flex-start;
    padding: 0.55rem 0.1rem;
    border-bottom: 1px solid hsla(208, 16%, 94%, 1);
    cursor: pointer;
  }
  .orphan input {
    margin-top: 0.2rem;
  }
  .orphan .body {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    min-width: 0;
  }
  .orphan .line {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .tag {
    font-size: 0.68rem;
    padding: 0.05rem 0.4rem;
    border-radius: 4px;
    background: hsla(208, 16%, 94%, 1);
  }
  .tag.warn {
    background: hsla(14, 100%, 53%, 0.12);
    color: var(--orange);
  }
  .errors summary {
    cursor: pointer;
    font-size: 0.76rem;
    color: var(--red, #c0392b);
  }
  .errors p {
    margin: 0.2rem 0;
  }
  @media (max-width: 600px) {
    .row {
      grid-template-columns: minmax(6rem, 1fr) 4rem auto;
    }
  }
</style>
