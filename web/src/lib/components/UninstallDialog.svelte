<script lang="ts">
  import { uninstallTarget } from '../stores/ui'
  import { uninstallApp } from '../stores/apps'
  import { t } from '../i18n'

  let { target }: { target: { id: string; name: string } } = $props()

  let zip = $state(false)
  let busy = $state(false)
  let error = $state('')

  function close() {
    if (!busy) uninstallTarget.set(null)
  }

  // The uninstall itself is NOT awaited here: the request only has to be
  // accepted, and from then on the app's tile carries the progress (two red
  // bars) and any failure. So the dialog closes right away instead of holding
  // the dashboard hostage through a multi-minute zip. `busy` covers just that
  // hand-off, which is where an up-front refusal (a protected app) surfaces.
  async function confirm() {
    busy = true
    error = ''
    try {
      await uninstallApp(target.id, zip)
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
      This stops and removes the app's containers. Your data is never deleted — the app's
      folder is renamed to <code>{target.id}.&lt;date&gt;.archive</code> in <code>AppData/</code>.
      It runs in the background: the tile shows the progress.
    </p>

    <label class="check">
      <input type="checkbox" bind:checked={zip} disabled={busy} />
      <span>Compress the archive to a <code>.zip</code></span>
    </label>
    <p class="note">
      {#if zip}
        The folder is zipped to <code>{target.id}.&lt;date&gt;.archive.zip</code>, then the original
        folder is removed. Restore by unzipping it back to <code>{target.id}</code>.
      {:else}
        The folder is renamed (kept as-is). Restore by renaming it back to
        <code>{target.id}</code>.
      {/if}
    </p>

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
    background: rgba(0, 0, 0, 0.5);
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
    color: var(--grey-600);
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
    color: var(--grey-600);
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
