<script lang="ts">
  /**
   * Settings › Backups — one page for the whole feature: where backups go, on what
   * schedule, and every backup that exists.
   *
   * It used to be two sections. `backups` listed archives and `cloud` configured the
   * engine, which put the schedule one click away from its own output and left the
   * two URLs (`/settings/backups`, `/settings/backup`) one keystroke apart. Worse, it
   * implied that a "cloud backup" is a different kind of thing. It is not: there is
   * one backup mechanism, and the engine — local disk or a remote repository — is a
   * setting it reads. Anything that triggers a backup calls the configured engine and
   * knows nothing else about it.
   *
   * So the two halves belong on one page, in the order the questions are asked:
   * where do backups go, then what do I have.
   *
   * The list is engine-agnostic (the server merges every engine, see backup.Set), so
   * a backup restores from wherever it actually is rather than from whichever engine
   * happens to be selected today.
   */
  import { t } from '../../i18n'
  import {
    fetchBackupStatus,
    saveBackupConfig,
    runBackupNow,
    emailBackupKey,
    showBackupKey,
    type BackupStatus,
    type BackupConfig,
  } from '../../stores/backupengine'
  import {
    fetchAllBackups,
    restoreBackup,
    deleteBackup,
    engineLabel,
    type EngineBackups,
    type Backup,
  } from '../../stores/backups'
  import { renderSize } from '../../format'
  import BackupRows from '../BackupRows.svelte'
  import UserDataCard from './UserDataCard.svelte'

  // --- where backups go ---------------------------------------------------------
  let status = $state<BackupStatus | null>(null)
  let conf = $state<BackupConfig | null>(null)
  let busy = $state(false)
  let error = $state('')
  let note = $state('')

  // --- what backups exist -------------------------------------------------------
  //
  // One entry per engine, shown as a tab each. Engines are independent repositories:
  // what is in one has no bearing on what is in another, they can be written to in
  // parallel, and the only thing the selected engine decides is where the NEXT backup
  // goes. A single merged list had to describe a backup as being "in two places at
  // once", which stopped being expressible the moment a second remote engine was
  // possible — a tab needs no such vocabulary.
  let engines = $state<EngineBackups[]>([])
  let tab = $state('')
  let free = $state<number | null>(null)
  let loading = $state(true)
  let listError = $state('')

  async function loadStatus() {
    try {
      status = await fetchBackupStatus()
      conf = { ...status.config }
    } catch (e) {
      error = (e as Error).message
    }
  }

  // Deliberately the expensive read — it measures folder archives, which is a tree
  // walk each — because it is what answers "what is eating the disk" and this page
  // is opened by hand.
  async function loadArchives() {
    loading = true
    try {
      const r = await fetchAllBackups()
      engines = r.engines ?? []
      free = r.free ?? null
      // Keep whichever tab is open across a reload — this reloads on a poll while a
      // restore runs, and a tab that jumped back to the default mid-restore would take
      // the progress off screen. Otherwise open the engine that receives new backups,
      // which is the one the user is most likely to be asking about.
      if (!engines.some((e) => e.engine === tab)) {
        tab = engines.some((e) => e.engine === status?.active)
          ? (status?.active ?? '')
          : (engines[0]?.engine ?? '')
      }
      listError = ''
    } catch (e) {
      listError = e instanceof Error ? e.message : String(e)
    } finally {
      loading = false
    }
  }

  loadStatus()
  loadArchives()

  // Deliberately a plain `let`, not $state: the effect below both reads and writes it,
  // and a reactive flag would re-trigger the effect on its own write.
  let wasRunning = false

  // While a run is in flight this page is the only place its progress shows — the app
  // tiles carry their own bars, but the user-data target has no tile to hang one off.
  // When the run finishes, the archive list is stale by definition, so reload it:
  // "Back up now" that leaves the list unchanged reads as a backup that did nothing.
  $effect(() => {
    const running = !!status?.run.running
    if (wasRunning && !running) loadArchives()
    wasRunning = running
    if (!running) return
    const id = setInterval(loadStatus, 2000)
    return () => clearInterval(id)
  })

  // A user-data restore has no tile to carry a progress bar, so this card is the only
  // place it shows, and polling the archive list is what refreshes it.
  //
  // Slower than the run poll above on purpose: this read asks every engine, which for a
  // remote one is a subprocess per call, and it measures folder archives on the way. The
  // message it is polling for changes once per restored folder, so a tighter loop would
  // spend a container start per tick to re-render the same sentence.
  $effect(() => {
    // Any engine's restore, not just the open tab's: there is one restore at a time on
    // the box, and switching tabs while it runs must not stop the polling that reports
    // it.
    if (!engines.some((e) => e.user_data.restore.running)) return
    const id = setInterval(loadArchives, 5000)
    return () => clearInterval(id)
  })

  async function apply(fn: () => Promise<unknown>, ok = '') {
    busy = true
    error = ''
    note = ''
    try {
      await fn()
      note = ok
      await loadStatus()
    } catch (e) {
      error = (e as Error).message
    } finally {
      busy = false
    }
  }

  const save = () => apply(() => saveBackupConfig(conf!), $t('saved'))
  const runNow = () => apply(runBackupNow, $t('backup_run_started'))
  const sendKey = () => apply(emailBackupKey, $t('backup_key_sent'))

  // --- the key itself -----------------------------------------------------------
  // Shown on demand rather than with the rest of the page: this is the one secret on
  // the box that has no recovery path, and it has no business being on screen behind
  // whoever walks past while its owner is reading about retention tiers.
  let key = $state('')
  let copied = $state(false)

  async function toggleKey() {
    if (key) {
      key = ''
      copied = false
      return
    }
    busy = true
    error = ''
    note = ''
    try {
      key = (await showBackupKey()).key
    } catch (e) {
      error = (e as Error).message
    } finally {
      busy = false
    }
  }

  // Copying is offered as well as selecting because the key is a long random string
  // and a half-selected one fails silently — the restore that needs it happens months
  // later, on a different machine, with no way to tell a wrong key from a lost one.
  async function copyKey() {
    try {
      await navigator.clipboard.writeText(key)
      copied = true
    } catch {
      // Not available on an insecure origin. Selecting the text still works, so this
      // is a missing convenience rather than a failure worth an error banner.
      copied = false
    }
  }

  /** When a copy of the key was last mailed, for the line under the buttons. */
  const keySentAt = $derived(
    status?.key_sent ? new Date(status.key_sent.sent_at).toLocaleString() : '',
  )

  /** An engine the user picked but that has nothing to write to. Backups would fail
   *  rather than quietly land on the data disk, and saying so here is the whole
   *  point — believing your data is offsite when it is not is the worst outcome this
   *  page can produce. */
  const activeEngine = $derived(status?.engines?.find((e) => e.id === status?.active))

  /** What "provisioned default" currently means on this box, for the picker's first
   *  option. Only shown when the user has no override — with one set, the default is
   *  not what is in force and naming it would be misleading. */
  const provisionedName = $derived(
    status?.chosen ? '' : engineLabel(status?.active ?? '', activeEngine?.name, (k) => $t(k)),
  )
  const misconfigured = $derived(!!activeEngine && !activeEngine.connected)


  // --- archive actions ----------------------------------------------------------
  async function run(fn: () => Promise<void>) {
    busy = true
    listError = ''
    try {
      await fn()
    } catch (e) {
      listError = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
    await loadArchives()
  }

  // The engine travels with the row: two engines can hold the same stamp, and each
  // copy is restored and deleted on its own.
  const restore = (app: string, b: Backup) => run(() => restoreBackup(app, b.name, b.engine))
  const remove = (app: string, b: Backup) => run(() => deleteBackup(app, b.name, b.engine ?? ''))

  /** Engine ID -> the name the deployment provisioned, for the row groups. This page
   *  already holds the engine status, so the names come free. */
  const engineNames = $derived(
    Object.fromEntries((status?.engines ?? []).filter((e) => e.name).map((e) => [e.id, e.name!])),
  )

  /** The open tab, falling back to the first engine so the page renders during the
   *  window between the engine list arriving and a tab being chosen. */
  const active = $derived(engines.find((e) => e.engine === tab) ?? engines[0] ?? null)
</script>

<header class="head">
  <h3>{$t('backups')}</h3>
  <p class="hint">{$t('backups_hint')}</p>
</header>

<section class="card">
  <h4>{$t('backup_destination')}</h4>
  <p class="hint">{$t('backup_destination_hint')}</p>

  {#if conf && status}
    <label class="row">
      <span>{$t('backup_engine')}</span>
      <select bind:value={conf.engine} disabled={busy}>
        <!-- "Provisioned default" is a choice about who decides, not a name, so it
             says what it currently resolves to — otherwise the one option a user is
             most likely to leave selected is the only one that never tells them
             where their backups are going. -->
        <option value="">
          {$t('backup_engine_default')}{provisionedName ? ` — ${provisionedName}` : ''}
        </option>
        {#each status.engines ?? [] as e}
          <option value={e.id}>
            {engineLabel(e.id, e.name, (k) => $t(k))}{e.offsite
              ? ''
              : ` — ${$t('backup_engine_not_offsite')}`}
          </option>
        {/each}
      </select>
    </label>
    <!-- Says what the setting actually governs. Engines run independently and every
         backup keeps the engine that wrote it, so this is a choice about where the
         NEXT backup goes — not about which backups exist or can be restored. -->
    <p class="hint">{$t('backup_engine_hint')}</p>

    {#if misconfigured}
      <p class="warn">{$t('backup_engine_unconfigured')} {activeEngine?.detail ?? ''}</p>
    {/if}

    <label class="row">
      <span>{$t('backup_schedule')}</span>
      <span class="time">
        <input type="number" min="0" max="23" bind:value={conf.hour} disabled={busy} />
        :
        <input type="number" min="0" max="59" bind:value={conf.minute} disabled={busy} />
      </span>
    </label>
    <label class="check">
      <input type="checkbox" bind:checked={conf.enabled} disabled={busy} />
      {$t('backup_schedule_enabled')}
    </label>
    <label class="check">
      <input type="checkbox" bind:checked={conf.user_data} disabled={busy} />
      {$t('backup_include_user_data')}
    </label>

    <h4>{$t('backup_retention')}</h4>
    <p class="hint">{$t('backup_retention_hint')}</p>
    <div class="tiers">
      <label><span>{$t('backup_keep_daily')}</span><input type="number" min="0" bind:value={conf.keep.daily} disabled={busy} /></label>
      <label><span>{$t('backup_keep_weekly')}</span><input type="number" min="0" bind:value={conf.keep.weekly} disabled={busy} /></label>
      <label><span>{$t('backup_keep_monthly')}</span><input type="number" min="0" bind:value={conf.keep.monthly} disabled={busy} /></label>
      <label><span>{$t('backup_keep_local')}</span><input type="number" min="0" bind:value={conf.keep_local} disabled={busy} /></label>
    </div>

    <div class="actions">
      <button onclick={save} disabled={busy}>{$t('save')}</button>
      <button onclick={runNow} disabled={busy || status.run.running}>{$t('backup_run_now')}</button>
    </div>

    {#if status.run.running}
      <p class="hint">{$t('backup_running')} {status.run.current ?? ''}</p>
    {:else if status.run.ran}
      <p class="hint">
        {status.run.failures > 0
          ? `${$t('backup_last_run_failed')} — ${status.run.last_error ?? ''}`
          : $t('backup_last_run_ok')}
      </p>
    {/if}

    <h4>{$t('backup_key')}</h4>
    <!-- Stated plainly because it has no recovery path: the box holds the key, the
         user holds a copy, and Yundera holds nothing and cannot help. -->
    <p class="hint">{$t('backup_key_hint')}</p>
    <div class="actions">
      <!-- Showing it first, mailing it second: reading the key off the screen keeps
           it on the box, where mailing it puts a plaintext secret in an inbox. Both
           are offered because the mail is the copy that survives losing the box. -->
      <button onclick={toggleKey} disabled={busy || status.has_key === false}>
        {key ? $t('backup_key_hide') : $t('backup_key_show')}
      </button>
      <button onclick={sendKey} disabled={busy}>{$t('backup_key_send')}</button>
    </div>

    {#if key}
      <div class="key">
        <code>{key}</code>
        <button onclick={copyKey}>{copied ? $t('copied') : $t('copy')}</button>
      </div>
    {/if}

    <!-- Whether a copy has ever left the box is the only fact that matters here, so
         it is stated either way rather than only when reassuring. -->
    <p class="hint key-state">
      {#if status.key_sent}
        {$t('backup_key_sent_on', { when: keySentAt, to: status.key_sent.to ?? '' })}
      {:else if status.has_key}
        {$t('backup_key_never_sent')}
      {/if}
    </p>

    {#if error}<p class="err">{error}</p>{/if}
    {#if note}<p class="ok">{note}</p>{/if}
  {:else if error}
    <p class="err">{error}</p>
  {:else}
    <p class="hint">{$t('loading')}</p>
  {/if}
</section>

<!-- One tab per engine. Each tab is that engine's repository and nothing else: its
     files, its app backups, its totals. Both cards used to sit above a single merged
     list, which meant "Your files" showed only the engine that happened to be selected
     — a box that wrote its files to a repository and then switched its default showed
     an empty card while the snapshots were still there.

     The strip sits outside the cards rather than wrapping them, so the two cards below
     stay the siblings they were and neither ends up nested inside another. -->
<div class="tabs" role="tablist">
  {#each engines as e (e.engine)}
    <button
      role="tab"
      class="tab"
      class:on={active?.engine === e.engine}
      aria-selected={active?.engine === e.engine}
      onclick={() => (tab = e.engine)}
    >
      {engineLabel(e.engine, e.name, (k) => $t(k))}
      <!-- The default engine is marked rather than reordered: which engine receives the
           next backup is the one thing the tabs cannot show by themselves, and moving it
           would make the strip reshuffle when the setting changes. -->
      {#if e.engine === status?.active}
        <span class="badge">{$t('backup_engine_active')}</span>
      {/if}
    </button>
  {/each}
</div>

{#if listError}
  <section class="card"><p class="err">{listError}</p></section>
{:else if loading && !engines.length}
  <section class="card"><p class="empty">{$t('loading')}</p></section>
{:else if active}
  <!-- Your files first: the backend's two sets are apps and user data, and the user
       data one is not an app — putting it inside the list below would file it under a
       heading that is a list of apps. -->
  <UserDataCard data={active.user_data} engine={active.engine} onchanged={loadArchives} />

  <!-- Every backup this engine holds, grouped by app. This list is what an app's own
       Backups tab cannot be: it reaches the backups of an app that is gone
       (uninstalling renames the folder, so the tile and its tab disappear with it — and
       "I uninstalled it and regret it" is the most common reason to want a restore),
       and it puts the cost on the same screen as the delete button. -->
  <section class="card">
    <h4>{$t('backups_stored')}</h4>

    <!-- Two different questions, so two different figures: what this engine holds, and
         what it costs on this disk. They are the same number for the local engine and
         zero-against-something for a remote one, and conflating them next to "free"
         would misdescribe the disk. -->
    <p class="totals">
      {$t('backups_total', { used: renderSize(active.total) })}
      · {$t('backups_local_used', { used: renderSize(active.used) })}
      {#if free !== null}
        · {$t('backups_free', { free: renderSize(free) })}
      {/if}
    </p>

    <!-- The same-disk warning belongs to the TAB's engine, not to the selected one. It
         used to be derived from whichever engine was writing, which said nothing about
         the list underneath it once a box had both: a repository's backups survive
         losing the disk whether or not it is the current default, and the local
         engine's never do. -->
    {#if !active.offsite}
      <p class="note">{$t('backups_scope_note')}</p>
    {/if}

    {#if !active.apps.length}
      <p class="empty">{$t('backups_empty')}</p>
    {:else}
      {#each active.apps as group (group.app)}
        <div class="group">
          <h5>
            {group.app}
            {#if group.orphan}
              <span class="tag" title={$t('backups_orphan_hint')}>{$t('backups_orphan')}</span>
            {/if}
            <span class="size">{renderSize(group.total)}</span>
          </h5>
          <!-- showEngine is off: this tab is already one engine, so naming it above
               every app would repeat the tab label down the page. -->
          <BackupRows
            backups={group.backups}
            showEngine={false}
            {busy}
            onrestore={(b) => restore(group.app, b)}
            ondelete={(b) => remove(group.app, b)}
          />
        </div>
      {/each}
    {/if}
  </section>
{/if}

<style>
  .tabs {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    margin-bottom: 0.9rem;
  }
  .tab {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    border: 0;
    background: none;
    padding: 0.45rem 0.7rem;
    margin-bottom: -1px;
    border-bottom: 2px solid transparent;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text-muted);
    cursor: pointer;
  }
  .tab:hover {
    color: var(--text);
  }
  .tab.on {
    color: var(--text);
    border-bottom-color: var(--accent, var(--text));
  }
  .badge {
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 0.05rem 0.4rem;
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
  }
  /* The tab strip separates the settings card from the tab's own cards, so there are
     no longer two adjacent .card siblings in this component to hang the gap on — and
     the card below the strip is rendered by UserDataCard, whose class this scope
     cannot reach anyway. The spacing lives on the strip instead. */
  .tabs {
    margin-top: 1.25rem;
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
  /* Every h4 after the first opens a group inside the card, so it needs air above
     it that the card's own padding already provides for the first. */
  h4 ~ h4 {
    margin-top: 1.25rem;
  }
  .hint,
  .empty,
  .note {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .note {
    margin-top: 1.5rem;
    margin-bottom: 0;
    padding-top: 0.9rem;
    border-top: 1px solid var(--border);
    font-size: 0.8rem;
  }
  .totals {
    margin: 0 0 1.2rem;
    font-size: 0.85rem;
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin: 0.5rem 0;
  }
  .row > span:first-child {
    min-width: 9rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.4rem 0;
    font-size: 0.9rem;
  }
  .time {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
  }
  .time input {
    width: 4rem;
  }
  .tiers {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
    gap: 0.5rem;
  }
  .tiers label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.85rem;
  }
  input,
  select {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.35rem 0.5rem;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.75rem;
    flex-wrap: wrap;
  }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .group {
    margin-bottom: 1.4rem;
  }
  h5 {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0 0 0.45rem;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text);
  }
  .size {
    margin-left: auto;
    font-size: 0.8rem;
    font-weight: 400;
    font-variant-numeric: tabular-nums;
    color: var(--text-muted);
  }
  .tag {
    padding: 0.1rem 0.4rem;
    border-radius: 4px;
    background: hsla(38, 92%, 90%, 1);
    color: hsl(30, 80%, 32%);
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  /* The key itself: monospaced and selectable in one gesture, because it is
     transcribed by hand often enough that a proportional font is a real hazard —
     l/1 and O/0 decide whether a restore works. */
  .key {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.75rem 0 0;
    padding: 0.5rem 0.6rem;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: 6px;
  }
  .key code {
    flex: 1;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.85rem;
    word-break: break-all;
    user-select: all;
    color: var(--text);
  }
  .key-state {
    margin-top: 0.75rem;
    margin-bottom: 0;
  }
  .err {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--red, #e5534b);
  }
  .ok {
    color: var(--text-muted);
    font-size: 0.85rem;
  }
  .warn {
    color: var(--warning, #d29922);
    font-size: 0.85rem;
  }
</style>
