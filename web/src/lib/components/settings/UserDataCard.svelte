<script lang="ts">
  /**
   * Settings › Backups › Your files — the user-data set.
   *
   * Everything at the data root except AppData/, which each app already backs up on its
   * own. It sits in its own card above the app list rather than as a row in it, because
   * it is not an app and the backend deliberately refuses to model it as one: no compose
   * project, no containers, no tile, and a reserved name that is not a valid project
   * name so it can never be pushed through the guards written for app paths.
   *
   * The two things this card has to get right are both about honesty:
   *
   *   - When the box *cannot* do this — the default install, local engine selected — an
   *     empty list would read as "nothing to worry about". So the unavailable state is a
   *     stated reason, not a blank.
   *   - Restoring in place is destructive and unavoidably so: there is no second copy of
   *     a terabyte tree to swap in atomically. The UI says what will happen in the words
   *     of the thing that happens, and defaults to the safe mode.
   */
  import { t } from '../../i18n'
  import { renderSize } from '../../format'
  import { renderStamp, restoreUserData, type Backup, type UserDataBackups } from '../../stores/backups'

  let {
    data,
    engine,
    onchanged,
  }: {
    data: UserDataBackups
    /** The engine whose tab this card is in. Restores name it, so a snapshot in an
     *  engine that is no longer the default is still restorable. */
    engine: string
    onchanged: () => void
  } = $props()

  // Which backup is being restored, and how. Null means the picker is closed — the
  // destructive option is never one stray click away.
  let picked = $state<{ backup: Backup; mode: 'copy' | 'in_place' } | null>(null)
  let confirming = $state(false)
  let busy = $state(false)
  let error = $state('')

  const restoring = $derived(data.restore.running)

  /** Where a copy-mode restore lands. Under the data root so it is reachable from every
   *  app and every file share on the box, and named for the backup so two restores do
   *  not merge into each other. */
  const copyDest = (b: Backup) => `${data.source}/Restored/${b.stamp}`

  function open(b: Backup, mode: 'copy' | 'in_place') {
    picked = { backup: b, mode }
    confirming = false
    error = ''
  }

  async function go() {
    if (!picked) return
    busy = true
    error = ''
    try {
      await restoreUserData(picked.backup.name, engine, {
        dest: picked.mode === 'copy' ? copyDest(picked.backup) : '',
      })
      picked = null
      confirming = false
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
      onchanged()
    }
  }
</script>

<section class="card">
  <h4>{$t('user_data')}</h4>
  <p class="hint">{$t('user_data_hint', { source: data.source })}</p>

  <!-- An interrupted in-place restore outlives the process that started it, so this is
       read from a marker on disk rather than from anything in memory. It is the first
       thing on the card because the tree is currently neither the old state nor the new
       one, and nothing else on this page matters until that is resolved. -->
  {#if data.restore.interrupted}
    <p class="alarm">
      {$t('user_data_interrupted')}
      {#if data.restore.interrupted_stamp}
        <br />{$t('user_data_interrupted_stamp', { when: renderStamp(data.restore.interrupted_stamp) })}
      {/if}
    </p>
  {/if}

  {#if !data.available}
    <p class="warn">{data.reason}</p>
  {:else}
    <p class="totals">
      {#if data.size > 0}
        {$t('user_data_size', { size: renderSize(data.size) })} ·
      {/if}
      {$t('user_data_count', { count: String(data.backups.length) })}
    </p>
  {/if}

  {#if data.restore.running}
    <p class="running">
      {data.restore.message || $t('user_data_restoring')}
    </p>
  {:else if data.restore.error}
    <p class="err">{data.restore.error}</p>
  {/if}

  {#if error}<p class="err">{error}</p>{/if}

  {#if data.available && !data.backups.length}
    <p class="empty">{$t('user_data_empty')}</p>
  {/if}

  {#if data.backups.length}
    <ul class="rows">
      {#each data.backups as b (b.name)}
        <li class="row" class:asking={picked?.backup.name === b.name}>
          <span class="when">{renderStamp(b.stamp)}</span>
          <span class="meta">{renderSize(b.size)}</span>

          {#if picked?.backup.name === b.name}
            <div class="choice">
              <!-- Copy first and selected by default: it is the operation that answers
                   "get my files from three weeks ago back" without putting anything at
                   risk, and it is what most people actually want. -->
              <label>
                <input type="radio" value="copy" bind:group={picked.mode} disabled={busy} />
                <span>
                  <strong>{$t('user_data_mode_copy')}</strong>
                  <em>{$t('user_data_mode_copy_hint', { dest: copyDest(b) })}</em>
                </span>
              </label>
              <label>
                <input type="radio" value="in_place" bind:group={picked.mode} disabled={busy} />
                <span>
                  <strong>{$t('user_data_mode_in_place')}</strong>
                  <em>{$t('user_data_mode_in_place_hint')}</em>
                </span>
              </label>

              {#if picked.mode === 'in_place'}
                <!-- Said before the button, in the words of what happens: files made since
                     this backup are deleted, and AppData is not touched at all. Both halves
                     matter — the second is what stops this reading as "restore everything",
                     which it is not. -->
                <p class="danger">{$t('user_data_in_place_warning')}</p>
                <p class="fine">{$t('user_data_excluded', { list: data.excluded.join(', ') })}</p>
                <label class="ack">
                  <input type="checkbox" bind:checked={confirming} disabled={busy} />
                  {$t('user_data_in_place_ack')}
                </label>
              {/if}

              <div class="actions">
                <button class="btn" onclick={() => (picked = null)} disabled={busy}>{$t('cancel')}</button>
                <button
                  class="btn go"
                  class:danger={picked.mode === 'in_place'}
                  disabled={busy || (picked.mode === 'in_place' && !confirming)}
                  onclick={go}
                >
                  {picked.mode === 'in_place' ? $t('user_data_do_in_place') : $t('user_data_do_copy')}
                </button>
              </div>
            </div>
          {:else}
            <button class="btn" disabled={busy || restoring} onclick={() => open(b, 'copy')}>
              {$t('restore')}
            </button>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .card {
    max-width: 46rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
    margin-bottom: 1.25rem;
  }
  h4 {
    margin: 0 0 0.25rem;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--text);
  }
  .hint,
  .empty {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .totals {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }
  .warn {
    margin: 0;
    padding: 0.6rem 0.75rem;
    border-radius: 6px;
    background: hsla(38, 92%, 95%, 1);
    color: hsl(30, 80%, 28%);
    font-size: 0.85rem;
    line-height: 1.5;
  }
  .alarm {
    margin: 0 0 0.9rem;
    padding: 0.6rem 0.75rem;
    border-radius: 6px;
    border: 1px solid var(--red);
    background: hsla(0, 80%, 97%, 1);
    color: var(--red);
    font-size: 0.85rem;
    line-height: 1.5;
  }
  .running {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--text);
  }
  .err {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--red);
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
    border-color: var(--border-strong);
    background: var(--surface-2);
  }
  .when {
    font-variant-numeric: tabular-nums;
    font-weight: 600;
    color: var(--text);
  }
  .meta {
    color: var(--text-muted);
    flex: 1;
    min-width: 6rem;
  }
  .choice {
    flex-basis: 100%;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    padding-top: 0.6rem;
  }
  .choice > label {
    display: flex;
    gap: 0.5rem;
    align-items: flex-start;
  }
  .choice strong {
    display: block;
    font-weight: 600;
    font-size: 0.85rem;
  }
  .choice em {
    display: block;
    font-style: normal;
    font-size: 0.8rem;
    color: var(--text-muted);
    word-break: break-all;
  }
  .danger {
    margin: 0;
    padding: 0.55rem 0.7rem;
    border-radius: 6px;
    border: 1px solid var(--red);
    color: var(--red);
    font-size: 0.82rem;
    line-height: 1.5;
  }
  .fine {
    margin: 0;
    font-size: 0.78rem;
    color: var(--text-muted);
  }
  .ack {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.82rem;
    color: var(--text);
  }
  .actions {
    display: flex;
    gap: 0.5rem;
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
