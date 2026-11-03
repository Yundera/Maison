<script lang="ts">
  import { setupApp } from '../stores/ui'
  import { fetchInputs, openApp, saveInputs, type App, type InputState } from '../stores/apps'
  import InputFields from './InputFields.svelte'
  import { loc, t } from '../i18n'

  // The values only the user can give an installed app (its declared inputs).
  // Opened from the tile while a required one is still empty — instead of an app
  // that cannot work without it — and from the tile menu at any time.
  let { target }: { target: { app: App; launch: boolean } } = $props()

  let inputs = $state<InputState[]>([])
  let values = $state<Record<string, string>>({})
  let set = $state<Record<string, boolean>>({})
  let loaded = $state(false)
  let saving = $state(false)
  let saved = $state(false)
  let error = $state('')

  // A required secret that is already set counts as filled even though its field
  // is empty: empty means "keep it".
  const missing = $derived(
    inputs.some((i) => i.required && !(values[i.env] ?? '').trim() && !(i.secret && set[i.env])),
  )

  async function load() {
    try {
      inputs = await fetchInputs(target.app.id)
      for (const i of inputs) {
        values[i.env] = i.secret ? '' : (i.value ?? '')
        set[i.env] = i.set
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      loaded = true
    }
  }
  load()

  async function save() {
    if (missing || saving) return
    saving = true
    error = ''
    try {
      await saveInputs(target.app.id, values)
      saved = true
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      saving = false
    }
  }

  function close() {
    setupApp.set(null)
  }

  // Synchronous inside the button's own click, like the tile's: that is what keeps
  // a new-tab open clear of the popup blocker.
  function launch(ev: MouseEvent) {
    const app = target.app
    close()
    openApp(app, ev)
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === 'Escape') close()
  }}
/>

<div class="backdrop sheet-backdrop" onclick={close} role="presentation">
  <div class="dialog sheet" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('setup_title', { name: $loc(target.app.title, target.app.name) })}</h2>

    {#if !loaded}
      <p class="body">{$t('loading')}</p>
    {:else if saved}
      <p class="body">{$t('setup_saved')}</p>
      <div class="actions">
        <button class="ghost" onclick={close}>{$t('close')}</button>
        <button class="go" onclick={launch}>{$t('open_app')}</button>
      </div>
    {:else}
      <p class="body">{$t('setup_intro')}</p>
      <form
        onsubmit={(e) => {
          e.preventDefault()
          void save()
        }}
      >
        <InputFields {inputs} bind:values {set} />
        {#if error}<p class="error">{error}</p>{/if}
        <div class="actions">
          <button type="button" class="ghost" onclick={close}>{$t('cancel')}</button>
          <button type="submit" class="go" disabled={missing || saving}>
            {saving ? $t('saving') : $t('save')}
          </button>
        </div>
      </form>
    {/if}
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
    width: min(92vw, 28rem);
    max-height: 85vh;
    overflow-y: auto;
    background: #fff;
    border-radius: var(--r-xl);
    padding: 1.25rem 1.4rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.6rem;
    font-size: 1.1rem;
  }
  .body {
    margin: 0 0 0.75rem;
    color: var(--text-subtle);
    font-size: 0.9rem;
  }
  .error {
    margin: 0.6rem 0 0;
    color: var(--red);
    font-size: 0.85rem;
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
