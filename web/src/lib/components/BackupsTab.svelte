<script lang="ts">
  // One app's Backups tab.
  //
  // Everything here is about the same folder the tile represents: what archives of
  // it exist, and making one more. The work itself is detached — the tile carries
  // the progress bar, exactly like an install or an uninstall — so this panel
  // starts an operation and then gets out of the way.
  import {
    fetchBackups,
    estimateBackup,
    startBackup,
    restoreBackup,
    deleteBackup,
    type Backup,
    type Estimate,
  } from '../stores/backups'
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

  async function load() {
    try {
      backups = await fetchBackups(id)
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    }
  }

  // Re-measure whenever the artefact choice changes: a zip has to budget for the
  // snapshot and the zip at once, so the two answers genuinely differ.
  $effect(() => {
    const wantZip = zip
    estimateBackup(id, wantZip)
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

  const backup = () => run(() => startBackup(id, zip))
  const restore = (b: Backup) => run(() => restoreBackup(id, b.name))
  const remove = (b: Backup) => run(() => deleteBackup(id, b.name))
</script>

<p class="hint">{$t('backup_hint')}</p>

<div class="make">
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
{/if}

{#if error}
  <p class="err">{error}</p>
{/if}

{#if running}
  <p class="hint">{$t('backup_running')}</p>
{/if}

{#if backups.length}
  <BackupRows {backups} busy={busy || running} onrestore={restore} ondelete={remove} />
{:else}
  <p class="empty">{$t('backup_empty')}</p>
{/if}

<style>
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
  .empty {
    margin: 0;
    font-size: 0.85rem;
    color: var(--text-muted);
  }
</style>
