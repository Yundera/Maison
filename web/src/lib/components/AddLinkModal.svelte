<script lang="ts">
  import { addLink } from '../stores/links'

  let { onclose }: { onclose: () => void } = $props()
  let name = $state('')
  let url = $state('')
  let icon = $state('')

  function submit(e: Event) {
    e.preventDefault()
    if (!name.trim() || !url.trim()) return
    addLink(name.trim(), url.trim(), icon.trim() || undefined)
    onclose()
  }
</script>

<div class="backdrop" onclick={onclose} role="presentation">
  <form class="dialog" onclick={(e) => e.stopPropagation()} onsubmit={submit}>
    <h2>Add external link</h2>
    <label>Name<input bind:value={name} placeholder="My service" /></label>
    <label>URL<input bind:value={url} placeholder="https://example.com" /></label>
    <label>Icon URL (optional)<input bind:value={icon} placeholder="https://…/icon.png" /></label>
    <div class="actions">
      <button type="button" class="ghost" onclick={onclose}>Cancel</button>
      <button type="submit" class="primary">Add</button>
    </div>
  </form>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 100;
    display: grid;
    place-items: center;
    background: rgba(0, 0, 0, 0.45);
  }
  .dialog {
    width: min(92vw, 26rem);
    background: #fff;
    border-radius: 14px;
    padding: 1.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.25rem;
    font-size: 1.1rem;
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    font-size: 0.8rem;
    color: var(--grey-600);
  }
  input {
    padding: 0.5rem 0.6rem;
    border: 1px solid hsla(208, 16%, 85%, 1);
    border-radius: 8px;
    font-size: 0.9rem;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.25rem;
  }
  .actions button {
    padding: 0.5rem 1rem;
    border-radius: 8px;
    border: none;
    font-size: 0.875rem;
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
  .primary {
    background: var(--primary);
    color: #fff;
  }
</style>
