<script lang="ts">
  /**
   * Settings › Updates — every app that needs something, in one list, with one
   * button that updates the lot (docs/lifecycle.md § Update all).
   *
   * Grouped by what the operator can do about each app: update it, look at why it
   * failed, link it to a store, or nothing at all. Up-to-date apps collapse into a
   * count at the bottom — they are the reassurance, not the content.
   *
   * The run itself is the server's. This page starts it and follows it on the
   * "updates" channel, so closing the tab does not stop it, and a second tab sees the
   * same progress.
   */
  import { t } from '../../i18n'
  import { settingsApp } from '../../stores/ui'
  import { setUpdateRef, apps, appProgress, subscribeApps } from '../../stores/apps'
  import { renderDuration, renderRate, renderSize } from '../../format'
  import { incidents, loadIncidents, subscribeIncidents, type Incident } from '../../stores/incidents'
  import {
    updates,
    loadUpdates,
    subscribeUpdates,
    checkUpdates,
    preflightUpdates,
    runUpdates,
    type AppUpdate,
    type Preflight,
    type RunItem,
  } from '../../stores/updates'

  loadUpdates()
  loadIncidents()
  $effect(() => subscribeUpdates())
  $effect(() => subscribeIncidents())
  // The live step of an app being updated rides on the app list, which this page
  // does not otherwise follow.
  $effect(() => subscribeApps())

  let busy = $state(false)
  let error = $state('')
  /** The confirmation open on the page: update all, one system app, or a link. */
  let confirm = $state<
    | { kind: 'run'; ids: string[]; system: boolean; noBackup: boolean; pre: Preflight | null }
    | { kind: 'link'; row: AppUpdate }
    | null
  >(null)
  let showCurrent = $state(false)

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  const rows = $derived($updates?.apps ?? [])
  const run = $derived($updates?.run)
  const runItems = $derived(new Map<string, RunItem>((run?.items ?? []).map((i) => [i.id, i])))
  const running = $derived(!!run?.running)
  /** Live progress of an app being updated — the same overlay its tile draws, so the
   *  row and the tile always agree on the step and the percentage. */
  const liveProgress = $derived(
    new Map($apps.filter((a) => a.updating).map((a) => [a.id, { p: appProgress(a), msg: a.update_message }])),
  )
  function progressDetail(p: ReturnType<typeof appProgress>): string {
    if (!p) return ''
    const bits: string[] = []
    if (p.total) bits.push(`${renderSize(p.done ?? 0)} / ${renderSize(p.total)}`)
    const rate = renderRate(p.rate)
    if (rate) bits.push(rate)
    const eta = renderDuration(p.eta)
    if (eta) bits.push($t('backup_time_left', { time: eta }))
    return bits.join(' · ')
  }
  const checking = $derived(!!$updates?.checking)

  /** Open app.update incidents, by app — a failed or rolled-back update that the
   *  check alone cannot see, since a rolled-back app still reads "available". */
  const updateIncidents = $derived(
    new Map<string, Incident>(
      $incidents.open
        .filter((i) => i.kind === 'app.update' && i.id.startsWith('app.update:'))
        .map((i) => [i.id.slice('app.update:'.length), i]),
    ),
  )

  const available = $derived(rows.filter((r) => r.state === 'available' && !r.protected))
  const system = $derived(rows.filter((r) => r.state === 'available' && r.protected))
  const attention = $derived(rows.filter((r) => r.state === 'error' || updateIncidents.has(r.id)))
  const untracked = $derived(rows.filter((r) => r.state === 'untracked'))
  const unmanaged = $derived(rows.filter((r) => r.state === 'unmanaged'))
  const current = $derived(rows.filter((r) => r.state === 'current' && !updateIncidents.has(r.id)))

  /** Go sends its zero time for "never"; anything before 2000 is that. */
  function when(iso?: string): string {
    if (!iso) return ''
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? '' : d.toLocaleString()
  }
  const checkedAt = $derived(when($updates?.checked_at))

  async function act(fn: () => Promise<unknown>) {
    busy = true
    error = ''
    try {
      await fn()
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  const recheck = () => act(checkUpdates)

  /** Open the run confirmation. The preflight measures app folders to say which
   *  rollback points will not fit, so it is asked once, here. */
  function askRun(ids: string[], system = false, noBackup = false) {
    confirm = { kind: 'run', ids, system, noBackup, pre: null }
    act(async () => {
      const pre = await preflightUpdates(ids)
      if (confirm?.kind === 'run') confirm = { ...confirm, pre }
    })
  }

  function startRun(ids: string[], noBackup = false) {
    act(async () => {
      await runUpdates(ids, noBackup)
      confirm = null
    })
  }

  function link(row: AppUpdate) {
    const ref = row.suggestion?.ref
    if (!ref) return
    act(async () => {
      await setUpdateRef(row.id, ref)
      confirm = null
    })
  }

  const openUpdateTab = (row: AppUpdate) =>
    settingsApp.set({ id: row.id, name: row.name, managed: true, tab: 'update' })

  function version(row: AppUpdate): string {
    const imgs = row.images ?? []
    if (imgs.length === 0) return $t('updates_definition_changed')
    return imgs
      .map((c) =>
        !c.from ? `+ ${c.to}` : !c.to ? `− ${c.from}` : `${c.from} → ${tagOf(c.to, c.from)}`,
      )
      .join(' · ')
  }

  /** The new image's tag alone when only the tag moved — "10.9.7 → 10.10.1" reads
   *  better than the repository twice. */
  function tagOf(to: string, from: string): string {
    const repo = (s: string) => s.replace(/@.*$/, '').replace(/:[^/:]*$/, '')
    return repo(to) === repo(from) ? to.slice(repo(to).length + 1) || to : to
  }

  function status(item: RunItem | undefined): { text: string; cls: string } | null {
    if (!item) return null
    switch (item.status) {
      case 'queued':
        return { text: $t('updates_queued'), cls: 'muted' }
      case 'updating':
        return { text: $t('updates_updating'), cls: 'busy' }
      case 'done':
        return item.applied ? { text: $t('updates_done'), cls: 'ok' } : { text: $t('updates_already_current'), cls: 'muted' }
      case 'failed':
        return {
          text: item.rolled_back ? $t('updates_rolled_back') : $t('updates_failed'),
          cls: 'bad',
        }
    }
  }
</script>

{#snippet appHead(row: AppUpdate)}
  {#if row.icon}
    <img class="icon" src={row.icon} alt="" loading="lazy" />
  {:else}
    <span class="icon blank"></span>
  {/if}
{/snippet}

{#snippet runNote(row: AppUpdate)}
  {@const item = runItems.get(row.id)}
  {@const st = status(item)}
  {@const live = item?.status === 'updating' ? liveProgress.get(row.id) : undefined}
  {#if st}
    <span class="state {st.cls}">{st.text}</span>
  {/if}
  {#if live?.p}
    <div class="step">
      <span class="step-label">
        {$t(live.p.label)} {Math.round(live.p.pct)}%{#if live.msg} — {live.msg}{/if}
      </span>
      <div class="bar"><span class="fill" style:width={`${live.p.pct}%`}></span></div>
      {#if progressDetail(live.p)}<span class="step-detail">{progressDetail(live.p)}</span>{/if}
    </div>
  {/if}
  {#if item?.error}<p class="detail bad">{item.error}</p>{/if}
  {#if item?.warning}<p class="detail warn">{item.warning}</p>{/if}
  {#if item?.no_rollback && !running}
    <button disabled={busy} onclick={() => askRun([row.id], !!row.protected, true)}>
      {$t('updates_no_backup_retry')}
    </button>
  {/if}
{/snippet}

<header class="head">
  <h3>{$t('updates')}</h3>
  <p class="hint">{$t('updates_hint')}</p>
</header>

<section class="card">
  <div class="summary">
    <div class="body">
      <p class="title">
        {#if available.length > 0}
          {$t('updates_n_available', { n: String(available.length) })}
        {:else}
          {$t('updates_none_available')}
        {/if}
      </p>
      <p class="meta">
        {#if checking}
          {$t('updates_checking')}
        {:else if checkedAt}
          {$t('updates_checked_at', { when: checkedAt })}
        {:else}
          {$t('updates_never_checked')}
        {/if}
      </p>
    </div>
    <div class="row-actions">
      <button disabled={busy || checking || running} onclick={recheck}>{$t('updates_check_now')}</button>
      <button
        class="primary"
        disabled={busy || running || available.length === 0}
        onclick={() => askRun([])}
      >
        {running ? $t('updates_running') : $t('updates_update_all', { n: String(available.length) })}
      </button>
    </div>
  </div>

  {#if confirm?.kind === 'run'}
    <div class="confirm">
      {#if confirm.system}
        <p class="warn">{$t('updates_system_warning')}</p>
      {/if}
      {#if confirm.pre === null}
        <p class="meta">{$t('updates_preflight')}</p>
      {:else}
        <p>{$t('updates_confirm_run', { n: String(confirm.pre.apps.length) })}</p>
        {#if confirm.noBackup}
          <p class="warn">{$t('updates_no_backup_warning')}</p>
        {:else if confirm.pre.no_rollback?.length}
          <p class="warn">
            {$t('updates_no_rollback', { apps: confirm.pre.no_rollback.join(', ') })}
          </p>
        {/if}
        <p class="meta">{$t('updates_run_hint')}</p>
      {/if}
      <div class="actions">
        <button disabled={busy} onclick={() => (confirm = null)}>{$t('cancel')}</button>
        <button
          class="primary"
          disabled={busy || confirm.pre === null}
          onclick={() => confirm?.kind === 'run' && startRun(confirm.ids, confirm.noBackup)}
        >
          {confirm.noBackup ? $t('updates_no_backup_start') : $t('updates_start')}
        </button>
      </div>
    </div>
  {/if}
  {#if error}<p class="err">{error}</p>{/if}
</section>

{#if available.length > 0}
  <section class="card">
    <h4>{$t('updates_available')}</h4>
    <ul class="list">
      {#each available as row (row.id)}
        <li class="item">
          {@render appHead(row)}
          <div class="body">
            <p class="title">{row.name}</p>
            <p class="meta mono">{version(row)}</p>
            {@render runNote(row)}
          </div>
          <div class="row-actions">
            <button disabled={busy || running} onclick={() => askRun([row.id])}>{$t('updates_update')}</button>
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if system.length > 0}
  <section class="card">
    <h4>{$t('updates_system')}</h4>
    <p class="hint">{$t('updates_system_hint')}</p>
    <ul class="list">
      {#each system as row (row.id)}
        <li class="item">
          {@render appHead(row)}
          <div class="body">
            <p class="title">{row.name}</p>
            <p class="meta mono">{version(row)}</p>
            {@render runNote(row)}
          </div>
          <div class="row-actions">
            <button disabled={busy || running} onclick={() => askRun([row.id], true)}>{$t('updates_update')}</button>
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if attention.length > 0}
  <section class="card">
    <h4>{$t('updates_attention')}</h4>
    <ul class="list">
      {#each attention as row (row.id)}
        {@const inc = updateIncidents.get(row.id)}
        <li class="item">
          <span class="dot" class:critical={inc?.severity === 'critical'}></span>
          <div class="body">
            <p class="title">{row.name}</p>
            {#if row.state === 'error'}
              <p class="detail">{$t('updates_check_failed', { error: row.error ?? '' })}</p>
            {/if}
            {#if inc}
              <p class="detail">{inc.title}</p>
              {#if inc.detail}<p class="detail">{inc.detail}</p>{/if}
            {/if}
            {@render runNote(row)}
          </div>
          <div class="row-actions">
            {#if row.state === 'available' || row.state === 'error'}
              <button disabled={busy || running} onclick={() => askRun([row.id], !!row.protected)}>
                {$t('updates_retry')}
              </button>
            {/if}
            <button disabled={busy} onclick={() => openUpdateTab(row)}>{$t('updates_open_tab')}</button>
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if untracked.length > 0}
  <section class="card">
    <h4>{$t('updates_untracked')}</h4>
    <p class="hint">{$t('updates_untracked_hint')}</p>
    <ul class="list">
      {#each untracked as row (row.id)}
        <li class="item">
          {@render appHead(row)}
          <div class="body">
            <p class="title">{row.name}</p>
            {#if row.suggestion}
              <p class="meta">
                {$t('updates_looks_like', {
                  name: row.suggestion.name,
                  store: row.suggestion.store_name ?? '',
                })}
                {#if !row.suggestion.images_match}
                  · <span class="warn-inline">{$t('updates_name_only')}</span>
                {/if}
              </p>
            {:else}
              <p class="meta">{$t('updates_no_match')}</p>
            {/if}
            {#if confirm?.kind === 'link' && confirm.row.id === row.id && row.suggestion}
              <div class="confirm">
                <p class="meta mono">{row.suggestion.ref}</p>
                <p class="warn">{$t('updates_link_warning')}</p>
                <div class="actions">
                  <button disabled={busy} onclick={() => (confirm = null)}>{$t('cancel')}</button>
                  <button class="primary" disabled={busy} onclick={() => link(row)}>{$t('updates_link_confirm')}</button>
                </div>
              </div>
            {/if}
          </div>
          <div class="row-actions">
            {#if row.suggestion}
              <button disabled={busy} onclick={() => (confirm = { kind: 'link', row })}>{$t('updates_link')}</button>
            {/if}
            <button disabled={busy} onclick={() => openUpdateTab(row)}>{$t('updates_set_source')}</button>
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if unmanaged.length > 0}
  <section class="card">
    <h4>{$t('updates_unmanaged')}</h4>
    <p class="hint">{$t('updates_unmanaged_hint')}</p>
    <ul class="list">
      {#each unmanaged as row (row.id)}
        <li class="item">
          {@render appHead(row)}
          <div class="body"><p class="title">{row.name}</p></div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if current.length > 0}
  <section class="card">
    <button class="link" onclick={() => (showCurrent = !showCurrent)}>
      {$t('updates_n_current', { n: String(current.length) })}
    </button>
    {#if showCurrent}
      <ul class="list">
        {#each current as row (row.id)}
          <li class="item">
            {@render appHead(row)}
            <div class="body">
              <p class="title">{row.name}</p>
              {#if row.store_name}<p class="meta">{row.store_name}</p>{/if}
              {@render runNote(row)}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}

<style>
  .step {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    margin-top: 0.35rem;
    width: 100%;
  }
  .step-label,
  .step-detail {
    font-size: 0.8rem;
    color: var(--text-muted, var(--grey-600));
  }
  .bar {
    height: 6px;
    border-radius: 3px;
    background: var(--surface-2, hsla(0, 0%, 50%, 0.2));
    overflow: hidden;
  }
  .bar .fill {
    display: block;
    height: 100%;
    background: var(--progress-update);
    transition: width 0.3s ease;
  }
  .head {
    max-width: 46rem;
    margin-bottom: 1rem;
  }
  .card {
    max-width: 46rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
    margin-bottom: 1.25rem;
  }
  h3 {
    margin: 0 0 0.25rem;
    font-size: 1.05rem;
    font-weight: 600;
    color: var(--text);
  }
  h4 {
    margin: 0 0 0.25rem;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--text);
  }
  .hint {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .summary {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .item {
    display: flex;
    align-items: flex-start;
    gap: 0.7rem;
    padding: 0.7rem 0;
    border-top: 1px solid var(--border);
  }
  .item:first-child {
    border-top: 0;
  }
  .icon {
    width: 2rem;
    height: 2rem;
    border-radius: 6px;
    flex: none;
    object-fit: contain;
  }
  .icon.blank {
    background: var(--surface-2);
  }
  .body {
    flex: 1;
    min-width: 0;
  }
  .title {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text);
  }
  .meta {
    margin: 0.2rem 0 0;
    font-size: 0.8rem;
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }
  .mono {
    font-family: var(--font-mono, ui-monospace, monospace);
  }
  .detail {
    margin: 0.2rem 0 0;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
    white-space: pre-wrap;
  }
  .state {
    display: inline-block;
    margin-top: 0.3rem;
    font-size: 0.78rem;
    font-weight: 600;
  }
  .muted {
    color: var(--text-muted);
  }
  .busy {
    color: var(--primary);
  }
  .ok {
    color: var(--green, #2da44e);
  }
  .bad {
    color: var(--red);
  }
  .warn,
  .warn-inline {
    color: var(--warning, #d29922);
  }
  p.warn {
    margin: 0.4rem 0;
    font-size: 0.85rem;
  }
  .err {
    margin: 0.5rem 0 0;
    font-size: 0.85rem;
    color: var(--red);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    margin-top: 0.4rem;
    flex: none;
    background: var(--warning, #d29922);
  }
  .dot.critical {
    background: var(--red);
  }
  .confirm {
    margin-top: 0.75rem;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--surface-2);
    font-size: 0.88rem;
  }
  .confirm p {
    margin: 0 0 0.4rem;
  }
  .row-actions {
    display: flex;
    gap: 0.35rem;
    flex: none;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.6rem;
  }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  button.primary {
    background: var(--primary);
    border-color: var(--primary);
    color: #fff;
  }
  button:disabled {
    opacity: 0.55;
    cursor: default;
  }
  button.link {
    background: none;
    border: 0;
    padding: 0;
    color: var(--primary);
  }
  @media (max-width: 768px) {
    .summary,
    .item {
      flex-wrap: wrap;
    }
  }
</style>
