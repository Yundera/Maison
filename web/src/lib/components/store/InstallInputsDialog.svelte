<script lang="ts">
  import type { InstallNeeds } from '../../stores/ui'
  import InputFields from '../InputFields.svelte'
  import { t } from '../../i18n'

  // Asked before a fresh install of an app that declares inputs: the values only
  // the user can give it, and what it costs and which accounts it needs. Skipping
  // installs it anyway — its tile says "Setup needed" and asks again.
  let {
    target,
  }: {
    target: { name: string; needs: InstallNeeds; resolve: (values: Record<string, string> | null) => void }
  } = $props()

  let values = $state<Record<string, string>>({})

  const inputs = $derived(target.needs.inputs ?? [])
  const missing = $derived(inputs.some((i) => i.required && !(values[i.env] ?? '').trim()))

  function filled(): Record<string, string> {
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(values)) if (v.trim()) out[k] = v.trim()
    return out
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (e.key === 'Escape') target.resolve(null)
  }}
/>

<div class="backdrop sheet-backdrop" onclick={() => target.resolve(null)} role="presentation">
  <div class="dialog sheet" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('install_inputs_title', { name: target.name })}</h2>

    {#if target.needs.cost === 'paid' || target.needs.cost === 'freemium' || target.needs.accounts?.length}
      <ul class="needs">
        {#if target.needs.cost === 'paid' || target.needs.cost === 'freemium'}
          <li>{$t(`cost_${target.needs.cost}_long`)}</li>
        {/if}
        {#each target.needs.accounts ?? [] as acc (acc.provider)}
          <li>
            {$t('needs_account', { provider: acc.provider })}
            {#if acc.url}<a href={acc.url} target="_blank" rel="noopener noreferrer">↗</a>{/if}
          </li>
        {/each}
      </ul>
    {/if}

    <p class="body">{$t('install_inputs_intro')}</p>
    <form
      onsubmit={(e) => {
        e.preventDefault()
        if (!missing) target.resolve(filled())
      }}
    >
      <InputFields {inputs} bind:values />
      <p class="note">{$t('install_inputs_later')}</p>
      <div class="actions">
        <button type="button" class="ghost" onclick={() => target.resolve(null)}>{$t('cancel')}</button>
        <button type="button" class="ghost" onclick={() => target.resolve({})}>{$t('install_inputs_skip')}</button>
        <button type="submit" class="go" disabled={missing}>{$t('install')}</button>
      </div>
    </form>
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
  .needs {
    margin: 0 0 0.75rem;
    padding: 0.6rem 0.8rem 0.6rem 1.8rem;
    background: hsl(38, 92%, 95%);
    border-radius: 10px;
    font-size: 0.875rem;
  }
  .body {
    margin: 0 0 0.75rem;
    color: var(--text-subtle);
    font-size: 0.9rem;
  }
  .note {
    margin: 0.75rem 0 0;
    font-size: 0.78rem;
    color: var(--text-subtle);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    flex-wrap: wrap;
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
