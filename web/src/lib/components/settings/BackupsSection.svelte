<script lang="ts">
  // Every archive on the box, grouped by app.
  //
  // This page exists for the two things an app's own Backups tab cannot do:
  //
  //   - reach the archives of an app that is gone. Uninstalling renames the
  //     folder, so the tile disappears — and with it the tab. Without this page
  //     the most common reason to want a restore ("I uninstalled it and regret
  //     it") would have nowhere to go.
  //   - show what backups cost in total. Deciding to delete one is deciding
  //     against the space it frees, so the free-space figure is on the same
  //     screen as the delete button rather than a click away.
  //
  // Unlike the per-app list this one measures folder archives, which is a walk per
  // archive — affordable because the page is opened deliberately.
  import { fetchAllBackups, restoreBackup, deleteBackup, type AppBackups, type Backup } from '../../stores/backups'
  import { renderSize } from '../../format'
  import { t } from '../../i18n'
  import BackupRows from '../BackupRows.svelte'

  let list = $state<AppBackups[]>([])
  let free = $state<number | null>(null)
  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')

  async function load() {
    loading = true
    try {
      const r = await fetchAllBackups()
      list = r.apps
      free = r.free ?? null
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      loading = false
    }
  }
  load()

  const used = $derived(list.reduce((n, a) => n + a.total, 0))

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

  const restore = (app: string, b: Backup) => run(() => restoreBackup(app, b.name))
  const remove = (app: string, b: Backup) => run(() => deleteBackup(app, b.name))
</script>

<section class="card">
  <header>
    <h3>{$t('backups')}</h3>
    <p class="hint">{$t('backups_hint')}</p>
  </header>

  <p class="totals">
    {$t('backups_total', { used: renderSize(used) })}
    {#if free !== null}
      · {$t('backups_free', { free: renderSize(free) })}
    {/if}
  </p>

  {#if error}
    <p class="err">{error}</p>
  {/if}

  {#if loading}
    <p class="empty">{$t('loading')}</p>
  {:else if !list.length}
    <p class="empty">{$t('backups_empty')}</p>
  {:else}
    {#each list as group (group.app)}
      <div class="group">
        <h4>
          {group.app}
          {#if group.orphan}
            <span class="tag" title={$t('backups_orphan_hint')}>{$t('backups_orphan')}</span>
          {/if}
          <span class="size">{renderSize(group.total)}</span>
        </h4>
        <BackupRows
          backups={group.backups}
          {busy}
          onrestore={(b) => restore(group.app, b)}
          ondelete={(b) => remove(group.app, b)}
        />
      </div>
    {/each}
  {/if}

  <p class="note">{$t('backups_scope_note')}</p>
</section>

<style>
  .card {
    max-width: 46rem;
  }
  header {
    margin-bottom: 0.75rem;
  }
  h3 {
    margin: 0 0 0.25rem;
    font-size: 1rem;
    font-weight: 600;
    color: var(--text);
  }
  .hint,
  .empty,
  .note {
    margin: 0;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .note {
    margin-top: 1.5rem;
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
  .err {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--red);
  }
  .group {
    margin-bottom: 1.4rem;
  }
  h4 {
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
</style>
