<script lang="ts">
  // The list of one app's archives, with its two destructive buttons. Shared by
  // the app's Backups tab and the global Backups settings page, so a backup looks
  // and behaves the same wherever it is reached from — which matters because after
  // an uninstall the global page is the *only* place it can be reached from.
  //
  // Rows are grouped by the engine that holds them, and that grouping is the whole
  // model: engines run independently, so a stamp present both on the data disk and
  // in a repository is TWO backups, restorable and deletable separately. They used to
  // be folded into one row marked "on this disk + offsite", which could only ever be
  // right while exactly one engine was offsite.
  //
  // Both actions confirm inline rather than through a dialog: they are one row's
  // worth of decision, and the row already says which archive it is about.
  import { type Backup, renderStamp, engineLabel } from '../stores/backups'
  import { renderSize } from '../format'
  import { t } from '../i18n'

  let {
    backups,
    engineNames = {},
    busy = false,
    onrestore,
    ondelete,
  }: {
    backups: Backup[]
    /** Engine ID -> the name the deployment provisioned for it, when it has one. */
    engineNames?: Record<string, string>
    busy?: boolean
    onrestore: (b: Backup) => void
    ondelete: (b: Backup) => void
  } = $props()

  /** Which row is asking for confirmation, and for what. Keyed by engine AND name,
   *  because the same stamp can appear under two engines and confirming one must not
   *  arm the other. One at a time: opening a second confirmation closes the first. */
  let pending = $state<{ key: string; action: 'restore' | 'delete' } | null>(null)

  /** A row's identity. Not the name alone — two engines can hold the same stamp, and
   *  a duplicate key is both a Svelte error and the wrong backup being acted on. */
  const rowKey = (b: Backup) => `${b.engine ?? ''}:${b.name}`

  function ask(b: Backup, action: 'restore' | 'delete') {
    pending = { key: rowKey(b), action }
  }

  function confirm(b: Backup) {
    const action = pending?.action
    pending = null
    if (action === 'restore') onrestore(b)
    else if (action === 'delete') ondelete(b)
  }

  /** Grouped by engine, in the order the server listed them — registration order,
   *  so the local engine leads and the sections do not reshuffle between reloads. */
  const groups = $derived.by(() => {
    const out: { engine: string; label: string; offsite: boolean; rows: Backup[] }[] = []
    for (const b of backups) {
      const id = b.engine ?? ''
      let g = out.find((x) => x.engine === id)
      if (!g) {
        g = {
          engine: id,
          label: engineLabel(id, engineNames[id], (k) => $t(k)),
          // Derived from the row rather than asked of the server: this component is
          // handed a list, not a status, and the tier is exactly this fact.
          offsite: b.tier === 'remote',
          rows: [],
        }
        out.push(g)
      }
      g.rows.push(b)
    }
    return out
  })
</script>

{#each groups as g (g.engine)}
  <section class="group">
    <!-- The engine is named here rather than on every row, and it is where the
         offsite question is answered: a backup's whole value is whether it survives
         losing this server, and the engine is what decides that. The name comes from
         the deployment when it provisioned one, so a PCS says "Yundera Backup
         Storage" while the same engine self-hosted says only what it is. -->
    <h5 class="engine">
      {g.label}
      <span class="tier">{g.offsite ? $t('backup_tier_remote') : $t('backup_tier_local')}</span>
    </h5>
    <ul class="rows">
      {#each g.rows as b (b.engine + ':' + b.name)}
        <li class="row" class:asking={pending?.key === b.engine + ':' + b.name}>
          <span class="when">{renderStamp(b.stamp)}</span>
          <!-- Both surfaces that use this component fetch measured lists, so the size
               is always real — including 0 B, which is itself worth showing. -->
          <span class="meta">
            {b.zip ? $t('backup_zip') : $t('backup_folder')} · {renderSize(b.size)}
          </span>

          {#if pending?.key === b.engine + ':' + b.name}
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
  </section>
{/each}

<style>
  .group + .group {
    margin-top: 0.9rem;
  }
  .engine {
    margin: 0 0 0.35rem;
    font-size: 0.8rem;
    font-weight: 600;
    color: var(--text);
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
  }
  .tier {
    font-weight: 400;
    color: var(--text-muted);
    white-space: nowrap;
  }
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
