<script lang="ts">
  import { installConfirm } from '../../stores/ui'
  import type { Compat } from '../../stores/store'
  import { reasonText } from '../../requirements'
  import { t } from '../../i18n'

  let {
    target,
  }: { target: { name: string; compat: Compat; resolve: (ok: boolean) => void } } = $props()

  // The same "tick to proceed" gate the uninstall of a backup engine uses: the
  // operator is being told the app will not work as intended, and a single click
  // through a dialog is how that sentence goes unread.
  let ack = $state(false)

  const missing = $derived(target.compat.unsupported ?? [])
  const short = $derived(target.compat.below_min ?? [])
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === 'Escape') target.resolve(false)
  }}
/>

<div class="backdrop sheet-backdrop" onclick={() => target.resolve(false)} role="presentation">
  <div class="dialog sheet" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('req_confirm_title', { name: target.name })}</h2>

    {#if missing.length > 0}
      <p class="body">{$t('req_confirm_missing')}</p>
      <ul>
        {#each missing as r (r.code)}<li>{reasonText(r, $t)}</li>{/each}
      </ul>
    {/if}
    {#if short.length > 0}
      <p class="body">{$t('req_confirm_short')}</p>
      <ul>
        {#each short as r (r.code)}<li>{reasonText(r, $t)}</li>{/each}
      </ul>
    {/if}
    <p class="note">{$t('req_confirm_hint')}</p>

    <label class="check">
      <input type="checkbox" bind:checked={ack} />
      <span>{$t('req_confirm_ack')}</span>
    </label>

    <div class="actions">
      <button class="ghost" onclick={() => target.resolve(false)}>{$t('cancel')}</button>
      <button class="go" onclick={() => target.resolve(true)} disabled={!ack}>
        {$t('req_install_anyway')}
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
    border-radius: var(--r-xl);
    padding: 1.25rem 1.4rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.5rem;
    font-size: 1.1rem;
  }
  .body {
    margin: 0 0 0.3rem;
    color: var(--text-subtle);
    font-size: 0.9rem;
  }
  ul {
    margin: 0 0 0.6rem;
    padding-left: 1.2rem;
    font-size: 0.9rem;
  }
  .note {
    margin: 0.4rem 0 0.8rem;
    font-size: 0.78rem;
    color: var(--text-subtle);
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.9rem;
    cursor: pointer;
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
  .go {
    background: var(--primary);
    color: #fff;
  }
  button:disabled {
    opacity: 0.6;
  }
</style>
