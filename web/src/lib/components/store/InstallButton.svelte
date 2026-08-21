<script lang="ts">
  import { fetchStoreBackups, installApp, type Backup, type BackupEngine } from '../../stores/store'
  import { apps, appProgress } from '../../stores/apps'
  import { engineLabel, renderStamp } from '../../stores/backups'
  import { sanitizeProject } from '../../project'
  import { t } from '../../i18n'
  import type { StoreRef } from '../../storeref'

  let {
    ref,
    installed = false,
    size = 'small',
  }: { ref: StoreRef; installed?: boolean; size?: 'small' | 'normal' } = $props()

  // Progress is not owned here — it rides the live "apps" channel, the same
  // source the dashboard tile reads. This button just kicks the install off and
  // reflects whatever the channel reports, so it stays in sync with the tile
  // (and keeps advancing even if this store panel is closed and reopened).
  let starting = $state(false) // optimistic: click → channel confirms
  let error = $state('')

  // Backups are fetched on click rather than on mount: the store grid renders one
  // of these per catalog app, and prefetching would fire a request per row for a
  // list that is almost always empty. For a remote engine it is also a subprocess
  // against the repository, which makes prefetching per row worse than slow.
  let engines = $state<BackupEngine[]>([])
  let picking = $state(false) // the backup picker is open
  let loading = $state(false) // looking for backups after a click

  const hasBackups = $derived(engines.some((e) => e.backups.length > 0))

  const projectId = $derived(sanitizeProject(ref.id))
  const entry = $derived($apps.find((a) => a.id === projectId))
  // The one bar, from the same helper the dashboard tile uses: the step running
  // now, in its operation's colour. It also covers an uninstall started from the
  // dashboard — the pill then shows that instead of offering to install.
  const progress = $derived(appProgress(entry))
  const busy = $derived(starting || progress !== null)
  const pct = $derived(Math.round(progress?.pct ?? 0))
  const failed = $derived(entry?.install_error ?? '')
  const isInstalled = $derived(
    installed || (!!entry && !entry.installing && !entry.uninstalling && !entry.install_error),
  )

  // Drop the optimistic flag once the channel confirms the install is tracked.
  $effect(() => {
    if (entry?.installing || entry?.install_error) starting = false
  })

  /** Click → look for this app's backups. None (the common case): install straight
   *  away, one click as before. Some: offer them, so a reinstall can land on the
   *  old data instead of a clean slate. */
  async function onclick(e: MouseEvent) {
    e.stopPropagation()
    if (isInstalled || busy || loading) return
    loading = true
    error = ''
    try {
      engines = (await fetchStoreBackups(ref)).engines
    } catch {
      engines = [] // a failed lookup must not block a plain install
    }
    loading = false
    if (!hasBackups) {
      await install()
      return
    }
    picking = true
  }

  async function install(from?: { name: string; engine?: string }) {
    picking = false
    starting = true
    error = ''
    try {
      // The reference pins the install to the store *and folder* this app was
      // shown from — without it a duplicate id in an earlier store would win the
      // merged-catalog lookup.
      await installApp(ref, from)
    } catch (err) {
      error = String(err)
      starting = false
    }
  }

  // The picker lives inside a store row that opens the app detail on click, so
  // every event it handles stops there.
  function stop(e: MouseEvent) {
    e.stopPropagation()
  }

  function onPickerKey(e: KeyboardEvent) {
    e.stopPropagation()
    if (e.key === 'Escape') picking = false
  }

  /** What a row is, in a few words.
   *
   *  Size first when the engine knows one: it is the fact that distinguishes two rows,
   *  and a repository reports it. "folder" and "zip" describe how a *local* archive is
   *  stored — a repository snapshot is neither, and labelling one "folder" was simply
   *  wrong on screen. Local folder archives are left unmeasured on this path (sizing
   *  one is a tree walk per row, on a click in the catalog), so they still fall back to
   *  naming the form. */
  function describe(b: Backup): string {
    if (b.zip) return b.size > 0 ? `${$t('backup_zip')} · ${humanSize(b.size)}` : $t('backup_zip')
    if (b.size > 0) return humanSize(b.size)
    return $t('backup_folder')
  }

  /** "12.4 MB" — only zips carry a size; a folder backup is left unmeasured. */
  function humanSize(bytes: number): string {
    const units = ['B', 'kB', 'MB', 'GB', 'TB']
    let n = bytes
    let u = 0
    while (n >= 1024 && u < units.length - 1) {
      n /= 1024
      u++
    }
    return `${n < 10 && u > 0 ? n.toFixed(1) : Math.round(n)} ${units[u]}`
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === 'Escape') picking = false
  }}
/>

{#if busy}
  <span
    class="pill installing"
    class:normal={size === 'normal'}
    title="{progress ? $t(progress.label) : $t('installing')} {pct}%"
  >
    <span class="track">
      <span class="fill {progress?.kind ?? 'download'}" style:width={`${pct}%`}></span>
    </span>
    <span class="pct">{pct}%</span>
  </span>
{:else}
  <span class="wrap">
    <button
      class="pill"
      class:done={isInstalled}
      class:failed={!!failed || !!error}
      class:normal={size === 'normal'}
      disabled={isInstalled}
      {onclick}
      title={failed || error}
    >
      {isInstalled ? $t('installed') : $t('install')}
    </button>

    {#if picking}
      <!-- Click-away backdrop: closes the picker without triggering the row
           underneath (the store grid opens the app detail on row click). -->
      <div
        class="backdrop"
        role="presentation"
        onclick={(e) => {
          stop(e)
          picking = false
        }}
      ></div>

      <div class="picker" role="menu" tabindex="-1" onclick={stop} onkeydown={onPickerKey}>
        <button class="row fresh" role="menuitem" onclick={() => install()}>
          {$t('fresh_install')}
        </button>
        <!-- A heading per engine, not one merged list: a stamp held by two engines is
             two backups, and the row the user clicks decides which one is fetched. The
             engine is part of each row's key for the same reason — two engines holding
             the same stamp would otherwise collide. -->
        {#each engines as e (e.engine)}
          {#if e.backups.length > 0}
            <p class="head">
              {$t('restore_from_backup')} · {engineLabel(e.engine, e.name, (k) => $t(k))}
            </p>
            {#each e.backups as b (b.name)}
              <button
                class="row"
                role="menuitem"
                onclick={() => install({ name: b.name, engine: e.engine })}
                title={b.name}
              >
                <!-- The time, not just the date: an app backed up nightly and then
                     uninstalled has two backups on the same day, and two rows reading
                     "2026-08-21" are a choice the user cannot make. -->
                <span class="date">{renderStamp(b.stamp)}</span>
                <span class="meta">{describe(b)}</span>
              </button>
            {/each}
          {/if}
        {/each}
        <p class="note">{$t('restore_from_backup_hint')}</p>
      </div>
    {/if}
  </span>
{/if}

<style>
  .wrap {
    position: relative;
    display: inline-flex;
  }
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 20;
  }
  .picker {
    position: absolute;
    z-index: 21;
    top: calc(100% + 0.35rem);
    right: 0;
    min-width: 15rem;
    padding: 0.3rem;
    border-radius: 0.6rem;
    background: #fff;
    border: 1px solid hsla(216, 20%, 50%, 0.18);
    box-shadow: 0 8px 24px hsla(216, 40%, 20%, 0.18);
    text-align: left;
    cursor: default;
  }
  .picker .head {
    margin: 0.45rem 0.5rem 0.25rem;
    font-size: 0.66rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    opacity: 0.55;
  }
  .picker .note {
    margin: 0.3rem 0.5rem 0.15rem;
    font-size: 0.66rem;
    line-height: 1.35;
    opacity: 0.55;
  }
  .row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 0.75rem;
    width: 100%;
    padding: 0.4rem 0.5rem;
    border: none;
    border-radius: 0.4rem;
    background: none;
    color: inherit;
    font-size: 0.78rem;
    text-align: left;
    cursor: pointer;
  }
  .row:hover {
    background: hsla(216, 90%, 54%, 0.1);
  }
  .row.fresh {
    font-weight: 600;
    color: hsl(216, 72%, 42%);
  }
  .row .date {
    font-variant-numeric: tabular-nums;
  }
  .row .meta {
    font-size: 0.68rem;
    opacity: 0.55;
    white-space: nowrap;
  }
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    border: none;
    border-radius: 999px;
    font-size: 0.72rem;
    font-weight: 600;
    padding: 0.22rem 0.75rem;
    /* is-primary is-light: pale accent bg + dark-blue text */
    background: hsla(216, 90%, 54%, 0.14);
    color: hsl(216, 72%, 42%);
    cursor: pointer;
    white-space: nowrap;
  }
  .pill:hover:not(:disabled) {
    background: hsla(216, 90%, 54%, 0.22);
  }
  .pill.normal {
    font-size: 0.85rem;
    padding: 0.4rem 1.2rem;
  }
  .pill.done {
    background: hsla(118, 70%, 45%, 0.16);
    color: hsl(118, 55%, 32%);
    cursor: default;
  }
  .pill.failed {
    background: hsla(6, 78%, 57%, 0.14);
    color: hsl(6, 60%, 45%);
  }
  .installing {
    background: hsla(216, 90%, 54%, 0.1);
    color: hsl(216, 72%, 42%);
  }
  .track {
    width: 46px;
    height: 4px;
    border-radius: 2px;
    background: hsla(216, 30%, 50%, 0.25);
    overflow: hidden;
  }
  .fill {
    display: block;
    height: 100%;
    transition: width 0.3s ease;
  }
  .fill.download {
    background: var(--progress-download);
  }
  .fill.install {
    background: var(--progress-install);
  }
  .fill.uninstall {
    background: var(--progress-uninstall);
  }
  .fill.backup {
    background: var(--progress-backup);
  }
  .pct {
    font-variant-numeric: tabular-nums;
  }
</style>
