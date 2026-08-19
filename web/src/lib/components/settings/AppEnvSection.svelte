<script lang="ts">
  // The .env.app editor — the variables Maison forwards into every app.
  //
  // It edits the file as text rather than as a form. The file's comments are its
  // documentation (see internal/appenv), it has no fixed schema — a deployment adds
  // its own keys freely — and an empty value means something a field cannot express.
  // A form would need a schema it doesn't have, and would throw the comments away.
  import { onMount, tick } from 'svelte'
  import { loadAppEnv, saveAppEnv } from '../../stores/settings'
  import { t } from '../../i18n'

  let text = $state('')
  let saved = $state('') // last text the server accepted — the revert target
  let ignored = $state<string[]>([])
  let path = $state('')
  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')
  let justSaved = $state(false)
  let errorEl = $state<HTMLElement | null>(null)

  const dirty = $derived(text !== saved)

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  onMount(async () => {
    try {
      const f = await loadAppEnv()
      text = saved = f.text
      ignored = f.ignored ?? []
      path = f.path ?? ''
    } catch (e) {
      error = message(e)
    } finally {
      loading = false
    }
  })

  async function save() {
    busy = true
    error = ''
    try {
      const f = await saveAppEnv(text)
      // Trust the server's echo over the textarea: it is what is on disk.
      text = saved = f.text
      ignored = f.ignored ?? []
      path = f.path ?? path
      justSaved = true
      setTimeout(() => (justSaved = false), 2000)
    } catch (e) {
      // A rejected save left the file untouched, so `saved` is still accurate.
      error = message(e)
      // The textarea is tall enough to push this below the fold, and a rejected
      // save that says nothing on screen reads as a save that worked.
      await tick()
      errorEl?.scrollIntoView({ block: 'nearest' })
    } finally {
      busy = false
    }
  }

  function revert() {
    text = saved
    error = ''
  }
</script>

<section class="card">
  <header>
    <h3>{$t('app_env')}</h3>
    <p class="hint">{$t('app_env_hint')}</p>
    {#if path}
      <p class="hint"><code>{path}</code></p>
    {/if}
  </header>

  {#if loading}
    <p class="note">{$t('loading')}</p>
  {:else}
    <textarea
      bind:value={text}
      spellcheck="false"
      autocapitalize="off"
      aria-label={$t('app_env')}
    ></textarea>

    {#if ignored.length}
      <p class="warn">{$t('app_env_ignored')} {ignored.join(', ')}</p>
    {/if}

    <footer>
      <button class="go" disabled={!dirty || busy} onclick={save}>{busy ? '…' : $t('save')}</button>
      <button class="ghost" disabled={!dirty || busy} onclick={revert}>{$t('revert')}</button>
      <span class="spacer"></span>
      {#if justSaved}<span class="ok">{$t('saved')}</span>{/if}
    </footer>

    <p class="note">{$t('app_env_applies')}</p>
    {#if error}<p class="err" bind:this={errorEl}>{error}</p>{/if}
  {/if}
</section>

<style>
  .card {
    max-width: 52rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
    padding: 1.25rem 1.5rem 1.5rem;
    display: flex;
    flex-direction: column;
    gap: 0.85rem;
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
  .hint code {
    font-size: 0.75rem;
  }
  textarea {
    width: 100%;
    box-sizing: border-box;
    min-height: 18rem;
    resize: vertical;
    border: 1px solid #cfcfcf;
    border-radius: 6px;
    padding: 0.75rem;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 0.8rem;
    line-height: 1.55;
    color: var(--grey-800);
    tab-size: 2;
    white-space: pre;
    overflow-wrap: normal;
    overflow-x: auto;
  }
  footer {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .spacer {
    flex: 1;
  }
  .go {
    border: none;
    background: var(--primary);
    color: #fff;
    border-radius: 6px;
    padding: 0.4rem 1rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .ghost {
    border: 1px solid #cfcfcf;
    background: #fff;
    color: var(--grey-600);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .go:disabled,
  .ghost:disabled {
    opacity: 0.45;
    cursor: default;
  }
  .ok {
    font-size: 0.8rem;
    color: var(--grey-600);
  }
  .note {
    margin: 0;
    font-size: 0.78rem;
    color: var(--grey-600);
  }
  .warn {
    margin: 0;
    font-size: 0.78rem;
    line-height: 1.45;
    color: #8a6d00;
  }
  .err {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--red);
    white-space: pre-wrap;
  }
</style>
