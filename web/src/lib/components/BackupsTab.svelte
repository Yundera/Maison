<script lang="ts">
  // One app's Backups tab.
  //
  // Everything here is about the same folder the tile represents: what backups of it
  // exist, and making one more. The work itself is detached — the tile carries the
  // progress bar, exactly like an install or an uninstall — so this panel starts an
  // operation and then gets out of the way.
  //
  // The list spans every engine, grouped by the one holding each backup, and "Back up
  // now" writes to whichever engine is the default in Settings › Backups. Neither is a
  // choice this tab makes: there is one backup mechanism and the engine is a setting it
  // reads, so a remote backup shows up here beside a local one and restores the same
  // way.
  import {
    fetchBackups,
    estimateBackup,
    startBackup,
    restoreBackup,
    deleteBackup,
    type Backup,
    type Estimate,
  } from '../stores/backups'
  import { fetchBackupStatus, type EngineInfo } from '../stores/backupengine'
  import { engineLabel } from '../stores/backups'
  import { apps } from '../stores/apps'
  import { renderSize } from '../format'
  import { t } from '../i18n'
  import BackupRows from './BackupRows.svelte'

  let { id }: { id: string } = $props()

  let backups = $state<Backup[]>([])
  let estimate = $state<Estimate | null>(null)
  let zip = $state(false)
  let error = $state('')
  let busy = $state(false)

  // The tile is the source of truth for whether anything is running, so the panel
  // reads it rather than keeping its own flag: a backup started here and one
  // started from another browser tab both disable these buttons.
  const running = $derived($apps.find((a) => a.id === id)?.backing_up ?? false)

  /** Engine ID -> the name the deployment provisioned for it, for the row groups.
   *
   *  Fetched once when the tab is opened — it is mounted lazily, so this does not run
   *  on app load. It is a second request purely for labels, which is why its failure is
   *  swallowed: BackupRows falls back to naming the engine itself, and a status call
   *  that did not answer must not turn a working list of backups into an error. */
  let engineNames = $state<Record<string, string>>({})
  /** The engines this backup could be written to, and which one is the default. */
  let engineList = $state<EngineInfo[]>([])
  let defaultEngine = $state('')
  /** Where "Back up now" will write. Empty means the default — kept as the empty
   *  string rather than resolved to an ID so that a box whose default changes while
   *  this panel is open still follows it. */
  let target = $state('')

  /** Which engine's backups are being LOOKED at. Deliberately separate from `target`
   *  above: one is a view, the other is where the next backup goes, and tying them
   *  together would mean opening a tab quietly re-aimed the button. The picker names
   *  its destination in full for the same reason. */
  let tab = $state('')
  fetchBackupStatus()
    .then((s) => {
      engineNames = Object.fromEntries(
        (s.engines ?? []).filter((e) => e.name).map((e) => [e.id, e.name!]),
      )
      engineList = s.engines ?? []
      defaultEngine = s.active
      if (!engineList.some((e) => e.id === tab)) tab = s.active || engineList[0]?.id || ''
    })
    .catch(() => {})

  async function load() {
    try {
      backups = await fetchBackups(id)
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  // Re-measure whenever the artefact choice changes: a zip has to budget for the
  // snapshot and the zip at once, so the two answers genuinely differ.
  // Re-measured when the TARGET changes too, not only the artefact: the answer
  // depends on where the bytes are going. A remote engine streams and needs no local
  // room; the local one needs a full second copy, and it is the copy that can fill the
  // data disk. An estimate left over from the other engine would either refuse a
  // backup that fits or wave through one that does not.
  $effect(() => {
    const wantZip = zip
    const wantEngine = target
    estimateBackup(id, wantZip, wantEngine)
      .then((e) => (estimate = e))
      .catch(() => (estimate = null))
  })

  // Reload the list when a run finishes — `running` going false is the signal, and
  // it arrives on the live channel whether or not this tab started the work.
  $effect(() => {
    if (!running) load()
  })

  async function run(fn: () => Promise<void>) {
    busy = true
    error = ''
    try {
      await fn()
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
    await load()
  }

  /** This app's backups under the engine being viewed. The server sends one flat
   *  list carrying each row's engine, so the split is a filter rather than a second
   *  request — and an engine with nothing for this app still gets a tab, which is what
   *  makes "there is no copy of this app over there" visible instead of absent. */
  const shown = $derived(backups.filter((b) => (b.engine ?? '') === tab))

  const backup = () => run(() => startBackup(id, zip, target))
  // The engine travels with the row: two engines can hold the same stamp, and each
  // copy is restored and deleted on its own.
  const restore = (b: Backup) => run(() => restoreBackup(id, b.name, b.engine))
  const remove = (b: Backup) => run(() => deleteBackup(id, b.name, b.engine ?? ''))
</script>

<p class="hint">{$t('backup_hint')}</p>

<div class="make">
  <!-- Where this one backup goes. Offered only once there is a choice to make: on a
       box with a single engine the control would be a select with one option, which
       tells the user nothing and implies a decision that does not exist.

       This is a target for this run, NOT a preference the app keeps. The nightly run,
       an uninstall and the update rollback point all keep writing to the default
       engine — so "back up a copy locally before I try something" cannot quietly
       become "this app stopped going offsite". -->
  {#if engineList.length > 1}
    <label class="opt">
      <span>{$t('backup_target')}</span>
      <select bind:value={target} disabled={busy || running}>
        <option value="">
          {$t('backup_target_default', {
            engine: engineLabel(defaultEngine, engineNames[defaultEngine], (k) => $t(k)),
          })}
        </option>
        {#each engineList as e (e.id)}
          <option value={e.id}>{engineLabel(e.id, e.name, (k) => $t(k))}</option>
        {/each}
      </select>
    </label>
  {/if}
  <label class="opt">
    <input type="checkbox" bind:checked={zip} disabled={busy || running} />
    <span>{$t('backup_compress')}</span>
  </label>
  <button class="go" disabled={busy || running || estimate?.enough === false} onclick={backup}>
    {$t('backup_now')}
  </button>
</div>

{#if estimate}
  <p class="hint size" class:tight={!estimate.enough}>
    {#if estimate.free < 0}
      <!-- No usable reading: the guard is skipped rather than guessed at, so say
           so instead of showing a number that means nothing. -->
      {$t('backup_size', { size: renderSize(estimate.size) })}
    {:else if estimate.enough}
      {$t('backup_space', {
        size: renderSize(estimate.size),
        needed: renderSize(estimate.needed),
        free: renderSize(estimate.free),
      })}
    {:else}
      {$t('backup_space_short', {
        needed: renderSize(estimate.needed),
        free: renderSize(estimate.free),
      })}
    {/if}
  </p>
  <!-- What the app asked to leave out, said before the backup runs and again beside
       every restore below. A backup that does not contain something has to say so:
       an app that comes back without its cache is working as declared, and one that
       comes back missing something its author wrongly marked derived is a bug report
       — and only naming the paths tells a user which of the two they are looking at. -->
  {#if estimate.excluded?.length}
    <p class="hint fine">
      {$t('backup_excluded', {
        list: estimate.excluded.join(', '),
        size: renderSize(estimate.excludedSize ?? 0),
      })}
    </p>
  {/if}
  {#if estimate.excludeErrors?.length}
    <p class="warn fine">
      {$t('backup_excluded_bad', { list: estimate.excludeErrors.join('; ') })}
    </p>
  {/if}
{/if}

{#if error}
  <p class="err">{error}</p>
{/if}

{#if running}
  <p class="hint">{$t('backup_running')}</p>
{/if}

<!-- A tab per engine, the same shape as Settings › Backups. Engines are independent
     repositories, so "this app's backups" is really one list per engine — and seeing
     them as one merged list was what made a stamp held in two places look like a
     single backup that was somehow in both. -->
{#if engineList.length > 1}
  <div class="tabs" role="tablist">
    {#each engineList as e (e.id)}
      <button
        role="tab"
        class="tab"
        class:on={tab === e.id}
        aria-selected={tab === e.id}
        onclick={() => (tab = e.id)}
      >
        {engineLabel(e.id, e.name, (k) => $t(k))}
        {#if e.id === defaultEngine}
          <span class="badge">{$t('backup_engine_active')}</span>
        {/if}
      </button>
    {/each}
  </div>
{/if}

{#if shown.length}
  <!-- showEngine is off: the tab above already names it. -->
  <BackupRows
    backups={shown}
    {engineNames}
    showEngine={false}
    busy={busy || running}
    excluded={estimate?.excluded ?? []}
    onrestore={restore}
    ondelete={remove}
  />
{:else}
  <p class="empty">{$t('backup_empty')}</p>
{/if}

<style>
  .tabs {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    margin: 0 0 0.75rem;
  }
  .tab {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    border: 0;
    background: none;
    padding: 0.4rem 0.65rem;
    margin-bottom: -1px;
    border-bottom: 2px solid transparent;
    font-size: 0.85rem;
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
    font-size: 0.68rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 0.05rem 0.38rem;
  }
  .hint {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .make {
    display: flex;
    align-items: center;
    gap: 1rem;
    margin-bottom: 0.6rem;
    flex-wrap: wrap;
  }
  .opt {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.85rem;
    color: var(--text);
  }
  .go {
    border: none;
    border-radius: 5px;
    background: var(--primary);
    color: var(--text-on-accent);
    padding: 0.4rem 0.9rem;
    font-size: 0.85rem;
    font-weight: 600;
    cursor: pointer;
  }
  .go:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .size {
    margin-bottom: 1rem;
    font-variant-numeric: tabular-nums;
  }
  .size.tight {
    color: var(--red);
  }
  .err {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--red);
  }
  /* The exclusion lines sit under the size line and belong to it, so they are
     quieter than a hint and tighter against it. A refused entry is a warning rather
     than an error: the backup still runs, it simply carries more than was asked. */
  .fine {
    margin: -0.7rem 0 0.9rem;
    font-size: 0.78rem;
  }
  .warn.fine {
    color: var(--orange, var(--red));
  }
  .empty {
    margin: 0;
    font-size: 0.85rem;
    color: var(--text-muted);
  }
</style>
