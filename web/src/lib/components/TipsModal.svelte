<script lang="ts">
  import { tipsApp } from '../stores/ui'
  import { renderTips } from '../stores/apps'
  import TipsView from './TipsView.svelte'
  import { t } from '../i18n'

  let { target }: { target: { id: string; name: string } } = $props()

  let tips = $state('')
  let loaded = $state(false)
  let error = $state('')

  async function load() {
    try {
      tips = await renderTips(target.id)
    } catch (e) {
      error = String(e)
    } finally {
      loaded = true
    }
  }
  load()

  function close() {
    tipsApp.set(null)
  }
</script>

<div class="backdrop" onclick={close} role="presentation">
  <div class="dialog" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('tips')} — {target.name}</h2>
    {#if !loaded}
      <p class="hint">{$t('loading')}</p>
    {:else if error}
      <p class="error">{error}</p>
    {:else if tips.trim()}
      <TipsView {tips} />
    {:else}
      <p class="hint">No tips for this app yet.</p>
    {/if}
    <div class="actions">
      <button class="ghost" onclick={close}>{$t('back')}</button>
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
    width: min(92vw, 32rem);
    max-height: 80vh;
    display: flex;
    flex-direction: column;
    background: #fff;
    border-radius: 14px;
    padding: 1.25rem 1.4rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.1rem;
  }
  .hint {
    margin: 0;
    color: var(--text-subtle);
    font-size: 0.9rem;
  }
  .error {
    color: var(--red);
    font-size: 0.85rem;
    margin: 0;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
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
</style>
