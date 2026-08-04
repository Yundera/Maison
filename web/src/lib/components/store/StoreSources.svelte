<script lang="ts">
  import {
    fetchStoreSources,
    addStoreSource,
    removeStoreSource,
    refreshStoreSource,
  } from '../../stores/store'
  import { clickOutside } from '../../actions'

  let { count, onchanged }: { count: number; onchanged: () => void } = $props()

  let open = $state(false)
  let sources = $state<string[]>([])
  let adding = $state(false)
  let url = $state('')
  let busy = $state(false)
  let reloading = $state('')
  // One message line for the whole menu: add, remove and ⟳ all report here, so a
  // reload that never reached the origin says so instead of just stopping the
  // spinner. Cleared at the start of each action.
  let error = $state('')

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  async function load() {
    try {
      sources = (await fetchStoreSources()).sources
    } catch {
      /* ignore */
    }
  }

  function toggle() {
    open = !open
    if (open) load()
  }

  function shortName(u: string): string {
    try {
      const p = new URL(u)
      const seg = p.pathname.split('/').filter(Boolean)
      return seg.length >= 2 ? `${seg[0]}/${seg[1]}` : p.hostname
    } catch {
      return u
    }
  }

  async function add() {
    if (!url.trim()) return
    busy = true
    error = ''
    try {
      const res = await addStoreSource(url.trim())
      sources = res.sources
      // The source is saved either way; a warning means it couldn't be fetched,
      // so keep the message up rather than closing the input on a silent no-op.
      error = res.warning ?? ''
      if (!res.warning) {
        url = ''
        adding = false
      }
      onchanged()
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  async function remove(u: string) {
    busy = true
    error = ''
    try {
      const res = await removeStoreSource(u)
      sources = res.sources
      error = res.warning ?? ''
      onchanged()
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  async function reload(u: string) {
    reloading = u
    error = ''
    try {
      await refreshStoreSource(u)
      onchanged()
    } catch (e) {
      error = message(e)
    } finally {
      reloading = ''
    }
  }
</script>

<div class="wrap">
  <button class="trigger" onclick={toggle}>
    {count} apps <span class="caret">{open ? '▴' : '▾'}</span>
  </button>

  {#if open}
    <div class="menu" use:clickOutside={() => (open = false)}>
      <div class="head">App store sources</div>
      {#each sources as u (u)}
        <div class="row">
          <span class="name" title={u}>{shortName(u)}</span>
          <button
            class="reload"
            class:spin={reloading === u}
            aria-label="Reload"
            title="Reload store"
            disabled={busy || reloading !== ''}
            onclick={() => reload(u)}>⟳</button
          >
          <button
            class="trash"
            aria-label="Remove"
            disabled={busy || sources.length <= 1}
            onclick={() => remove(u)}>✕</button
          >
        </div>
      {/each}

      <hr />
      {#if adding}
        <div class="add">
          <input
            placeholder="https://…/AppStore/archive/refs/heads/main.zip"
            bind:value={url}
            onkeydown={(e) => e.key === 'Enter' && add()}
          />
          <button class="go" disabled={busy} onclick={add}>{busy ? '…' : 'Add'}</button>
        </div>
      {:else}
        <button class="add-source" onclick={() => (adding = true)}>+ Add source</button>
      {/if}

      <!-- Outside the add form: a failed ⟳ or remove reports here too. -->
      {#if error}<div class="err">{error}</div>{/if}
    </div>
  {/if}
</div>

<style>
  .wrap {
    position: relative;
  }
  .trigger {
    height: 2rem;
    border: 1px solid #cfcfcf;
    background: #fff;
    border-radius: 6px;
    padding: 0 0.6rem;
    font-size: 0.8rem;
    color: var(--grey-800);
    white-space: nowrap;
  }
  .caret {
    color: var(--text-subtle);
  }
  .menu {
    position: absolute;
    right: 0;
    top: 2.4rem;
    z-index: 30;
    min-width: 15rem;
    background: #fff;
    border-radius: 8px;
    box-shadow: 0 10px 30px rgba(0, 0, 0, 0.2);
    padding: 0.5rem;
    color: var(--grey-800);
  }
  .head {
    font-size: 0.72rem;
    color: var(--text-subtle);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    padding: 0.25rem 0.35rem 0.4rem;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.3rem 0.35rem;
    border-radius: 5px;
  }
  .row:hover {
    background: hsla(208, 16%, 96%, 1);
  }
  .name {
    flex: 1;
    font-size: 0.82rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .reload {
    border: none;
    background: none;
    color: var(--text-subtle);
    font-size: 0.95rem;
    line-height: 1;
    cursor: pointer;
  }
  .reload:disabled {
    cursor: default;
    opacity: 0.4;
  }
  .reload.spin {
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .trash {
    border: none;
    background: none;
    color: var(--red);
    font-size: 0.85rem;
    cursor: pointer;
  }
  .trash:disabled {
    opacity: 0.3;
    cursor: default;
  }
  hr {
    border: none;
    border-top: 1px solid hsla(208, 16%, 90%, 1);
    margin: 0.4rem 0;
  }
  .add-source {
    width: 100%;
    text-align: left;
    border: none;
    background: none;
    color: var(--primary);
    font-size: 0.82rem;
    padding: 0.35rem;
    border-radius: 5px;
    cursor: pointer;
  }
  .add-source:hover {
    background: hsla(208, 16%, 96%, 1);
  }
  .add {
    display: flex;
    gap: 0.4rem;
  }
  .add input {
    flex: 1;
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    padding: 0 0.5rem;
    font-size: 0.78rem;
  }
  .go {
    border: none;
    background: var(--primary);
    color: #fff;
    border-radius: 6px;
    padding: 0 0.8rem;
    font-size: 0.8rem;
  }
  .err {
    color: var(--red);
    font-size: 0.72rem;
    margin-top: 0.35rem;
  }
</style>
