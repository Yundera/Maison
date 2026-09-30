<script lang="ts">
  /**
   * Settings › Updates — every app that needs something, in one list, with one
   * button that updates the lot (docs/lifecycle.md § Update all).
   *
   * One row per app, always. An app with an update available lives under *Updates
   * available* (or *System apps*) whatever happened to its last attempt — a refusal,
   * a rollback — and that row says, in plain words, what happened and what can be
   * done about it. *Needs attention* is only for apps with a problem and no pending
   * update. Up-to-date apps collapse into a count at the bottom — they are the
   * reassurance, not the content.
   *
   * Confirmations open in the row that asked for them and are worded for that
   * action. A plain update of one app asks nothing: it is backed up first and put
   * back if it fails.
   *
   * The run itself is the server's. This page starts it and follows it on the
   * "updates" channel, so closing the tab does not stop it, and a second tab sees the
   * same progress. The live step of the app being updated rides on the app list —
   * the same overlay its tile draws.
   */
  import { t } from '../../i18n'
  import { settingsApp } from '../../stores/ui'
  import { setUpdateRef, apps, appProgress, subscribeApps, type AppProgress } from '../../stores/apps'
  import { renderDuration, renderElapsed, renderRate, renderSize } from '../../format'
  import { incidents, loadIncidents, subscribeIncidents } from '../../stores/incidents'
  import {
    updates,
    loadUpdates,
    subscribeUpdates,
    checkUpdates,
    preflightUpdates,
    runUpdates,
    type AppUpdate,
    type ImageChange,
    type Preflight,
    type RunItem,
  } from '../../stores/updates'

  loadUpdates()
  loadIncidents()
  $effect(() => subscribeUpdates())
  $effect(() => subscribeIncidents())
  $effect(() => subscribeApps())

  let busy = $state(false)
  let error = $state('')
  /** The one confirmation open on the page, and the row it belongs to. */
  let confirm = $state<
    | { kind: 'all'; pre: Preflight | null }
    | { kind: 'nobackup' | 'system' | 'link'; id: string }
    | null
  >(null)
  let showCurrent = $state(false)

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))
  /** `$t` has no plural rules; the two forms are separate keys. */
  const tn = (key: string, n: number, params: Record<string, string> = {}) =>
    $t(`${key}_${n === 1 ? 'one' : 'other'}`, { n: String(n), ...params })

  const rows = $derived($updates?.apps ?? [])
  const byID = $derived(new Map(rows.map((r) => [r.id, r])))
  const run = $derived($updates?.run)
  const runItems = $derived(new Map<string, RunItem>((run?.items ?? []).map((i) => [i.id, i])))
  const running = $derived(!!run?.running)
  const checking = $derived(!!$updates?.checking)
  const live = $derived(
    new Map(
      $apps
        .filter((a) => a.updating)
        .map((a) => [a.id, { p: appProgress(a), msg: a.update_message }] as const),
    ),
  )

  // ── What happened to each app ──────────────────────────────────────────────

  /** Why an app's last update did not land, from the run (fresh) or from its open
   *  app.update incident (survives a reload and a restart). `reason` is the server's
   *  code; the row turns it into a sentence. */
  type Problem = { reason: string; critical: boolean; raw: string; needed?: number; free?: number }

  const updateIncidents = $derived(
    new Map(
      $incidents.open
        .filter((i) => i.kind === 'app.update' && i.id.startsWith('app.update:'))
        .map((i) => [i.id.slice('app.update:'.length), i]),
    ),
  )

  function problemFor(id: string): Problem | null {
    const item = runItems.get(id)
    if (item?.status === 'done') return null
    const inc = updateIncidents.get(id)
    if (item?.status === 'failed') {
      return {
        reason: item.reason ?? (item.rolled_back ? 'rolled_back' : 'failed'),
        critical: inc?.severity === 'critical',
        // The incident's detail is the engine's own account; the run item's error is
        // the one-line summary the row already says in words.
        raw: inc?.detail || item.error || '',
        needed: item.needed,
        free: item.free,
      }
    }
    if (!inc) return null
    const num = (v?: string) => (v ? Number(v) : undefined)
    return {
      reason: inc.args?.reason ?? (inc.severity === 'critical' ? 'broken' : 'failed'),
      critical: inc.severity === 'critical',
      raw: [inc.title, inc.detail].filter(Boolean).join('\n\n'),
      needed: num(inc.args?.needed),
      free: num(inc.args?.free),
    }
  }

  const REFUSED = new Set(['no_room', 'backup_timeout', 'backup_failed'])
  const BROKEN = new Set(['broken', 'rollback_failed', 'not_running'])

  type RowState = 'queued' | 'updating' | 'updated' | 'refused' | 'rolled_back' | 'broken' | 'failed' | 'idle'
  function rowState(id: string, p: Problem | null): RowState {
    const item = runItems.get(id)
    if (item?.status === 'queued') return 'queued'
    if (item?.status === 'updating') return 'updating'
    if (item?.status === 'done' && item.applied) return 'updated'
    if (!p) return 'idle'
    if (REFUSED.has(p.reason)) return 'refused'
    if (p.reason === 'rolled_back') return 'rolled_back'
    if (BROKEN.has(p.reason) || p.critical) return 'broken'
    return 'failed'
  }

  const CHIP: Record<RowState, { key: string; cls: string } | null> = {
    queued: { key: 'updates_queued', cls: 'muted' },
    updating: { key: 'updates_updating', cls: 'busy' },
    updated: { key: 'updates_done', cls: 'ok' },
    refused: { key: 'updates_chip_refused', cls: 'warn' },
    rolled_back: { key: 'updates_chip_rolled_back', cls: 'warn' },
    broken: { key: 'updates_chip_broken', cls: 'bad' },
    failed: { key: 'updates_failed', cls: 'bad' },
    idle: null,
  }

  function explain(p: Problem): string {
    switch (p.reason) {
      case 'no_room':
        return $t('updates_why_no_room', {
          needed: renderSize(p.needed ?? 0),
          free: renderSize(p.free ?? 0),
        })
      case 'backup_timeout':
        return $t('updates_why_timeout')
      case 'backup_failed':
        return $t('updates_why_backup_failed')
      case 'rolled_back':
        return $t('updates_why_rolled_back')
      case 'not_running':
        return $t('updates_why_not_running')
      case 'rollback_failed':
        return $t('updates_why_rollback_failed')
      case 'broken':
        return $t('updates_why_broken')
      default:
        return $t('updates_why_failed')
    }
  }

  // ── Grouping: every app once ───────────────────────────────────────────────

  const available = $derived(rows.filter((r) => r.state === 'available' && !r.protected))
  const system = $derived(rows.filter((r) => r.state === 'available' && r.protected))
  const attention = $derived(
    rows.filter((r) => r.state === 'error' || (r.state === 'current' && problemFor(r.id) !== null)),
  )
  const untracked = $derived(rows.filter((r) => r.state === 'untracked'))
  const unmanaged = $derived(rows.filter((r) => r.state === 'unmanaged'))
  const current = $derived(rows.filter((r) => r.state === 'current' && problemFor(r.id) === null))

  // ── Header and run banner ──────────────────────────────────────────────────

  /** Go sends its zero time for "never"; anything before 2000 is that. */
  function valid(iso?: string): string | undefined {
    if (!iso) return undefined
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? undefined : iso
  }
  /** "5 min ago" — refreshed every 30 s so it does not freeze on an open page. */
  let now = $state(Date.now())
  $effect(() => {
    const h = setInterval(() => (now = Date.now()), 30_000)
    return () => clearInterval(h)
  })
  function ago(iso?: string): string {
    void now
    const v = valid(iso)
    return v ? renderElapsed(v) || '0s' : ''
  }
  const checkedAt = $derived(valid($updates?.checked_at))

  const runTotal = $derived(run?.items?.length ?? 0)
  const runDone = $derived((run?.items ?? []).filter((i) => i.status === 'done' || i.status === 'failed').length)
  const runCurrent = $derived((run?.items ?? []).find((i) => i.status === 'updating'))
  const currentLive = $derived(runCurrent ? live.get(runCurrent.id) : undefined)
  const runPct = $derived(
    runTotal ? ((runDone + (currentLive?.p?.pct ?? 0) / 100) / runTotal) * 100 : 0,
  )
  const runResult = $derived.by(() => {
    const items = run?.items ?? []
    const updated = items.filter((i) => i.status === 'done' && i.applied).length
    const refused = items.filter((i) => i.status === 'failed' && i.no_rollback).length
    const failed = items.filter((i) => i.status === 'failed' && !i.no_rollback).length
    const bits: string[] = []
    if (updated) bits.push(tn('updates_result_updated', updated))
    if (refused) bits.push(tn('updates_result_refused', refused))
    if (failed) bits.push(tn('updates_result_failed', failed))
    return bits.join(' · ')
  })

  function progressDetail(p: AppProgress | null | undefined): string {
    if (!p) return ''
    const bits: string[] = []
    if (p.total) bits.push(`${renderSize(p.done ?? 0)} / ${renderSize(p.total)}`)
    const rate = renderRate(p.rate)
    if (rate) bits.push(rate)
    const eta = renderDuration(p.eta)
    if (eta) bits.push($t('backup_time_left', { time: eta }))
    return bits.join(' · ')
  }

  // ── Version line ───────────────────────────────────────────────────────────

  /** "ghcr.io/yundera/appshield:2.0.5" → { name: "appshield", tag: "2.0.5" }. */
  function img(ref: string): { name: string; tag: string } {
    const last = ref.replace(/@.*$/, '').split('/').pop() ?? ref
    const i = last.lastIndexOf(':')
    return i > 0 ? { name: last.slice(0, i), tag: last.slice(i + 1) } : { name: last, tag: '' }
  }
  function change(c: ImageChange): string {
    if (!c.from && c.to) return `+ ${img(c.to).name} ${img(c.to).tag}`.trim()
    if (c.from && !c.to) return `− ${img(c.from).name}`
    const a = img(c.from ?? '')
    const b = img(c.to ?? '')
    return a.name === b.name ? `${a.name} ${a.tag} → ${b.tag}` : `${a.name} ${a.tag} → ${b.name} ${b.tag}`
  }
  function version(row: AppUpdate): { text: string; more: number; all: string } {
    const imgs = row.images ?? []
    if (imgs.length === 0) return { text: $t('updates_definition_changed'), more: 0, all: '' }
    return {
      text: change(imgs[0]),
      more: imgs.length - 1,
      all: imgs.map((c) => `${c.from ?? '∅'} → ${c.to ?? '∅'}`).join('\n'),
    }
  }

  // ── Actions ────────────────────────────────────────────────────────────────

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

  /** One app, backed up and reversible: no confirmation. */
  const update = (id: string, noBackup = false) =>
    act(async () => {
      await runUpdates([id], noBackup)
      confirm = null
    })

  /** Update all asks the preflight first, which names the apps whose rollback point
   *  will not fit — they will be skipped, so the owner hears it before, not after. */
  function askAll() {
    confirm = { kind: 'all', pre: null }
    act(async () => {
      const pre = await preflightUpdates([])
      if (confirm?.kind === 'all') confirm = { kind: 'all', pre }
    })
  }
  const startAll = () =>
    act(async () => {
      await runUpdates([])
      confirm = null
    })

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

  const isOpen = (kind: string, id: string) =>
    confirm !== null && confirm.kind === kind && 'id' in confirm && confirm.id === id
</script>

{#snippet icon(row: AppUpdate)}
  {#if row.icon}
    <img class="icon" src={row.icon} alt="" loading="lazy" />
  {:else}
    <span class="icon blank"></span>
  {/if}
{/snippet}

{#snippet chip(state: RowState)}
  {@const c = CHIP[state]}
  {#if c}<span class="chip {c.cls}">{$t(c.key)}</span>{/if}
{/snippet}

{#snippet stepBar(p: AppProgress | null | undefined, msg?: string)}
  {#if p}
    <div class="step">
      <span class="step-label">
        {$t(p.label)} {Math.round(p.pct)}%{#if msg}<span class="muted">&nbsp;· {msg}</span>{/if}
      </span>
      <div class="bar"><span class="fill" style:width={`${p.pct}%`}></span></div>
      {#if progressDetail(p)}<span class="step-detail">{progressDetail(p)}</span>{/if}
    </div>
  {/if}
{/snippet}

{#snippet problemNote(row: AppUpdate, p: Problem)}
  <p class="explain" class:bad={p.critical || BROKEN.has(p.reason)}>{explain(p)}</p>
  <details class="more">
    <summary>{$t('updates_details')}</summary>
    {#if p.raw}<p class="raw">{p.raw}</p>{/if}
    <button class="link" onclick={() => openUpdateTab(row)}>{$t('updates_open_settings')}</button>
  </details>
{/snippet}

{#snippet updateRow(row: AppUpdate)}
  {@const p = problemFor(row.id)}
  {@const st = rowState(row.id, p)}
  {@const v = version(row)}
  {@const item = runItems.get(row.id)}
  {@const lv = st === 'updating' ? live.get(row.id) : undefined}
  {@const idle = !running && st !== 'queued' && st !== 'updating'}
  <li class="item">
    {@render icon(row)}
    <div class="body">
      <div class="line">
        <span class="title">{row.name}</span>
        {@render chip(st)}
      </div>
      <p class="meta mono" title={v.all}>
        {v.text}{#if v.more}<span class="more-count">{$t('updates_more', { n: String(v.more) })}</span>{/if}
      </p>
      {#if lv}{@render stepBar(lv.p, lv.msg)}{/if}
      {#if p && st !== 'updating' && st !== 'queued'}{@render problemNote(row, p)}{/if}
      {#if st === 'updated' && item?.warning}<p class="explain warn">{$t('updates_done_no_backup')}</p>{/if}

      {#if isOpen('nobackup', row.id)}
        <div class="confirm danger">
          <p>{$t('updates_no_backup_warning')}</p>
          <div class="actions">
            <button disabled={busy} onclick={() => (confirm = null)}>{$t('cancel')}</button>
            <button class="danger" disabled={busy} onclick={() => update(row.id, true)}>
              {$t('updates_no_backup_start')}
            </button>
          </div>
        </div>
      {:else if isOpen('system', row.id)}
        <div class="confirm">
          <p>{$t('updates_system_warning')}</p>
          <div class="actions">
            <button disabled={busy} onclick={() => (confirm = null)}>{$t('cancel')}</button>
            <button class="primary" disabled={busy} onclick={() => update(row.id)}>{$t('updates_update')}</button>
          </div>
        </div>
      {/if}
    </div>
    {#if idle && !(confirm && 'id' in confirm && confirm.id === row.id)}
      <div class="row-actions">
        {#if st === 'refused'}
          <button disabled={busy} onclick={() => (row.protected ? (confirm = { kind: 'system', id: row.id }) : update(row.id))}>
            {$t('updates_retry')}
          </button>
          <button class="warn-btn" disabled={busy} onclick={() => (confirm = { kind: 'nobackup', id: row.id })}>
            {$t('updates_no_backup_retry')}
          </button>
        {:else}
          <button
            class={p ? '' : 'primary'}
            disabled={busy}
            onclick={() => (row.protected ? (confirm = { kind: 'system', id: row.id }) : update(row.id))}
          >
            {p ? $t('updates_retry') : $t('updates_update')}
          </button>
        {/if}
      </div>
    {/if}
  </li>
{/snippet}

<!-- ── Header ─────────────────────────────────────────────────────────────── -->
<section class="card head">
  <div class="headline">
    <div class="body">
      <h3>{$t('updates')}</h3>
      <p class="status">
        {#if available.length + system.length > 0}
          <strong>{tn('updates_available_count', available.length + system.length)}</strong>
        {:else}
          <strong>{$t('updates_none_available')}</strong>
        {/if}
        <span class="muted">
          ·
          {#if checking}
            {$t('updates_checking')}
          {:else if checkedAt}
            <span title={new Date(checkedAt).toLocaleString()}>{$t('updates_checked_ago', { when: ago(checkedAt) })}</span>
          {:else}
            {$t('updates_never_checked')}
          {/if}
        </span>
        <button
          class="icon-btn"
          title={$t('updates_check_now')}
          aria-label={$t('updates_check_now')}
          disabled={busy || checking || running}
          onclick={recheck}
        >
          <span class:spin={checking}>↻</span>
        </button>
      </p>
      <p class="hint">{$t('updates_hint')}</p>
    </div>
    {#if available.length >= 2 && !running && confirm?.kind !== 'all'}
      <button class="primary" disabled={busy} onclick={askAll}>{$t('updates_update_all')}</button>
    {/if}
  </div>

  {#if confirm?.kind === 'all'}
    <div class="confirm">
      {#if confirm.pre === null}
        <p class="muted">{$t('updates_preflight')}</p>
      {:else}
        <p>{tn('updates_all_confirm', confirm.pre.apps.length)}</p>
        <p class="muted">{confirm.pre.apps.map((id) => byID.get(id)?.name ?? id).join(', ')}</p>
        {#if confirm.pre.no_rollback?.length}
          <p class="warn">
            {$t('updates_no_rollback', {
              apps: confirm.pre.no_rollback.map((id) => byID.get(id)?.name ?? id).join(', '),
            })}
          </p>
        {/if}
        <p class="muted">{$t('updates_run_hint')}</p>
      {/if}
      <div class="actions">
        <button disabled={busy} onclick={() => (confirm = null)}>{$t('cancel')}</button>
        <button class="primary" disabled={busy || confirm.pre === null} onclick={startAll}>
          {$t('updates_start')}
        </button>
      </div>
    </div>
  {/if}

  {#if running && runTotal > 0}
    <div class="banner">
      <p class="banner-line">
        <strong>{$t('updates_banner_running', { i: String(Math.min(runDone + 1, runTotal)), n: String(runTotal) })}</strong>
        {#if runCurrent}
          <span class="muted">
            — {byID.get(runCurrent.id)?.name ?? runCurrent.id}{#if currentLive?.p}
              · {$t(currentLive.p.label)} {Math.round(currentLive.p.pct)}%{/if}
          </span>
        {/if}
      </p>
      <div class="bar big"><span class="fill" style:width={`${runPct}%`}></span></div>
      <p class="muted small">{$t('updates_run_hint')}</p>
    </div>
  {:else if runTotal > 0 && runResult}
    <p class="result">
      {$t('updates_banner_result', { when: ago(run?.finished) })} <strong>{runResult}</strong>
    </p>
  {/if}
  {#if error}<p class="err">{error}</p>{/if}
</section>

<!-- ── Apps with an update ────────────────────────────────────────────────── -->
{#if available.length > 0}
  <section class="card">
    <h4>{$t('updates_available')}</h4>
    <ul class="list">
      {#each available as row (row.id)}
        {@render updateRow(row)}
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
        {@render updateRow(row)}
      {/each}
    </ul>
  </section>
{/if}

<!-- ── A problem and no pending update ────────────────────────────────────── -->
{#if attention.length > 0}
  <section class="card">
    <h4>{$t('updates_attention')}</h4>
    <ul class="list">
      {#each attention as row (row.id)}
        {@const p = problemFor(row.id)}
        <li class="item">
          {@render icon(row)}
          <div class="body">
            <div class="line">
              <span class="title">{row.name}</span>
              {#if p}{@render chip(rowState(row.id, p))}{/if}
            </div>
            {#if row.state === 'error'}
              <p class="explain">{$t('updates_check_failed')}</p>
              <details class="more">
                <summary>{$t('updates_details')}</summary>
                <p class="raw">{row.error ?? ''}</p>
              </details>
            {:else if p}
              {@render problemNote(row, p)}
            {/if}
          </div>
          <div class="row-actions">
            {#if row.state === 'error'}
              <button disabled={busy || checking || running} onclick={recheck}>{$t('updates_retry')}</button>
            {:else}
              <button disabled={busy} onclick={() => openUpdateTab(row)}>{$t('updates_open_settings')}</button>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<!-- ── Not linked / not managed / up to date ──────────────────────────────── -->
{#if untracked.length > 0}
  <section class="card">
    <h4>{$t('updates_untracked')}</h4>
    <p class="hint">{$t('updates_untracked_hint')}</p>
    <ul class="list">
      {#each untracked as row (row.id)}
        <li class="item">
          {@render icon(row)}
          <div class="body">
            <span class="title">{row.name}</span>
            {#if row.suggestion}
              <p class="meta">
                {$t('updates_looks_like', { name: row.suggestion.name, store: row.suggestion.store_name ?? '' })}
                {#if !row.suggestion.images_match}
                  · <span class="warn">{$t('updates_name_only')}</span>
                {/if}
              </p>
            {:else}
              <p class="meta">{$t('updates_no_match')}</p>
            {/if}
            {#if isOpen('link', row.id) && row.suggestion}
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
          {#if !isOpen('link', row.id)}
            <div class="row-actions">
              {#if row.suggestion}
                <button disabled={busy} onclick={() => (confirm = { kind: 'link', id: row.id })}>{$t('updates_link')}</button>
              {/if}
              <button disabled={busy} onclick={() => openUpdateTab(row)}>{$t('updates_set_source')}</button>
            </div>
          {/if}
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
          {@render icon(row)}
          <div class="body"><span class="title">{row.name}</span></div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

{#if current.length > 0}
  <section class="card">
    <button class="link" onclick={() => (showCurrent = !showCurrent)}>
      {showCurrent ? '▾' : '▸'} {tn('updates_current_count', current.length)}
    </button>
    {#if showCurrent}
      <ul class="list">
        {#each current as row (row.id)}
          <li class="item">
            {@render icon(row)}
            <div class="body">
              <div class="line">
                <span class="title">{row.name}</span>
                {@render chip(rowState(row.id, null))}
              </div>
              {#if row.store_name}<p class="meta">{row.store_name}</p>{/if}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}

<style>
  .card {
    max-width: 46rem;
    color: var(--text);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
    margin-bottom: 1.25rem;
  }
  .headline {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
  }
  h3 {
    margin: 0 0 0.3rem;
    font-size: 1.05rem;
    font-weight: 600;
    color: var(--text);
  }
  h4 {
    margin: 0 0 0.25rem;
    font-size: 0.8rem;
    font-weight: 600;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    color: var(--text-muted);
  }
  .status {
    margin: 0;
    font-size: 0.9rem;
    color: var(--text);
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.3rem;
  }
  .hint {
    margin: 0.35rem 0 0;
    font-size: 0.82rem;
    color: var(--text-muted);
    line-height: 1.45;
  }
  .card > .hint {
    margin: 0 0 0.5rem;
  }
  .muted {
    color: var(--text-muted);
  }
  .small {
    font-size: 0.78rem;
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .item {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    padding: 0.75rem 0;
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
  .line {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .title {
    font-size: 0.92rem;
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
  .more-count {
    margin-left: 0.4em;
    font-family: inherit;
    opacity: 0.8;
  }
  .chip {
    font-size: 0.72rem;
    font-weight: 600;
    padding: 0.1rem 0.45rem;
    border-radius: 999px;
    border: 1px solid currentColor;
    line-height: 1.4;
  }
  .chip.muted {
    color: var(--text-muted);
  }
  .chip.busy {
    color: var(--progress-update, var(--primary));
  }
  .chip.ok {
    color: var(--green, #2da44e);
  }
  .chip.warn,
  .warn {
    color: var(--warning, #d29922);
  }
  .chip.bad,
  .bad {
    color: var(--red);
  }
  .explain {
    margin: 0.35rem 0 0;
    font-size: 0.85rem;
    line-height: 1.45;
    color: var(--text);
  }
  .more {
    margin-top: 0.25rem;
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .more summary {
    cursor: pointer;
    width: fit-content;
  }
  .raw {
    margin: 0.35rem 0;
    padding: 0.5rem 0.6rem;
    border-radius: 6px;
    background: var(--surface-2);
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: 0.75rem;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .step {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    margin-top: 0.4rem;
  }
  .step-label,
  .step-detail {
    font-size: 0.8rem;
    color: var(--text);
  }
  .step-detail {
    color: var(--text-muted);
  }
  .bar {
    height: 6px;
    border-radius: 3px;
    background: var(--surface-2, hsla(0, 0%, 50%, 0.2));
    overflow: hidden;
  }
  .bar.big {
    height: 8px;
    margin: 0.45rem 0 0.35rem;
    background: var(--border);
  }
  .bar .fill {
    display: block;
    height: 100%;
    background: var(--progress-update);
    transition: width 0.3s ease;
  }
  .banner {
    margin-top: 0.9rem;
    padding: 0.75rem 0.85rem;
    border-radius: 8px;
    background: var(--surface-2);
  }
  .banner-line {
    margin: 0;
    color: var(--text);
    font-size: 0.88rem;
  }
  .result {
    margin: 0.75rem 0 0;
    font-size: 0.85rem;
    color: var(--text-muted);
  }
  .confirm {
    margin-top: 0.6rem;
    padding: 0.7rem 0.8rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--surface-2);
    font-size: 0.86rem;
  }
  .confirm.danger {
    border-color: var(--red);
  }
  .confirm p {
    margin: 0 0 0.4rem;
  }
  .row-actions {
    display: flex;
    gap: 0.4rem;
    flex: none;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.5rem;
  }
  .err {
    margin: 0.6rem 0 0;
    font-size: 0.85rem;
    color: var(--red);
  }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    font-size: 0.85rem;
    cursor: pointer;
    white-space: nowrap;
  }
  button.primary {
    background: var(--primary);
    border-color: var(--primary);
    color: #fff;
  }
  button.danger {
    background: var(--red);
    border-color: var(--red);
    color: #fff;
  }
  button.warn-btn {
    border-color: var(--warning, #d29922);
    color: var(--warning, #d29922);
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
  .icon-btn {
    background: none;
    border: 0;
    padding: 0 0.2rem;
    font-size: 1rem;
    line-height: 1;
    color: var(--text-muted);
  }
  .icon-btn:hover:not(:disabled) {
    color: var(--text);
  }
  .spin {
    display: inline-block;
    animation: spin 1s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  @media (max-width: 768px) {
    .headline,
    .item {
      flex-wrap: wrap;
    }
    .row-actions {
      width: 100%;
      justify-content: flex-start;
      padding-left: 2.75rem;
    }
  }
</style>
