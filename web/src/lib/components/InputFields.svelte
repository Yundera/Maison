<script lang="ts">
  import type { Input } from '../stores/store'
  import { loc, t } from '../i18n'

  // The fields for an app's declared inputs (x-compose-app.inputs), shared by the
  // install dialog and the setup form. `values` is bound: keyed by variable name.
  // `set` names the secret inputs that already hold a value — their field stays
  // empty (the value never reaches the browser), and empty means "keep it".
  let {
    inputs,
    values = $bindable(),
    set = {},
  }: {
    inputs: Input[]
    values: Record<string, string>
    set?: Record<string, boolean>
  } = $props()
</script>

<div class="fields">
  {#each inputs as input (input.env)}
    <label class="field">
      <span class="label">
        {$loc(input.label, input.env)}
        {#if input.required}<span class="req" title={$t('input_required')}>*</span>{/if}
      </span>
      <input
        type={input.secret ? 'password' : 'text'}
        bind:value={values[input.env]}
        autocomplete={input.secret ? 'new-password' : 'off'}
        spellcheck="false"
        placeholder={input.secret && set[input.env] ? $t('input_keep_secret') : ''}
      />
      {#if input.description}
        <span class="hint">{$loc(input.description)}</span>
      {/if}
    </label>
  {/each}
</div>

<style>
  .fields {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .label {
    font-size: 0.85rem;
    font-weight: 600;
  }
  .req {
    color: var(--red);
    margin-left: 0.15rem;
  }
  input {
    font: inherit;
    font-size: 0.9rem;
    padding: 0.45rem 0.6rem;
    border: 1px solid var(--border);
    border-radius: 8px;
  }
  input:focus {
    outline: 2px solid var(--primary);
    outline-offset: -1px;
  }
  .hint {
    font-size: 0.78rem;
    color: var(--text-subtle);
  }
</style>
