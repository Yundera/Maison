<script lang="ts">
  import {
    getConfig,
    setConfig,
    setWebUI,
    setTips,
    getServices,
    checkUpdate,
    applyUpdate,
    getEnv,
    setEnv,
    validateOverride,
    effectiveConfig,
    type WebUI,
    type AppService,
    type UpdateStatus,
    type EnvVar,
  } from '../stores/apps'
  import { BRAND } from '../brand'
  import { openStream } from '../live/stream'
  import { renderSize } from '../format'
  import OverrideForm from './OverrideForm.svelte'
  import BackupsTab from './BackupsTab.svelte'

  let {
    id,
    name,
    managed,
    initialTab,
    onclose,
  }: {
    id: string
    name: string
    managed: boolean
    initialTab?: string
    onclose: () => void
  } = $props()

  // Managed apps get the config-editing pages (incl. editable Tips, saved into
  // the override) and Backups — which is a managed-only page because it archives
  // and restores the app *folder*, and an unmanaged stack has none. Every app
  // gets logs + stats.
  type Tab =
    | 'tips'
    | 'webui'
    | 'env'
    | 'compose'
    | 'override'
    | 'update'
    | 'backups'
    | 'logs'
    | 'stats'
  const MANAGED_TABS: Tab[] = [
    'tips',
    'webui',
    'env',
    'compose',
    'override',
    'update',
    'backups',
    'logs',
    'stats',
  ]
  const UNMANAGED_TABS: Tab[] = ['logs', 'stats']
  const tabs = $derived<Tab[]>(managed ? MANAGED_TABS : UNMANAGED_TABS)
  const tabLabel: Record<Tab, string> = {
    tips: 'Tips',
    webui: 'Web UI',
    env: 'Env',
    compose: 'Compose',
    override: 'Override',
    update: 'Update',
    backups: 'Backups',
    logs: 'Logs',
    stats: 'Stats',
  }
  // Open on the deep-linked tab when one was requested and it exists for this app
  // (managed apps have the config tabs; every app has logs + stats); otherwise the
  // default first tab.
  function firstTab(): Tab {
    const wanted = initialTab as Tab | undefined
    const available = managed ? MANAGED_TABS : UNMANAGED_TABS
    if (wanted && available.includes(wanted)) return wanted
    return managed ? 'tips' : 'logs'
  }
  let tab = $state<Tab>(firstTab())

  // Two tabs, two views each, split by what you can do to them:
  //   Override — Form | YAML,      both editable, both the same override file.
  //   Compose  — Store | Effective, both read-only.
  type View = 'form' | 'yaml'
  const views: View[] = ['form', 'yaml']
  const viewLabel: Record<View, string> = { form: 'Form', yaml: 'YAML' }
  let view = $state<View>('form')

  type ComposeView = 'store' | 'effective'
  const composeViews: ComposeView[] = ['store', 'effective']
  const composeViewLabel: Record<ComposeView, string> = { store: 'Store', effective: 'Effective' }
  let composeView = $state<ComposeView>('store')

  // Effective config — `docker compose config` over base + override, loaded when
  // the view is first opened and after every save that could change it.
  let effective = $state('')
  let effectiveMsg = $state('')
  $effect(() => {
    if (tab !== 'compose' || composeView !== 'effective' || effective) return
    effectiveConfig(id)
      .then((c) => (effective = c))
      .catch((e) => (effectiveMsg = String(e)))
  })

  // Form and YAML read the same file, so a save through either has to refresh the
  // other — and the merge that the Compose tab's Effective view shows.
  async function afterOverrideSave() {
    effective = ''
    await loadConfig()
  }

  // Validate — Compose's own verdict on the override in the editor, without
  // applying it. The save path runs the same check server-side.
  let validating = $state(false)
  async function validate() {
    validating = true
    overrideMsg = ''
    try {
      const r = await validateOverride(id, override)
      overrideMsg = r.ok ? 'Valid.' : (r.error ?? 'Invalid.')
    } catch (e) {
      overrideMsg = String(e)
    } finally {
      validating = false
    }
  }

  // --- Config (base compose / override / web-UI / tips) ---
  let baseCompose = $state('')
  let override = $state('')
  let webui = $state<WebUI>({ scheme: '', host: '', port: '', path: '', url: '' })
  let tips = $state('')
  let configLoaded = $state(false)

  async function loadConfig() {
    const c = await getConfig(id)
    baseCompose = c.base
    override = c.override
    webui = c.webui
    tips = c.tips
    configLoaded = true
  }
  $effect(() => {
    if (managed && !configLoaded) loadConfig()
  })

  // Tips editor — the app's guidance, editable; saved into the override.
  let savingTips = $state(false)
  let tipsMsg = $state('')
  async function saveTips() {
    savingTips = true
    tipsMsg = ''
    try {
      await setTips(id, tips)
      tipsMsg = 'Saved.'
    } catch (e) {
      tipsMsg = String(e)
    } finally {
      savingTips = false
    }
  }
  // Override editor
  let savingOverride = $state(false)
  let overrideMsg = $state('')
  async function saveOverride() {
    savingOverride = true
    overrideMsg = ''
    try {
      await setConfig(id, override)
      overrideMsg = 'Saved & recreated.'
      effective = '' // the merge changed; the Effective view must re-read it
    } catch (e) {
      overrideMsg = String(e)
    } finally {
      savingOverride = false
    }
  }

  // --- .env editor (the app's ${VAR} interpolation vars, one KEY=VALUE per row) ---
  let envVars = $state<EnvVar[]>([])
  let envLoaded = $state(false)
  let savingEnv = $state(false)
  let envMsg = $state('')

  $effect(() => {
    if (tab !== 'env' || envLoaded) return
    getEnv(id)
      .then((v) => {
        envVars = v
        envLoaded = true
      })
      .catch((e) => (envMsg = String(e)))
  })

  // Same rules the server enforces before writing, surfaced while typing so a bad
  // row is caught here instead of coming back as a 400.
  const KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/
  const envError = $derived(
    (() => {
      const seen = new Set<string>()
      for (const v of envVars) {
        if (!KEY_RE.test(v.key)) {
          return `Invalid name "${v.key}" — letters, digits and _ only, not starting with a digit.`
        }
        if (seen.has(v.key)) return `Duplicate variable "${v.key}".`
        seen.add(v.key)
      }
      return ''
    })(),
  )

  function addEnvVar() {
    envVars = [...envVars, { key: '', value: '' }]
  }
  function removeEnvVar(i: number) {
    envVars = envVars.filter((_, n) => n !== i)
  }
  async function saveEnv() {
    savingEnv = true
    envMsg = ''
    try {
      await setEnv(id, envVars)
      envMsg = 'Saved & recreated.'
    } catch (e) {
      envMsg = String(e)
    } finally {
      savingEnv = false
    }
  }

  // --- Update (pull a fresher compose from the app's reference store) ---
  let update = $state<UpdateStatus | null>(null)
  let checkingUpdate = $state(false)
  let applyingUpdate = $state(false)
  let updateMsg = $state('')
  let updateChecked = $state(false) // one-shot: don't re-auto-check on error

  async function runCheckUpdate() {
    checkingUpdate = true
    updateChecked = true
    updateMsg = ''
    try {
      update = await checkUpdate(id)
    } catch (e) {
      updateMsg = String(e)
    } finally {
      checkingUpdate = false
    }
  }
  // Auto-check the first time the Update tab is opened.
  $effect(() => {
    if (tab === 'update' && !updateChecked) runCheckUpdate()
  })

  async function runApplyUpdate() {
    applyingUpdate = true
    updateMsg = ''
    try {
      const applied = await applyUpdate(id)
      updateMsg = applied ? 'Updated & recreated.' : 'Already up to date.'
      await runCheckUpdate() // refresh the status after applying
    } catch (e) {
      updateMsg = String(e)
    } finally {
      applyingUpdate = false
    }
  }

  // Web-UI (opening URL) form
  let savingWebui = $state(false)
  let webuiMsg = $state('')
  // A client-side approximation of the click URL (server resolves ${domain}).
  const previewUrl = $derived(
    (() => {
      const host = webui.host.trim()
      if (!host) return ''
      const scheme = webui.scheme.trim() || 'https'
      const port = webui.port.trim() ? `:${webui.port.trim()}` : ''
      let path = webui.path.trim() || '/'
      if (!path.startsWith('/')) path = '/' + path
      return `${scheme}://${host}${port}${path}`
    })(),
  )
  async function saveWebUI() {
    savingWebui = true
    webuiMsg = ''
    try {
      await setWebUI(id, {
        scheme: webui.scheme,
        host: webui.host,
        port: webui.port,
        path: webui.path,
      })
      await loadConfig() // refresh override + resolved URL
      webuiMsg = 'Saved & recreated.'
    } catch (e) {
      webuiMsg = String(e)
    } finally {
      savingWebui = false
    }
  }

  // --- Services (multi-service picker, shared by logs + stats) ---
  let services = $state<AppService[]>([])
  let selected = $state('')
  let servicesLoaded = $state(false)
  $effect(() => {
    getServices(id)
      .then((s) => {
        services = s
        if (s.length && !selected) selected = s[0].service
      })
      .catch(() => {
        services = []
      })
      .finally(() => (servicesLoaded = true))
  })
  const selectedSvc = $derived(services.find((s) => s.service === selected))

  function healthLabel(h: string): string {
    if (h === 'healthy') return 'Healthy'
    if (h === 'unhealthy') return 'Unhealthy'
    if (h === 'starting') return 'Starting…'
    return 'No health check'
  }

  // --- Logs (streamed while the logs tab is active, for the selected service) ---
  let logLines = $state<string[]>([])
  $effect(() => {
    if (tab !== 'logs' || !selected) return
    const svc = selected
    logLines = []
    const close = openStream(
      `/api/apps/${encodeURIComponent(id)}/logs?service=${encodeURIComponent(svc)}`,
      (line) => {
        logLines = [...logLines.slice(-500), line]
      },
    )
    return close
  })

  // --- Stats (streamed while the stats tab is active, for the selected service) ---
  let stat = $state<{
    cpu_percent: number
    mem_usage: number
    mem_limit: number
    health: string
  } | null>(null)
  $effect(() => {
    if (tab !== 'stats' || !selected) return
    const svc = selected
    stat = null
    const close = openStream(
      `/api/apps/${encodeURIComponent(id)}/stats?service=${encodeURIComponent(svc)}`,
      (raw) => {
        try {
          stat = JSON.parse(raw)
        } catch {
          /* ignore */
        }
      },
    )
    return close
  })
  // Prefer the live health from the stats frame, fall back to the services list.
  const liveHealth = $derived(stat?.health || selectedSvc?.health || '')
</script>

<div class="backdrop" onclick={onclose} role="presentation">
  <div class="modal" onclick={(e) => e.stopPropagation()} role="presentation">
    <header>
      <h2>{name}</h2>
      <button class="close" aria-label="Close" onclick={onclose}>✕</button>
    </header>

    <nav class="tabs">
      {#each tabs as t (t)}
        <button class:active={tab === t} onclick={() => (tab = t)}>{tabLabel[t]}</button>
      {/each}
    </nav>

    <div class="content">
      {#if tab === 'tips'}
        {#if !configLoaded}
          <p class="hint">Loading…</p>
        {:else}
          <p class="hint">
            <strong>Tips</strong> for this app — setup guidance, credentials reminders, reverse-proxy
            details, whatever you need. Seeded from the store and freely editable; saving writes them
            into the <code>docker-compose.override.yml</code>. <code>{'${VAR}'}</code> references are
            shown as-is here; use the tile's <em>Tips</em> menu to see them with values filled in.
          </p>
          <textarea
            bind:value={tips}
            spellcheck="false"
            placeholder="Write tips for this app…"
          ></textarea>
          <div class="actions">
            <span class="msg">{tipsMsg}</span>
            <button class="primary" disabled={savingTips} onclick={saveTips}>
              {savingTips ? 'Saving…' : 'Save tips'}
            </button>
          </div>
        {/if}
      {:else if tab === 'webui'}
        <p class="hint">
          The <strong>opening URL</strong> is where the tile's <em>Open</em> button points. This is
          a friendlier shortcut for the <code>x-compose-app</code> web-UI fields — saving writes them
          into the <code>docker-compose.override.yml</code>.
        </p>
        <div class="form">
          <label>
            <span>Scheme</span>
            <select bind:value={webui.scheme}>
              <option value="">https (default)</option>
              <option value="https">https</option>
              <option value="http">http</option>
            </select>
          </label>
          <label>
            <span>Host</span>
            <input
              bind:value={webui.host}
              spellcheck="false"
              placeholder={`${id}-\${domain}`}
            />
          </label>
          <label>
            <span>Port</span>
            <input bind:value={webui.port} spellcheck="false" placeholder="(none — 443 via gateway)" />
          </label>
          <label>
            <span>Path</span>
            <input bind:value={webui.path} spellcheck="false" placeholder="/" />
          </label>
        </div>
        <p class="hint">
          <code>{'${domain}'}</code> resolves to the deployment domain when the app is brought up.
          {#if webui.url}
            Current URL: <a href={webui.url} target="_blank" rel="noopener">{webui.url}</a>
          {:else if previewUrl}
            Preview: <span class="mono">{previewUrl}</span>
          {:else}
            No opening URL set — the tile has no <em>Open</em> action.
          {/if}
        </p>
        <div class="actions">
          <span class="msg">{webuiMsg}</span>
          <button class="primary" disabled={savingWebui} onclick={saveWebUI}>
            {savingWebui ? 'Saving…' : 'Save & recreate'}
          </button>
        </div>
      {:else if tab === 'env'}
        <p class="hint">
          The app's <code>.env</code> — the variables its <code>docker-compose.yml</code> resolves
          <code>{'${VAR}'}</code> against. Prefilled at install (<code>PUID</code>,
          <code>DATA_ROOT</code>, <code>REF_*</code>, …) and yours to edit. This is not a service's
          <code>environment:</code> block — for that, edit the override. Saving recreates the stack,
          because Compose only reads <code>.env</code> when it brings the app up.
        </p>
        {#if !envLoaded && !envMsg}
          <p class="hint">Loading…</p>
        {:else}
          <div class="env-list">
            {#each envVars as v, i (i)}
              <div class="env-row">
                <input
                  bind:value={v.key}
                  spellcheck="false"
                  placeholder="KEY"
                  aria-label="Variable name"
                  class:bad={v.key !== '' && !KEY_RE.test(v.key)}
                />
                <input
                  bind:value={v.value}
                  spellcheck="false"
                  placeholder="value"
                  aria-label="Value for {v.key || 'new variable'}"
                />
                <button
                  class="remove"
                  aria-label="Remove {v.key || 'variable'}"
                  onclick={() => removeEnvVar(i)}>✕</button
                >
              </div>
            {:else}
              <p class="hint">No variables — this app's <code>.env</code> is empty.</p>
            {/each}
          </div>
          <div class="actions">
            <span class="msg">{envError || envMsg}</span>
            <button onclick={addEnvVar}>Add variable</button>
            <button
              class="primary"
              disabled={savingEnv || !!envError}
              onclick={saveEnv}
            >
              {savingEnv ? 'Saving…' : 'Save & recreate'}
            </button>
          </div>
        {/if}
      {:else if tab === 'compose'}
        <div class="views">
          {#each composeViews as v (v)}
            <button class:active={composeView === v} onclick={() => (composeView = v)}>
              {composeViewLabel[v]}
            </button>
          {/each}
        </div>

        {#if composeView === 'store'}
          <p class="hint">
            The strict <code>docker-compose.yml</code> as shipped by the store —
            <strong>read-only</strong>. {BRAND} never modifies it, so updates stay clean; your
            changes live in the <strong>Override</strong> tab instead.
          </p>
          <pre class="code readonly">{baseCompose || (configLoaded ? '(empty)' : 'Loading…')}</pre>
        {:else}
          <p class="hint">
            What Compose actually runs: the store's compose and your override merged, with every
            <code>{'${VAR}'}</code> resolved from the <code>.env</code> —
            <strong>read-only</strong>. Read this when an override doesn't do what you expected: it
            shows which of the store's values survived and which yours replaced.
          </p>
          <pre class="code readonly">{effective || effectiveMsg || 'Loading…'}</pre>
        {/if}
      {:else if tab === 'override'}
        <div class="views">
          {#each views as v (v)}
            <button class:active={view === v} onclick={() => (view = v)}>{viewLabel[v]}</button>
          {/each}
        </div>

        {#if view === 'form'}
          <OverrideForm {id} onsaved={afterOverrideSave} />
        {:else}
          <p class="hint">
            Your edits, layered on top via Compose override semantics. The running stack is
            <code>docker-compose.yml</code> + this override. The <strong>Form</strong> view writes
            this same file — anything it can't express (long-syntax ports, a list-form command)
            belongs here.
          </p>
          <textarea
            bind:value={override}
            spellcheck="false"
            placeholder={'services:\n  app:\n    ports:\n      - "8080:80"'}
          ></textarea>
          <div class="actions">
            <span class="msg">{overrideMsg}</span>
            <button disabled={validating || savingOverride} onclick={validate}>
              {validating ? 'Checking…' : 'Validate'}
            </button>
            <button class="primary" disabled={savingOverride} onclick={saveOverride}>
              {savingOverride ? 'Saving…' : 'Save & recreate'}
            </button>
          </div>
        {/if}
      {:else if tab === 'update'}
        <p class="hint">
          Pulls a fresher <code>docker-compose.yml</code> from the store this app was installed
          from and re-applies it (<code>docker compose up -d</code>). Your
          <code>docker-compose.override.yml</code> and <code>.env</code> are left untouched.
        </p>
        {#if checkingUpdate && update === null}
          <p class="hint">Checking the store…</p>
        {:else if update && !update.has_ref}
          <p class="hint">
            This app has no store reference recorded, so {BRAND} can't check for updates.
            Reinstall it from the store to enable updates.
          </p>
        {:else if update}
          <div class="update-box">
            <div class="update-row">
              <span class="k">Reference store</span>
              <span class="v mono">{update.store || '(merged catalog)'}</span>
            </div>
            <div class="update-row">
              <span class="k">Store app</span>
              <span class="v mono">{update.store_app_id}</span>
            </div>
            <div class="update-row">
              <span class="k">Status</span>
              {#if update.error}
                <span class="v badge health-unhealthy">Couldn't check</span>
              {:else if update.available}
                <span class="v badge health-starting">Update available</span>
              {:else}
                <span class="v badge health-healthy">Up to date</span>
              {/if}
            </div>
            {#if update.error}
              <p class="hint">{update.error}</p>
            {/if}
          </div>
        {/if}
        <div class="actions">
          <span class="msg">{updateMsg}</span>
          <button
            disabled={checkingUpdate || applyingUpdate}
            onclick={runCheckUpdate}
          >
            {checkingUpdate ? 'Checking…' : 'Check again'}
          </button>
          <button
            class="primary"
            disabled={applyingUpdate || checkingUpdate || !update?.available}
            onclick={runApplyUpdate}
          >
            {applyingUpdate ? 'Updating…' : 'Update now'}
          </button>
        </div>
      {:else if tab === 'backups'}
        <BackupsTab {id} />
      {:else if tab === 'logs'}
        {@render servicePicker()}
        {#if !servicesLoaded}
          <p class="hint">Loading…</p>
        {:else if !services.length}
          <p class="hint">No running containers — start the app to see logs.</p>
        {:else}
          <pre class="logs">{logLines.join('\n')}</pre>
        {/if}
      {:else if tab === 'stats'}
        {@render servicePicker()}
        {#if !servicesLoaded}
          <p class="hint">Loading…</p>
        {:else if !services.length}
          <p class="hint">No running containers — start the app to see stats.</p>
        {:else}
          <div class="stats">
            <div class="stat health">
              <span class="k">Health</span>
              <span class="badge health-{liveHealth || 'none'}">{healthLabel(liveHealth)}</span>
            </div>
            {#if stat}
              <div class="stat">
                <span class="k">CPU</span>
                <div class="bar">
                  <div class="fill" style:width={`${Math.min(100, stat.cpu_percent)}%`}></div>
                </div>
                <span class="v">{stat.cpu_percent}%</span>
              </div>
              <div class="stat">
                <span class="k">Memory</span>
                <div class="bar">
                  <div
                    class="fill"
                    style:width={`${stat.mem_limit ? Math.min(100, (stat.mem_usage / stat.mem_limit) * 100) : 0}%`}
                  ></div>
                </div>
                <span class="v">{renderSize(stat.mem_usage)}</span>
              </div>
            {:else}
              <p class="hint">Waiting for stats…</p>
            {/if}
          </div>
        {/if}
      {/if}
    </div>
  </div>
</div>

{#snippet servicePicker()}
  {#if services.length > 1}
    <div class="picker">
      <label for="svc-select">Service</label>
      <select id="svc-select" bind:value={selected}>
        {#each services as s (s.service)}
          <option value={s.service}>
            {s.service}{s.health ? ` · ${s.health}` : ''}{s.state !== 'running' ? ` · ${s.state}` : ''}
          </option>
        {/each}
      </select>
    </div>
  {/if}
{/snippet}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 95;
    background: var(--scrim);
    display: grid;
    place-items: center;
  }
  .modal {
    width: min(94vw, 820px);
    height: min(88vh, 680px);
    background: var(--surface);
    backdrop-filter: blur(14px);
    border-radius: 14px;
    padding: 1.1rem 1.35rem;
    color: var(--text);
    display: flex;
    flex-direction: column;
  }
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  h2 {
    margin: 0;
    font-size: 1.15rem;
  }
  .close {
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 1.1rem;
  }
  .tabs {
    display: flex;
    gap: 0.25rem;
    margin: 0.75rem 0;
    border-bottom: 1px solid var(--border);
  }
  .tabs button {
    background: none;
    border: none;
    color: var(--text-muted);
    padding: 0.5rem 0.9rem;
    border-bottom: 2px solid transparent;
    font-size: 0.9rem;
  }
  .tabs button.active {
    color: var(--text);
    border-bottom-color: var(--primary);
  }
  .content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  /* Form / YAML / Effective — three views of the same override, right-aligned so
     the switch reads as a mode toggle rather than another row of tabs. */
  .views {
    display: flex;
    align-self: flex-end;
    gap: 0.15rem;
    padding: 0.15rem;
    margin-bottom: 0.6rem;
    background: var(--surface-sunken);
    border-radius: 8px;
  }
  .views button {
    background: none;
    border: none;
    color: var(--text-muted);
    border-radius: 6px;
    padding: 0.3rem 0.8rem;
    font-size: 0.78rem;
  }
  .views button.active {
    background: var(--surface-3);
    color: var(--text);
  }
  .hint {
    color: var(--text-muted);
    font-size: 0.8rem;
    margin: 0 0 0.5rem;
  }
  .hint a {
    color: var(--primary);
  }
  code {
    background: var(--surface-2);
    padding: 0 0.25rem;
    border-radius: 4px;
  }
  .mono {
    font-family: ui-monospace, monospace;
  }
  /* Web-UI form */
  .form {
    display: grid;
    gap: 0.6rem;
    margin-bottom: 0.5rem;
  }
  .form label {
    display: grid;
    grid-template-columns: 5rem 1fr;
    align-items: center;
    gap: 0.75rem;
  }
  .form label span {
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .form input,
  .form select {
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.5rem 0.6rem;
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
  }
  /* .env editor */
  .env-list {
    flex: 1;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    padding-right: 0.25rem;
  }
  .env-row {
    display: grid;
    grid-template-columns: 14rem 1fr 2rem;
    align-items: center;
    gap: 0.5rem;
  }
  .env-row input {
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.45rem 0.6rem;
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
    min-width: 0;
  }
  .env-row input.bad {
    border-color: var(--red);
  }
  .env-row .remove {
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 0.85rem;
    padding: 0.25rem;
    border-radius: 6px;
  }
  .env-row .remove:hover {
    color: var(--red);
    background: var(--surface-2);
  }
  textarea {
    flex: 1;
    min-height: 8rem;
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.6rem;
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
    resize: none;
  }
  .code {
    flex: 1;
    overflow: auto;
    background: var(--surface-sunken);
    padding: 0.6rem;
    border-radius: 8px;
    font-size: 0.75rem;
    margin: 0;
    white-space: pre;
  }
  .code.readonly {
    border: 1px solid var(--border);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 0.75rem;
    margin-top: 0.5rem;
  }
  .msg {
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
    border: none;
    border-radius: 8px;
    padding: 0.5rem 1.1rem;
    font-size: 0.875rem;
  }
  .actions button:not(.primary) {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.5rem 1.1rem;
    font-size: 0.875rem;
  }
  .actions button:disabled {
    opacity: 0.5;
  }
  /* Update tab */
  .update-box {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem 0.9rem;
    margin-bottom: 0.5rem;
  }
  .update-row {
    display: grid;
    grid-template-columns: 8rem 1fr;
    align-items: center;
    gap: 0.75rem;
  }
  .update-row .k {
    color: var(--text-muted);
    font-size: 0.8rem;
  }
  .update-row .v {
    font-size: 0.82rem;
    word-break: break-all;
  }
  /* Service picker */
  .picker {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    margin-bottom: 0.6rem;
  }
  .picker label {
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .picker select {
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.35rem 0.6rem;
    font-size: 0.8rem;
  }
  .logs {
    flex: 1;
    overflow: auto;
    background: var(--surface-sunken);
    border-radius: 8px;
    padding: 0.6rem;
    font-family: ui-monospace, monospace;
    font-size: 0.75rem;
    white-space: pre-wrap;
    word-break: break-all;
    margin: 0;
  }
  .stats {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    padding-top: 0.5rem;
  }
  .stat {
    display: grid;
    grid-template-columns: 4rem 1fr 6rem;
    align-items: center;
    gap: 0.75rem;
  }
  .stat.health {
    grid-template-columns: 4rem 1fr;
  }
  .stat .k {
    color: var(--text-muted);
    font-size: 0.85rem;
  }
  .stat .v {
    text-align: right;
    font-size: 0.85rem;
  }
  .badge {
    justify-self: start;
    font-size: 0.75rem;
    padding: 0.15rem 0.55rem;
    border-radius: 999px;
    border: 1px solid transparent;
  }
  .health-healthy {
    color: var(--status-running);
    border-color: var(--status-running);
  }
  .health-unhealthy {
    color: var(--red);
    border-color: var(--red);
  }
  .health-starting {
    color: var(--yellow);
    border-color: var(--yellow);
  }
  .health-none {
    color: var(--text-muted);
    border-color: var(--border);
  }
  .bar {
    height: 8px;
    background: var(--surface-3);
    border-radius: 4px;
    overflow: hidden;
  }
  .fill {
    height: 100%;
    background: var(--primary);
    transition: width 0.4s;
  }
</style>
