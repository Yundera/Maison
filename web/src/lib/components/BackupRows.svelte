<script lang="ts">
  // The list of one app's archives, with its two destructive buttons. Shared by
  // the app's Backups tab and the global Backups settings page, so a backup looks
  // and behaves the same wherever it is reached from — which matters because after
  // an uninstall the global page is the *only* place it can be reached from.
  //
  // Both actions confirm inline rather than through a dialog: they are one row's
  // worth of decision, and the row already says which archive it is about.
  import { type Backup, renderStamp } from '../stores/backups'
  import { renderSize } from '../format'
  import { t } from '../i18n'

  let {
    backups,
    busy = false,
    onrestore,
    ondelete,
  }: {
    backups: Backup[]
    busy?: boolean
    onrestore: (b: Backup) => void
    ondelete: (b: Backup) => void
  } = $props()

  // Which row is asking for confirmation, and for what. One at a time: opening a
  // second confirmation closes the first, so there is never a choice about which
  // "Confirm" you are looking at.
  let pending = $state<{ name: string; action: 'restore' | 'delete' } | null>(null)

  function ask(b: Backup, action: 'restore' | 'delete') {
    pending = { name: b.name, action }
  }

  // The tiers the server can report. Kept here as data so the guard in the markup is
  // one list rather than a chain of comparisons.
  const TIERS = ['local', 'remote', 'both']

  function confirm(b: Backup) {
    const action = pending?.action
    pending = null
    if (action === 'restore') onrestore(b)
    else if (action === 'delete') ondelete(b)
  }
</script>

<ul class="rows">
  {#each backups as b (b.name)}
    <!-- Guarded rather than interpolated straight into the key: t() falls back to the
         key it was given, so an engine that forgot to set a tier would put the string
         "backup_tier_" on screen. Nothing is a better label than that. -->
    {@const where = TIERS.includes(b.tier) ? $t(`backup_tier_${b.tier}`) : ''}
    <li class="row" class:asking={pending?.name === b.name}>
      <span class="when">{renderStamp(b.stamp)}</span>
      <!-- Both surfaces that use this component fetch measured lists, so the size
           is always real — including 0 B, which is itself worth showing.

           Where it is comes last and is always shown, including for the ordinary
           local case: a backup's whole value is whether it survives losing this
           server, and a row that only says "folder · 2 GB" cannot answer that. It
           is the tier, not the engine name, because "kopia" is an implementation
           detail and "offsite" is the property the user is buying. -->
      <span class="meta">
        {b.zip ? $t('backup_zip') : $t('backup_folder')} · {renderSize(b.size)}{#if where}
          · <span class="where" title={b.engine ?? ''}>{where}</span>
        {/if}
      </span>

      {#if pending?.name === b.name}
        <span class="warn">
          {pending.action === 'delete' ? $t('backup_delete_confirm') : $t('backup_restore_confirm')}
        </span>
        <button class="btn" onclick={() => (pending = null)}>{$t('cancel')}</button>
        <button class="btn danger" disabled={busy} onclick={() => confirm(b)}>
          {$t('confirm')}
        </button>
      {:else}
        <button class="btn" disabled={busy} onclick={() => ask(b, 'restore')}>
          {$t('restore')}
        </button>
        <button class="btn danger" disabled={busy} onclick={() => ask(b, 'delete')}>
          {$t('delete')}
        </button>
      {/if}
    </li>
  {/each}
</ul>

<style>
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.5rem 0.65rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    font-size: 0.85rem;
    flex-wrap: wrap;
  }
  .row.asking {
    border-color: var(--red);
    background: hsla(0, 80%, 97%, 1);
  }
  .when {
    font-variant-numeric: tabular-nums;
    font-weight: 600;
    color: var(--text);
  }
  .meta {
    color: var(--text-muted);
    flex: 1;
    min-width: 8rem;
  }
  .where {
    white-space: nowrap;
  }
  .warn {
    color: var(--red);
    flex-basis: 100%;
    order: 9;
  }
  .btn {
    border: 1px solid var(--border-strong);
    border-radius: 5px;
    background: var(--surface);
    padding: 0.25rem 0.6rem;
    font-size: 0.8rem;
    color: var(--text);
    cursor: pointer;
  }
  .btn:hover:not(:disabled) {
    background: var(--surface-2);
  }
  .btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .btn.danger {
    color: var(--red);
    border-color: hsla(0, 60%, 80%, 1);
  }
</style>
