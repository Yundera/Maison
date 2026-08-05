<script lang="ts">
  /**
   * Settings › Cloud backup — which engine, on what schedule, kept for how long.
   *
   * Named "cloud" rather than "backup" because the section next to it is already
   * `backups`, which lists archives. `/settings/backup` and `/settings/backups`
   * one keystroke apart would be a trap for anyone typing a URL or reading a bug
   * report.
   */
  import { t } from '../../i18n'
  import {
    fetchBackupStatus,
    saveBackupConfig,
    runBackupNow,
    emailBackupKey,
    type BackupStatus,
    type BackupConfig,
  } from '../../stores/backupengine'

  let status = $state<BackupStatus | null>(null)
  let conf = $state<BackupConfig | null>(null)
  let busy = $state(false)
  let error = $state('')
  let note = $state('')

  async function load() {
    try {
      status = await fetchBackupStatus()
      conf = { ...status.config }
    } catch (e) {
      error = (e as Error).message
    }
  }
  load()

  // While a run is in flight the page is the only place its progress shows, so it
  // polls — the app tiles carry their own bars, but the user-data target has no
  // tile to hang one off.
  $effect(() => {
    if (!status?.run.running) return
    const id = setInterval(load, 2000)
    return () => clearInterval(id)
  })

  async function apply(fn: () => Promise<unknown>, ok = '') {
    busy = true
    error = ''
    note = ''
    try {
      await fn()
      note = ok
      await load()
    } catch (e) {
      error = (e as Error).message
    } finally {
      busy = false
    }
  }

  const save = () => apply(() => saveBackupConfig(conf!), $t('saved'))
  const runNow = () => apply(runBackupNow, $t('backup_run_started'))
  const sendKey = () => apply(emailBackupKey, $t('backup_key_sent'))

  /** An engine the user picked but that has nothing to write to. Backups would fail
   *  rather than quietly land on the data disk, and saying so here is the whole
   *  point — believing your data is offsite when it is not is the worst outcome
   *  this page can produce. */
  const activeEngine = $derived(status?.engines?.find((e) => e.id === status?.active))
  const misconfigured = $derived(!!activeEngine && !activeEngine.connected)
</script>

<section class="card">
  <h3>{$t('cloud_backup')}</h3>
  <p class="hint">{$t('cloud_backup_hint')}</p>

  {#if conf && status}
    <label class="row">
      <span>{$t('backup_engine')}</span>
      <select bind:value={conf.engine} disabled={busy}>
        <option value="">{$t('backup_engine_default')}</option>
        {#each status.engines ?? [] as e}
          <option value={e.id}>{e.id}{e.offsite ? '' : ` — ${$t('backup_engine_not_offsite')}`}</option>
        {/each}
      </select>
    </label>

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
      <button onclick={sendKey} disabled={busy}>{$t('backup_key_send')}</button>
    </div>

    {#if error}<p class="err">{error}</p>{/if}
    {#if note}<p class="ok">{note}</p>{/if}
  {:else if error}
    <p class="err">{error}</p>
  {:else}
    <p class="hint">{$t('loading')}</p>
  {/if}
</section>

<style>
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
  }
  h3 { margin: 0 0 0.25rem; }
  h4 { margin: 1.25rem 0 0.25rem; font-size: 0.95rem; }
  .hint { color: var(--text-dim); font-size: 0.85rem; margin: 0 0 0.75rem; }
  .row { display: flex; align-items: center; gap: 0.75rem; margin: 0.5rem 0; }
  .row > span:first-child { min-width: 9rem; }
  .check { display: flex; align-items: center; gap: 0.5rem; margin: 0.4rem 0; font-size: 0.9rem; }
  .time { display: inline-flex; align-items: center; gap: 0.25rem; }
  .time input { width: 4rem; }
  .tiers { display: grid; grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr)); gap: 0.5rem; }
  .tiers label { display: flex; flex-direction: column; gap: 0.2rem; font-size: 0.85rem; }
  input, select {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.35rem 0.5rem;
  }
  .actions { display: flex; gap: 0.5rem; margin-top: 0.75rem; flex-wrap: wrap; }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    cursor: pointer;
  }
  button:disabled { opacity: 0.5; cursor: default; }
  .err { color: var(--danger, #e5534b); font-size: 0.85rem; }
  .ok { color: var(--text-dim); font-size: 0.85rem; }
  .warn { color: var(--warning, #d29922); font-size: 0.85rem; }
</style>
