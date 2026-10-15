<script lang="ts">
  import { credentialsApp } from '../stores/ui'
  import {
    changeCredential,
    credentialRuleViolations,
    generateCredential,
    openApp,
    setCredentialsDismissed,
    showCredentials,
    type App,
    type AppCredential,
  } from '../stores/apps'
  import { ApiError } from '../api/client'
  import { t } from '../i18n'

  let { target }: { target: { app: App; launch: boolean } } = $props()

  let creds = $state<AppCredential[]>([])
  let dismissed = $state(false)
  let loaded = $state(false)
  let error = $state('')
  let revealed = $state<Record<number, boolean>>({})
  let copied = $state('')
  let initial = false

  // The change form, open for at most one credential at a time (by its index).
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
  let anyInitial = $derived(creds.some((c) => !c.change?.changeable))

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
      draft = await generateCredential(target.app.id, editing)
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
      await changeCredential(target.app.id, index, draft)
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

  async function load() {
    try {
      const r = await showCredentials(target.app.id)
      creds = r.credentials
      dismissed = initial = r.dismissed
    } catch (e) {
      error = String(e)
    } finally {
      loaded = true
    }
  }
  load()

  // Persisted when the modal closes, not on every toggle, so a misclick that is
  // undone never reaches the server — and only when it changed.
  async function persist() {
    if (dismissed === initial) return
    try {
      await setCredentialsDismissed(target.app.id, dismissed)
    } catch {
      // Best-effort: the worst case is seeing the credentials once more.
    }
  }

  function close() {
    if (saving) return // the change is applying; closing would hide its outcome
    if (loaded && !error) persist()
    credentialsApp.set(null)
  }

  // Synchronous inside the button's own click, like the tile's: that is what keeps
  // a new-tab open clear of the popup blocker.
  function launch(ev: MouseEvent) {
    const app = target.app
    close()
    openApp(app, ev)
  }

  // The Clipboard API exists only in a secure context, and a box is often reached
  // over plain HTTP on the LAN — fall back to the legacy selection copy there.
  async function copy(value: string, key: string) {
    let ok = false
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(value)
        ok = true
      }
    } catch {
      ok = false
    }
    if (!ok) {
      const ta = document.createElement('textarea')
      ta.value = value
      ta.setAttribute('readonly', '')
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      try {
        ok = document.execCommand('copy')
      } catch {
        ok = false
      }
      ta.remove()
    }
    if (ok) {
      copied = key
      setTimeout(() => {
        if (copied === key) copied = ''
      }, 1500)
    }
  }
</script>

<div class="backdrop" onclick={close} role="presentation">
  <div class="dialog" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('credentials_title')} — {target.app.name}</h2>
    {#if !loaded}
      <p class="hint">{$t('loading')}</p>
    {:else if error}
      <p class="error">{error}</p>
    {:else if creds.length === 0}
      <p class="hint">{$t('credentials_none')}</p>
    {:else}
      <p class="intro">{$t('credentials_intro', { name: target.app.name })}</p>
      <div class="list">
        {#each creds as c, i (c.index)}
          <section class="cred">
            {#if c.label}<h3>{c.label}</h3>{/if}
            {#if c.username}
              <div class="row">
                <span class="key">{$t('username')}</span>
                <code class="value">{c.username}</code>
                <button class="copy" onclick={() => copy(c.username ?? '', `u${i}`)}>
                  {copied === `u${i}` ? $t('copied') : $t('copy')}
                </button>
              </div>
            {/if}
            {#if c.password}
              <div class="row">
                <span class="key">{$t('password')}</span>
                <code class="value">{revealed[i] ? c.password : '•'.repeat(Math.min(c.password.length, 16))}</code>
                <button
                  class="eye"
                  title={revealed[i] ? $t('hide_password') : $t('show_password')}
                  aria-label={revealed[i] ? $t('hide_password') : $t('show_password')}
                  onclick={() => (revealed[i] = !revealed[i])}
                >
                  {#if revealed[i]}
                    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 3l18 18M10.6 10.6a2 2 0 002.8 2.8M9.9 5.1A9.8 9.8 0 0112 5c5 0 9 4.5 10 7-.4 1-1.3 2.4-2.6 3.7M6.6 6.6C4.4 8 2.7 10.2 2 12c1 2.5 5 7 10 7 1.8 0 3.4-.6 4.8-1.4" /></svg>
                  {:else}
                    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12c1-2.5 5-7 10-7s9 4.5 10 7c-1 2.5-5 7-10 7S3 14.5 2 12z" /><circle cx="12" cy="12" r="3" /></svg>
                  {/if}
                </button>
                <button class="copy" onclick={() => copy(c.password ?? '', `p${i}`)}>
                  {copied === `p${i}` ? $t('copied') : $t('copy')}
                </button>
              </div>
            {/if}
            {#if c.note}<p class="note">{c.note}</p>{/if}
            {#if changedFor === c.index}
              <p class="ok">{$t('cred_changed')}</p>
            {/if}
            {#if c.change && !c.change.changeable}
              <p class="note">{$t(c.change.reason === 'shared' ? 'cred_not_changeable_shared' : 'cred_not_changeable')}</p>
            {:else if c.change && editing !== c.index}
              <button class="link" onclick={() => startChange(c)} disabled={editing !== null || saving}>
                {$t('cred_change')}
              </button>
            {:else if c.change && editing === c.index}
              <div class="change">
                <div class="row">
                  <span class="key">{$t('cred_new_password')}</span>
                  <input
                    class="value"
                    type={draftShown ? 'text' : 'password'}
                    autocomplete="new-password"
                    spellcheck="false"
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
                  <button class="copy" onclick={regenerate} disabled={saving}>{$t('cred_regenerate')}</button>
                </div>
                <p class="note">{policyHint(c)}</p>
                {#each [...new Set([...draftRules, ...serverRules])] as rule (rule)}
                  <p class="error">{$t(`cred_rule_${rule}`, { min: String(c.change.policy.min_length), max: String(c.change.policy.max_length), forbid: c.change.policy.forbid })}</p>
                {/each}
                {#if editError && !serverRules.length}<p class="error">{editError}</p>{/if}
                <p class="note">{$t(c.change.apply === 'hook' ? 'cred_apply_hook' : 'cred_apply_env', { name: target.app.name })}</p>
                <div class="change-actions">
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
      {#if anyInitial}<p class="hint">{$t('credentials_initial_hint')}</p>{/if}
      <label class="dismiss">
        <input type="checkbox" bind:checked={dismissed} />
        {$t('dont_show_again')}
      </label>
    {/if}
    <div class="actions">
      <button class="ghost" onclick={close}>{target.launch ? $t('close') : $t('back')}</button>
      {#if target.launch}
        <button class="primary" onclick={launch}>{$t('open_app')}</button>
      {/if}
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
    overflow-y: auto;
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
  h3 {
    margin: 0 0 0.4rem;
    font-size: 0.9rem;
  }
  .intro {
    margin: 0 0 0.75rem;
    font-size: 0.9rem;
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .cred {
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 0.7rem 0.8rem;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.25rem 0;
  }
  .key {
    width: 6.5rem;
    flex: none;
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .value {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
    font-size: 0.875rem;
    background: var(--surface-sunken);
    border-radius: 6px;
    padding: 0.3rem 0.5rem;
  }
  .copy {
    flex: none;
    border: 1px solid var(--border);
    background: var(--surface);
    border-radius: 6px;
    padding: 0.25rem 0.6rem;
    font-size: 0.8rem;
    min-width: 4.5rem;
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
  .hint {
    margin: 0.75rem 0 0;
    color: var(--text-subtle);
    font-size: 0.8rem;
  }
  .dismiss {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    margin-top: 0.75rem;
    font-size: 0.875rem;
  }
  .ok {
    margin: 0.4rem 0 0;
    font-size: 0.8rem;
    color: var(--green, #1a7f37);
  }
  .link {
    margin-top: 0.4rem;
    border: none;
    background: none;
    padding: 0;
    color: var(--primary);
    font-size: 0.8rem;
    cursor: pointer;
  }
  .link:disabled {
    color: var(--text-subtle);
    cursor: default;
  }
  .change {
    margin-top: 0.5rem;
    padding-top: 0.5rem;
    border-top: 1px dashed var(--border);
  }
  input.value {
    border: 1px solid var(--border);
    font-family: ui-monospace, monospace;
  }
  .change .error {
    margin-top: 0.3rem;
  }
  .change-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.6rem;
  }
  .change-actions button {
    padding: 0.4rem 0.9rem;
    border-radius: 8px;
    border: none;
    font-size: 0.8rem;
  }
  .change-actions button:disabled {
    opacity: 0.6;
  }
  .error {
    color: var(--red);
    font-size: 0.85rem;
    margin: 0;
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
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
  }
</style>
