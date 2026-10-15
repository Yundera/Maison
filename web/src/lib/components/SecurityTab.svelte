<script lang="ts">
  // One app's Security tab: change the passwords its manifest says Maison may change
  // (x-compose-app credentials[].change). The Credentials popup only shows them.
  //
  // Only credentials that declare `change` are listed. One that declares it but
  // cannot be changed (a template, or the server-wide password) says why; an
  // initial-only credential is not listed at all — it belongs to the app.
  import {
    changeCredential,
    credentialRuleViolations,
    generateCredential,
    showCredentials,
    type AppCredential,
  } from '../stores/apps'
  import { ApiError } from '../api/client'
  import { t } from '../i18n'

  let { id, name }: { id: string; name: string } = $props()

  let creds = $state<AppCredential[]>([])
  let loaded = $state(false)
  let error = $state('')

  // The form, open for at most one credential at a time (by its index).
  let editing = $state<number | null>(null)
  let draft = $state('')
  let draftShown = $state(false)
  let saving = $state(false)
  let editError = $state('')
  let serverRules = $state<string[]>([])
  let changedFor = $state<number | null>(null)

  let editingCred = $derived(creds.find((c) => c.index === editing))
  let draftRules = $derived(
    editingCred?.change && draft ? credentialRuleViolations(editingCred.change.policy, draft) : [],
  )

  async function load() {
    try {
      const r = await showCredentials(id)
      creds = r.credentials.filter((c) => c.change)
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      loaded = true
    }
  }
  load()

  function startChange(c: AppCredential) {
    editing = c.index
    draft = ''
    draftShown = false
    editError = ''
    serverRules = []
    changedFor = null
  }

  function cancelChange() {
    editing = null
    draft = ''
    editError = ''
    serverRules = []
  }

  async function regenerate() {
    if (editing === null) return
    editError = ''
    serverRules = []
    try {
      draft = await generateCredential(id, editing)
      draftShown = true
    } catch (e) {
      editError = e instanceof Error ? e.message : String(e)
    }
  }

  async function saveChange() {
    if (editing === null || saving || !draft || draftRules.length) return
    saving = true
    editError = ''
    serverRules = []
    const index = editing
    try {
      await changeCredential(id, index, draft)
      editing = null
      draft = ''
      changedFor = index
      await load()
    } catch (e) {
      if (e instanceof ApiError && Array.isArray(e.body?.rules)) serverRules = e.body.rules as string[]
      editError = e instanceof Error ? e.message : String(e)
    } finally {
      saving = false
    }
  }

  function policyHint(c: AppCredential): string {
    const p = c.change!.policy
    const parts = [$t('cred_rule_length', { min: String(p.min_length), max: String(p.max_length) })]
    for (const r of p.require ?? []) parts.push($t(`cred_rule_require_${r}`))
    if (p.charset === 'alnum') parts.push($t('cred_rule_alnum'))
    return parts.join(' · ')
  }
</script>

{#if !loaded}
  <p class="hint">{$t('loading')}</p>
{:else if error}
  <p class="error">{error}</p>
{:else if creds.length === 0}
  <p class="hint">{$t('security_none', { name })}</p>
{:else}
  <p class="hint">{$t('security_intro', { name })}</p>
  <div class="list">
    {#each creds as c (c.index)}
      <section class="cred">
        <div class="head">
          <div>
            <h3>{c.label || $t('password')}</h3>
            {#if c.username}<p class="who">{$t('username')}: <code>{c.username}</code></p>{/if}
          </div>
          {#if c.change?.changeable && editing !== c.index}
            <button class="secondary" onclick={() => startChange(c)} disabled={editing !== null || saving}>
              {$t('cred_change')}
            </button>
          {/if}
        </div>
        {#if changedFor === c.index}
          <p class="ok">{$t('cred_changed')}</p>
        {/if}
        {#if c.change && !c.change.changeable}
          <p class="note">{$t(c.change.reason === 'shared' ? 'cred_not_changeable_shared' : 'cred_not_changeable')}</p>
        {:else if c.change && editing === c.index}
          <div class="change">
            <div class="row">
              <input
                class="value"
                type={draftShown ? 'text' : 'password'}
                autocomplete="new-password"
                spellcheck="false"
                placeholder={$t('cred_new_password')}
                aria-label={$t('cred_new_password')}
                bind:value={draft}
                disabled={saving}
                oninput={() => (serverRules = [])}
              />
              <button
                class="eye"
                title={draftShown ? $t('hide_password') : $t('show_password')}
                aria-label={draftShown ? $t('hide_password') : $t('show_password')}
                onclick={() => (draftShown = !draftShown)}
              >
                {#if draftShown}
                  <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 3l18 18M10.6 10.6a2 2 0 002.8 2.8M9.9 5.1A9.8 9.8 0 0112 5c5 0 9 4.5 10 7-.4 1-1.3 2.4-2.6 3.7M6.6 6.6C4.4 8 2.7 10.2 2 12c1 2.5 5 7 10 7 1.8 0 3.4-.6 4.8-1.4" /></svg>
                {:else}
                  <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12c1-2.5 5-7 10-7s9 4.5 10 7c-1 2.5-5 7-10 7S3 14.5 2 12z" /><circle cx="12" cy="12" r="3" /></svg>
                {/if}
              </button>
              <button class="secondary" onclick={regenerate} disabled={saving}>{$t('cred_regenerate')}</button>
            </div>
            <p class="note">{policyHint(c)}</p>
            {#each [...new Set([...draftRules, ...serverRules])] as rule (rule)}
              <p class="error">{$t(`cred_rule_${rule}`, { min: String(c.change.policy.min_length), max: String(c.change.policy.max_length), forbid: c.change.policy.forbid })}</p>
            {/each}
            {#if editError && !serverRules.length}<p class="error">{editError}</p>{/if}
            <p class="note">{$t(c.change.apply === 'hook' ? 'cred_apply_hook' : 'cred_apply_env', { name })}</p>
            <div class="actions">
              <button class="ghost" onclick={cancelChange} disabled={saving}>{$t('cancel')}</button>
              <button class="primary" onclick={saveChange} disabled={saving || !draft || draftRules.length > 0}>
                {saving ? $t('cred_applying') : $t('save')}
              </button>
            </div>
          </div>
        {/if}
      </section>
    {/each}
  </div>
{/if}

<style>
  .hint {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .cred {
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 0.75rem 0.9rem;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
  }
  h3 {
    margin: 0;
    font-size: 0.9rem;
  }
  .who {
    margin: 0.2rem 0 0;
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .change {
    margin-top: 0.6rem;
    padding-top: 0.6rem;
    border-top: 1px dashed var(--border);
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .value {
    flex: 1;
    min-width: 0;
    font-size: 0.875rem;
    font-family: ui-monospace, monospace;
    background: var(--surface-sunken);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.35rem 0.5rem;
  }
  .eye {
    flex: none;
    border: none;
    background: none;
    padding: 0.2rem;
    display: grid;
    place-items: center;
    color: var(--text-muted);
  }
  .eye svg {
    width: 18px;
    height: 18px;
    fill: none;
    stroke: currentColor;
    stroke-width: 1.8;
    stroke-linecap: round;
    stroke-linejoin: round;
  }
  .note {
    margin: 0.4rem 0 0;
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .ok {
    margin: 0.4rem 0 0;
    font-size: 0.8rem;
    color: var(--green, #1a7f37);
  }
  .error {
    color: var(--red);
    font-size: 0.85rem;
    margin: 0.3rem 0 0;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.6rem;
  }
  button {
    border-radius: 8px;
    font-size: 0.8rem;
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.6;
    cursor: default;
  }
  .secondary {
    flex: none;
    border: 1px solid var(--border);
    background: var(--surface);
    padding: 0.3rem 0.7rem;
  }
  .actions button {
    padding: 0.4rem 0.9rem;
    border: none;
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
  }
</style>
