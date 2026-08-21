<script lang="ts">
  import { uninstallTarget } from '../stores/ui'
  import { uninstallApp } from '../stores/apps'
  import { fetchBackupStatus } from '../stores/backupengine'
  import { engineLabel } from '../stores/backups'
  import { t } from '../i18n'

  let { target }: { target: { id: string; name: string } } = $props()

  let zip = $state(false)
  let busy = $state(false)
  let error = $state('')

  /** Where this uninstall's backup will be written, so the dialog can say so.
   *
   *  It has to be named out loud. An uninstall is the one destructive thing a user does
   *  on purpose, and "your data is safe" is only a true sentence if it says *where* —
   *  on a box backing up offsite the answer is a repository, and on a default install
   *  it is the same disk the app was on, which is a rollback and not a safety net.
   *
   *  A status call that does not answer leaves the name empty and the copy falls back
   *  to the generic wording: the uninstall still works, and the server resolves the
   *  engine itself. */
  let engineId = $state('')
  let engineName = $state('')
  let offsite = $state(false)
  fetchBackupStatus()
    .then((s) => {
      const active = (s.engines ?? []).find((e) => e.id === s.active)
      engineId = s.active
      engineName = engineLabel(s.active, active?.name, (k) => $t(k))
      offsite = active?.offsite ?? false
    })
    .catch(() => {})

  // Zipping is a local-engine idea. An engine that deduplicates ignores it — a zip is
  // one opaque blob whose every byte changes when anything inside it does — so offering
  // the checkbox against a repository would offer a setting with no effect.
  const canZip = $derived(engineId !== '' && !offsite)

  function close() {
    if (!busy) uninstallTarget.set(null)
  }

  // The uninstall itself is NOT awaited here: the request only has to be
  // accepted, and from then on the app's tile carries the progress (red bars for
  // backing up, then removing) and any failure. So the dialog closes right away
  // instead of holding the dashboard hostage through a multi-minute upload. `busy`
  // covers just that hand-off, which is where an up-front refusal (a protected app)
  // surfaces.
  async function confirm() {
    busy = true
    error = ''
    try {
      await uninstallApp(target.id, canZip && zip)
      uninstallTarget.set(null)
    } catch (e) {
      error = String(e)
      busy = false
    }
  }
</script>

<div class="backdrop" onclick={close} role="presentation">
  <div class="dialog" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('uninstall')} {target.name}?</h2>
    <p class="body">
      {#if engineName}
        This backs the app up to <strong>{engineName}</strong>, then stops and removes its
        containers. Your data is never deleted — the backup is an ordinary one, listed on the
        Backups page, and the app can be reinstalled on top of it from the App Store. It runs in
        the background: the tile shows the progress.
      {:else}
        This backs the app up, then stops and removes its containers. Your data is never deleted —
        the backup is an ordinary one, listed on the Backups page, and the app can be reinstalled
        on top of it from the App Store. It runs in the background: the tile shows the progress.
      {/if}
    </p>

    {#if canZip}
      <label class="check">
        <input type="checkbox" bind:checked={zip} disabled={busy} />
        <span>Compress the archive to a <code>.zip</code></span>
      </label>
      <p class="note">
        {#if zip}
          Smaller on disk, but slower to make and to restore. Without it the app's folder is
          simply moved into the backups directory, which is instant at any size.
        {:else}
          The app's folder is moved into the backups directory as it is — instant, whatever the
          app's size.
        {/if}
      </p>
    {/if}

    {#if engineName && !offsite}
      <p class="note warn">
        {engineName} keeps the backup on this server's own disk, so it protects against a mistaken
        uninstall but not against losing the disk. Choose a remote engine in Settings → Backups if
        you need that.
      </p>
    {/if}

    {#if error}<p class="error">{error}</p>{/if}

    <div class="actions">
      <button class="ghost" onclick={close} disabled={busy}>{$t('cancel')}</button>
      <button class="danger" onclick={confirm} disabled={busy}>
        {busy ? '…' : $t('uninstall')}
      </button>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 110;
    background: var(--scrim);
    display: grid;
    place-items: center;
  }
  .dialog {
    width: min(92vw, 26rem);
    background: #fff;
    border-radius: 14px;
    padding: 1.25rem 1.4rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.5rem;
    font-size: 1.1rem;
  }
  .body {
    margin: 0 0 0.9rem;
    color: var(--text-subtle);
    font-size: 0.9rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.9rem;
    cursor: pointer;
  }
  .note {
    margin: 0.4rem 0 0;
    font-size: 0.78rem;
    color: var(--text-subtle);
  }
  .note.warn {
    margin-top: 0.75rem;
  }
  code {
    background: hsla(208, 16%, 94%, 1);
    padding: 0 0.25rem;
    border-radius: 4px;
  }
  .error {
    color: var(--red);
    font-size: 0.8rem;
    margin: 0.6rem 0 0;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 1.1rem;
  }
  .actions button {
    padding: 0.5rem 1.1rem;
    border-radius: 8px;
    border: none;
    font-size: 0.875rem;
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
  .danger {
    background: var(--red);
    color: #fff;
  }
  button:disabled {
    opacity: 0.6;
  }
</style>
