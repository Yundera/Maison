<script lang="ts">
  // The app stores this box installs from.
  //
  // This used to be a dropdown hanging off the "N apps" trigger in the store's
  // toolbar — 15rem wide, holding a list, an add form and an error line. The store
  // panel is for browsing apps; managing where apps come from is box configuration,
  // so it lives here and the panel keeps one link to it.
  //
  // Nothing invalidates the store panel when a source changes: App.svelte only
  // renders StorePanel while the store route is open, so navigating here unmounts
  // it and re-opening it re-fetches the catalog.
  import { onMount } from 'svelte'
  import {
    fetchStore,
    fetchStoreSources,
    addStoreSource,
    removeStoreSource,
    refreshStoreSource,
    renameStoreSource,
    type StoreSource,
  } from '../../stores/store'
  import { t } from '../../i18n'
  import { autofocus } from '../../actions'

  let sources = $state<StoreSource[]>([])
  // Apps per store URL, from the catalog — every store's own copy, including the
  // ones that lost an id collision, which is what that store actually offers.
  let counts = $state<Record<string, number>>({})
  let loading = $state(true)
  let url = $state('')
  let busy = $state(false)
  let reloading = $state('')
  // One message line for the whole panel: add, remove and ⟳ all report here, so a
  // reload that never reached the origin says so instead of just stopping the
  // spinner. Cleared at the start of each action.
  let error = $state('')
  // The URL of the row being renamed, and the text in its input. One at a time:
  // the row turns into a field in place, so there is nowhere for a second one to
  // go. '' means nobody is editing.
  let editing = $state('')
  let draft = $state('')
  // Guards the blur handler against saving twice — committing on Enter moves focus
  // off the input, which fires blur right behind it.
  let committing = false

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  // The address minus the implied scheme — the same shortening the store panel
  // applies, so a store reads the same in both places.
  const bare = (u: string) => u.replace(/^https?:\/\//, '')

  /** Recount from the catalog. Failing is not worth reporting: the counts are a
   *  detail beside the list, and the list itself came back fine. */
  async function loadCounts() {
    try {
      const data = await fetchStore()
      const n: Record<string, number> = {}
      for (const a of data.apps) if (a.store) n[a.store] = (n[a.store] ?? 0) + 1
      counts = n
    } catch {
      /* leave the previous counts up */
    }
  }

  onMount(async () => {
    try {
      sources = (await fetchStoreSources()).sources
    } catch (e) {
      error = message(e)
    } finally {
      loading = false
    }
    await loadCounts()
  })

  async function add() {
    if (!url.trim()) return
    busy = true
    error = ''
    try {
      const res = await addStoreSource(url.trim())
      sources = res.sources
      // The source is saved either way; a warning means it couldn't be fetched, so
      // keep the message up and keep what was typed rather than clearing the input
      // on what looks like a success.
      error = res.warning ?? ''
      if (!res.warning) url = ''
      await loadCounts()
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
      await loadCounts()
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
      const res = await refreshStoreSource(u)
      sources = res.sources
      await loadCounts()
    } catch (e) {
      error = message(e)
    } finally {
      reloading = ''
    }
  }

  /** What the row shows as the store's name: the operator's, the store's own, or
   *  the address when the store has never said what it is called. */
  const label = (s: StoreSource) => (s.name === s.url ? bare(s.url) : s.name)

  function startEdit(s: StoreSource) {
    editing = s.url
    // Prefilled with what is on screen rather than left empty, so a small
    // correction to the store's own name is a small edit and not retyping it.
    // Clearing the field is how you go back to the store's own name — see rename.
    draft = label(s)
  }

  /** Save the name being edited. An empty field clears the custom name, which is
   *  the only way back to the store's own; the server treats it that way too. */
  async function rename(s: StoreSource) {
    if (committing) return
    const name = draft.trim()
    // Nothing to do when the text is unchanged, and — for a store that was never
    // renamed — when it still reads as the label we prefilled. Saving that would
    // pin the store's current self-declared name as a custom one.
    if (name === label(s)) {
      editing = ''
      return
    }
    committing = true
    busy = true
    error = ''
    try {
      const res = await renameStoreSource(s.url, name)
      sources = res.sources
      editing = ''
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
      committing = false
    }
  }

  function cancelEdit() {
    editing = ''
    draft = ''
  }
</script>

<section class="card">
  <header>
    <h3>{$t('app_stores')}</h3>
    <p class="hint">{$t('app_stores_hint')}</p>
  </header>

  {#if loading}
    <p class="hint">{$t('loading')}</p>
  {:else}
    <ul class="rows">
      {#each sources as s (s.url)}
        <li class="row">
          <div class="meta">
            <!-- The operator's name for the store if they set one, else the name the
                 store gives itself in store.json; a store that has never been read
                 successfully is listed by its address instead. Click to rename —
                 the address below is what actually identifies it, and it does not
                 change. -->
            {#if editing === s.url}
              <input
                class="rename"
                aria-label={$t('store_name')}
                placeholder={$t('store_name_default')}
                bind:value={draft}
                disabled={busy}
                onkeydown={(e) => {
                  if (e.key === 'Enter') rename(s)
                  else if (e.key === 'Escape') cancelEdit()
                }}
                onblur={() => {
                  // Only when this row is still the one being edited: Escape closes
                  // the field first, and the blur that follows must not then save
                  // the draft it just discarded — which, since cancelling empties
                  // it, would have cleared the store's name.
                  if (editing === s.url) rename(s)
                }}
                use:autofocus
              />
            {:else}
              <button
                class="name"
                title={$t('rename_store')}
                disabled={busy || reloading !== ''}
                onclick={() => startEdit(s)}>{label(s)}</button
              >
            {/if}
            <code class="url">{bare(s.url)}</code>
          </div>
          <span class="count">
            {counts[s.url] === undefined ? '' : $t('store_apps_count', { count: String(counts[s.url]) })}
          </span>
          <button
            class="reload"
            class:spin={reloading === s.url}
            title={$t('reload_store')}
            aria-label={$t('reload_store')}
            disabled={busy || reloading !== ''}
            onclick={() => reload(s.url)}>⟳</button
          >
          <button
            class="trash"
            aria-label={$t('remove')}
            title={sources.length <= 1 ? $t('store_last_source') : $t('remove')}
            disabled={busy || sources.length <= 1}
            onclick={() => remove(s.url)}>✕</button
          >
        </li>
      {/each}
    </ul>

    <div class="add">
      <input
        placeholder="https://…/AppStore/archive/refs/heads/main.zip"
        aria-label={$t('store_url')}
        bind:value={url}
        spellcheck="false"
        autocapitalize="off"
        onkeydown={(e) => e.key === 'Enter' && add()}
      />
      <button class="go" disabled={busy || !url.trim()} onclick={add}>{busy ? '…' : $t('add')}</button>
    </div>

    <!-- Stated rather than configurable: the schedule is in the server (see
         appstore.StartDailyRefresh), and a store is re-read at boot too. -->
    <p class="hint">{$t('store_refresh_hint')}</p>

    {#if error}<p class="err">{error}</p>{/if}
  {/if}
</section>

<style>
  .card {
    max-width: 46rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
    padding: 1.25rem 1.5rem 1.5rem;
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  header {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: #29343d;
  }
  .hint {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.6rem 0.85rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
    background: hsla(208, 16%, 97%, 1);
  }
  .meta {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .name {
    font-size: 0.875rem;
    color: var(--grey-800);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    /* A button so it is reachable by keyboard, styled as the text it replaced. */
    border: none;
    background: none;
    padding: 0;
    text-align: left;
    cursor: text;
    font-family: inherit;
  }
  .name:hover:not(:disabled) {
    text-decoration: underline dotted;
    text-underline-offset: 3px;
  }
  .name:disabled {
    cursor: default;
  }
  .rename {
    width: 100%;
    min-width: 0;
    height: 1.5rem;
    border: 1px solid var(--primary);
    border-radius: 4px;
    padding: 0 0.35rem;
    font-size: 0.875rem;
    font-family: inherit;
    background: #fff;
    color: var(--grey-800);
  }
  .url {
    font-size: 0.72rem;
    color: var(--grey-600);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .count {
    font-size: 0.75rem;
    color: var(--grey-600);
    white-space: nowrap;
    flex: none;
  }
  .reload,
  .trash {
    border: none;
    background: none;
    line-height: 1;
    cursor: pointer;
    flex: none;
    padding: 0.2rem;
  }
  .reload {
    color: var(--text-subtle);
    font-size: 1rem;
  }
  .trash {
    color: var(--red);
    font-size: 0.9rem;
  }
  .reload:disabled,
  .trash:disabled {
    opacity: 0.35;
    cursor: default;
  }
  .reload.spin {
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .add {
    display: flex;
    gap: 0.5rem;
  }
  .add input {
    flex: 1;
    min-width: 0;
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    padding: 0 0.55rem;
    font-size: 0.8rem;
    background: #fff;
    color: var(--grey-800);
  }
  .go {
    border: none;
    background: var(--primary);
    color: #fff;
    border-radius: 6px;
    padding: 0 1rem;
    font-size: 0.8rem;
    cursor: pointer;
  }
  .go:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .err {
    margin: 0;
    color: var(--red);
    font-size: 0.78rem;
  }
</style>
