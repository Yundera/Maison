<script lang="ts">
  import { tipsApp } from '../stores/ui'
  import { renderTips } from '../stores/apps'
  import { renderMarkdown } from '../markdown'
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
      <div class="tips markdown">{@html renderMarkdown(tips, { breaks: true })}</div>
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
    background: rgba(0, 0, 0, 0.5);
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
    color: var(--grey-600);
    font-size: 0.9rem;
  }
  .tips {
    overflow: auto;
    padding: 0.75rem 0.85rem;
    background: hsla(208, 16%, 96%, 1);
    border-radius: 8px;
    font-size: 0.85rem;
    line-height: 1.5;
    word-break: break-word;
  }
  .markdown :global(h3),
  .markdown :global(h4),
  .markdown :global(h5) {
    color: #29343d;
    font-weight: 600;
    margin: 0.9rem 0 0.4rem;
  }
  .markdown :global(:first-child) {
    margin-top: 0;
  }
  .markdown :global(p) {
    margin: 0 0 0.6rem;
  }
  .markdown :global(:last-child) {
    margin-bottom: 0;
  }
  .markdown :global(ul) {
    margin: 0 0 0.6rem;
    padding-left: 1.25rem;
  }
  .markdown :global(li) {
    margin: 0.2rem 0;
  }
  .markdown :global(code) {
    background: hsla(208, 16%, 90%, 1);
    padding: 0 0.25rem;
    border-radius: 4px;
    font-size: 0.82rem;
  }
  .markdown :global(pre) {
    margin: 0 0 0.6rem;
    padding: 0.6rem 0.7rem;
    overflow-x: auto;
    background: hsla(208, 16%, 90%, 1);
    border-radius: 6px;
  }
  .markdown :global(pre code) {
    padding: 0;
    background: none;
  }
  .markdown :global(table) {
    width: 100%;
    margin: 0 0 0.6rem;
    border-collapse: collapse;
    font-size: 0.82rem;
  }
  .markdown :global(th),
  .markdown :global(td) {
    padding: 0.35rem 0.5rem;
    text-align: left;
    border: 1px solid hsla(208, 16%, 86%, 1);
  }
  .markdown :global(th) {
    color: #29343d;
    font-weight: 600;
    background: hsla(208, 16%, 92%, 1);
  }
  .markdown :global(a) {
    color: var(--primary);
  }
  .markdown :global(strong) {
    color: #29343d;
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
