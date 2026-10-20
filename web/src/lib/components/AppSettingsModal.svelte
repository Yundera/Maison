<script lang="ts">
  import {
    getConfig,
    getOverrideForm,
    renderOverrideForm,
    parseOverrideForm,
    saveSettings,
    setTips,
    renderTips,
    getServices,
    checkUpdate,
    applyUpdate,
    setUpdateRef,
    getEnv,
    validateOverride,
    effectiveConfig,
    type OverrideForm as Form,
    type AppService,
    type UpdateStatus,
    type EnvEntry,
    type EnvSource,
    apps,
  } from '../stores/apps'
  import { ApiError } from '../api/client'
  import { BRAND } from '../brand'
  import { loc, type Localized } from '../i18n'
  import { bare, isAddressableRef } from '../storeref'
  import { fetchStoreSources, type StoreSource } from '../stores/store'
  import { openStream } from '../live/stream'
  import { renderSize } from '../format'
  import OverrideForm, { normalizeForm } from './OverrideForm.svelte'
  import TipsView from './TipsView.svelte'
  import BackupsTab from './BackupsTab.svelte'
  import SecurityTab from './SecurityTab.svelte'
  import AppStatsHistory from './AppStatsHistory.svelte'

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
  type Tab = 'tips' | 'config' | 'update' | 'backups' | 'security' | 'logs' | 'stats'
  const MANAGED_TABS: Tab[] = ['tips', 'config', 'update', 'backups', 'security', 'logs', 'stats']
  const UNMANAGED_TABS: Tab[] = ['logs', 'stats']
  // An app that declares `lifecycle.updatable: false` is updated by something else
  // (its platform installer); the backend refuses its update routes, so no tab.
  // Security changes the app's declared credentials, so an app without any has none.
  const updatable = $derived($apps.find((a) => a.id === id)?.updatable !== false)
  const hasCredentials = $derived(!!$apps.find((a) => a.id === id)?.has_credentials)
  const tabsFor = (isManaged: boolean, isUpdatable: boolean, hasCreds: boolean): Tab[] =>
    !isManaged
      ? UNMANAGED_TABS
      : MANAGED_TABS.filter((t) => (t !== 'update' || isUpdatable) && (t !== 'security' || hasCreds))
  const tabs = $derived<Tab[]>(tabsFor(managed, updatable, hasCredentials))
  const tabLabel: Record<Tab, string> = {
    tips: 'Tips',
    config: 'Config',
    update: 'Update',
    backups: 'Backups',
    security: 'Security',
    logs: 'Logs',
    stats: 'Stats',
  }

  // The Config tab is what Compose runs, in two sections: the compose (store +
  // override — Form | YAML) and the .env its ${VAR}s resolve against. They used to
  // be four tabs (Web UI, Env, Compose, Override); a deep link to one of those
  // lands on the section that replaced it.
  type Section = 'compose' | 'env'
  type View = 'form' | 'yaml'
  const LEGACY: Record<string, { section: Section; view: View }> = {
    webui: { section: 'compose', view: 'form' },
    override: { section: 'compose', view: 'form' },
    compose: { section: 'compose', view: 'yaml' },
    env: { section: 'env', view: 'form' },
  }

  // Open on the deep-linked tab when one was requested and it exists for this app
  // (managed apps have the config tabs; every app has logs + stats); otherwise the
  // default first tab.
  function firstTab(): Tab {
    const wanted = (initialTab && LEGACY[initialTab] ? 'config' : initialTab) as Tab | undefined
    const app = $apps.find((a) => a.id === id)
    const available = tabsFor(managed, app?.updatable !== false, !!app?.has_credentials)
    if (wanted && available.includes(wanted)) return wanted
    return managed ? 'tips' : 'logs'
  }
  let tab = $state<Tab>(firstTab())
  const legacy = () => (initialTab ? LEGACY[initialTab] : undefined)
  let section = $state<Section>(legacy()?.section ?? 'compose')
  let view = $state<View>(legacy()?.view ?? 'form')
  const sectionLabel: Record<Section, string> = { compose: 'Compose', env: 'Env' }
  const viewLabel: Record<View, string> = { form: 'Form', yaml: 'YAML' }

  // --- Compose: Form and YAML are two views of one file, the override ---------
  //
  // `override` is the YAML view's draft of the file. The form is built from a
  // draft too (`formFor`; null = the file on disk), and its edits sit on top of
  // that draft: switching to YAML folds them into the text (server-side, with the
  // same code a save runs), switching back rebuilds the form from the text if it
  // moved. A save sends the draft if it differs from the file, and the form's
  // edits if it has any — the server applies them onto the draft, so an edit the
  // form can't show (a healthcheck typed in YAML) is never dropped by one it can.
  let form = $state<Form | null>(null)
  let formBase = $state('') // JSON of the form as built from formFor, for dirtiness
  let formDisk = $state('') // JSON of the form as built from the file, for Discard
  let formFor = $state<string | null>(null)
  let formErr = $state('')
  async function loadForm() {
    try {
      const f = normalizeForm(await getOverrideForm(id))
      formDisk = formBase = JSON.stringify(f)
      formFor = null
      form = f
      formErr = ''
    } catch (e) {
      formErr = String(e)
    }
  }
  const formDirty = $derived(!!form && JSON.stringify(form) !== formBase)

  let switching = $state(false)
  let configMsg = $state('')
  async function switchView(v: View) {
    if (v === view || switching) return
    configMsg = ''
    switching = true
    try {
      if (v === 'yaml' && formDirty && form) {
        override = await renderOverrideForm(id, $state.snapshot(form) as Form, override)
        formBase = JSON.stringify(form)
        formFor = override
      } else if (v === 'form' && override !== (formFor ?? savedOverride)) {
        const f = normalizeForm(await parseOverrideForm(id, override))
        formBase = JSON.stringify(f)
        formFor = override
        form = f
      }
      view = v
    } catch (e) {
      // YAML the form can't read: stay on it, with the parser's message.
      configMsg = e instanceof Error ? e.message : String(e)
    } finally {
      switching = false
    }
  }

  // The merge of both files — `docker compose config`, variables as written unless
  // "Resolve variables" is ticked. Loaded when the disclosure is first opened, and
  // dropped after every save.
  let mergedOpen = $state(false)
  let resolveVars = $state(false)
  let merged = $state('')
  let mergedMsg = $state('')
  $effect(() => {
    if (tab !== 'config' || view !== 'yaml' || !mergedOpen || merged || mergedMsg) return
    effectiveConfig(id, resolveVars)
      .then((c) => (merged = c))
      .catch((e) => (mergedMsg = String(e)))
  })
  function toggleResolve() {
    resolveVars = !resolveVars
    merged = ''
    mergedMsg = ''
  }

  // Validate — Compose's own verdict on the override in the editor, without
  // applying it. The save path runs the same check server-side.
  let validating = $state(false)
  async function validate() {
    validating = true
    configMsg = ''
    try {
      const r = await validateOverride(id, override)
      configMsg = r.ok ? 'Valid.' : (r.error ?? 'Invalid.')
    } catch (e) {
      configMsg = String(e)
    } finally {
      validating = false
    }
  }

  // --- Config (base compose / override / web-UI / tips) ---
  let baseCompose = $state('')
  let override = $state('')
  let tips = $state('')
  let configLoaded = $state(false)
  let savedOverride = $state('') // the override as last loaded, for the YAML view's dirtiness
  const yamlDirty = $derived(configLoaded && override !== savedOverride)

  // keepEdits: a reload after a tips save must not throw away YAML being edited.
  async function loadConfig(keepEdits = false) {
    const c = await getConfig(id)
    baseCompose = c.base
    if (!keepEdits || override === savedOverride) override = c.override
    savedOverride = c.override
    // The editor works on one text: the operator's locale, else en_us. Saving
    // stores it as the override's en_us, which every locale then falls back to.
    tips = $loc(c.tips)
    savedTips = tips
    configLoaded = true
  }
  $effect(() => {
    if (managed && !configLoaded) loadConfig()
  })

  // Tips — the app's guidance. The page opens on the rendered tips (Markdown, with
  // ${VAR} references filled in by the server), because that is what the operator
  // came to read — a service's tile click lands here. Edit swaps in the raw source;
  // saving writes it into the override and goes back to the rendered view.
  let savedTips = $state('') // the source as last loaded or saved, for Cancel
  let renderedAll = $state<Localized>({})
  const renderedTips = $derived($loc(renderedAll))
  let renderedLoaded = $state(false)
  let editingTips = $state(false)
  let savingTips = $state(false)
  let tipsMsg = $state('')
  async function loadRenderedTips() {
    try {
      renderedAll = await renderTips(id)
    } catch (e) {
      tipsMsg = String(e)
    } finally {
      renderedLoaded = true
    }
  }
  $effect(() => {
    if (managed && tab === 'tips' && !renderedLoaded) loadRenderedTips()
  })
  function editTips() {
    tipsMsg = ''
    editingTips = true
  }
  function cancelTips() {
    tips = savedTips
    tipsMsg = ''
    editingTips = false
  }
  async function saveTips() {
    savingTips = true
    tipsMsg = ''
    try {
      await setTips(id, tips)
      savedTips = tips
      // Tips live in the override, so the Config tab's copy is stale now — reload
      // it, unless it carries edits of its own (a form's edits patch the file, so
      // they keep the new tips; a YAML draft would need re-reading by hand).
      const editing = formDirty || override !== savedOverride
      await loadConfig(true)
      if (!editing) await loadForm()
      await loadRenderedTips()
      editingTips = false
      tipsMsg = 'Saved.'
    } catch (e) {
      tipsMsg = String(e)
    } finally {
      savingTips = false
    }
  }
  // --- .env editor (the app's ${VAR} interpolation vars, one KEY=VALUE per row) ---
  let envVars = $state<EnvEntry[]>([])
  let envSaved = $state('') // JSON as last loaded, for dirtiness
  let envLoaded = $state(false)
  let envMsg = $state('')
  async function loadEnv() {
    try {
      const v = await getEnv(id)
      envSaved = JSON.stringify(v)
      envVars = v
      envLoaded = true
    } catch (e) {
      envMsg = String(e)
    }
  }
  const envDirty = $derived(envLoaded && JSON.stringify(envVars) !== envSaved)

  // The whole Config tab loads together the first time it is opened: the save
  // below covers all of it, and a dirty marker on a section never shown would be
  // a lie.
  $effect(() => {
    if (tab !== 'config' || !managed) return
    if (!form && !formErr) loadForm()
    if (!envLoaded && !envMsg) loadEnv()
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

  // Every sourced row is locked: a maison or deployment row is put back by the
  // start a save triggers, and a seeded or secret one is part of the app's data.
  // The server enforces the same rule; read-only here only says so up front.
  const envLocked = (v: EnvEntry) => !!v.source
  const envSourceLabel: Record<EnvSource, string> = {
    maison: 'Set by ' + BRAND,
    deployment: 'From the box',
    seeded: 'Set once',
    secret: 'Secret',
  }
  const envSourceTitle: Record<EnvSource, string> = {
    maison: `${BRAND} computes this one and writes it back on every start — edit it here and it reverts.`,
    deployment: `Forwarded from the box's .env.app and written back on every start — change it there, not here.`,
    seeded: `Written from the box's .env.app when the app was installed and never refreshed. The app has already used it (an admin password, a database password), so it is hidden and locked here — changing it would not change it inside the app.`,
    secret: `Generated for this app when it was installed and part of its data from then on — changing it here would break the app. Hidden and locked; change a login password from Security, if the app allows it.`,
  }

  function addEnvVar() {
    envVars = [...envVars, { key: '', value: '' }]
  }
  function removeEnvVar(i: number) {
    envVars = envVars.filter((_, n) => n !== i)
  }
  // --- One save for the whole Config tab ---
  const dirty = $derived(formDirty || yamlDirty || envDirty)
  const dirtyParts = $derived(
    [formDirty || yamlDirty ? 'Compose' : '', envDirty ? 'Env' : ''].filter(Boolean),
  )
  let savingConfig = $state(false)
  async function saveConfig() {
    savingConfig = true
    configMsg = ''
    try {
      await saveSettings(id, {
        ...(yamlDirty ? { override } : {}),
        ...(formDirty && form ? { form: $state.snapshot(form) as Form } : {}),
        ...(envDirty ? { env: envVars.map(({ key, value }) => ({ key, value })) } : {}),
      })
      await Promise.all([loadConfig(), loadForm(), loadEnv()])
      merged = '' // the merge changed; the merged view must re-read it
      mergedMsg = ''
      configMsg = 'Saved & recreated.'
    } catch (e) {
      configMsg = e instanceof Error ? e.message : String(e)
    } finally {
      savingConfig = false
    }
  }
  function discardConfig() {
    if (formDisk) form = JSON.parse(formDisk)
    formBase = formDisk
    formFor = null
    override = savedOverride
    envVars = JSON.parse(envSaved || '[]')
    configMsg = ''
  }

  // Closing with unsaved Config edits asks first — they'd be lost silently.
  function requestClose() {
    if (dirty && !confirm('Discard unsaved changes to ' + dirtyParts.join(' and ') + '?')) return
    onclose()
  }

  // --- Update (pull a fresher compose from the app's reference store) ---
  let update = $state<UpdateStatus | null>(null)
  let checkingUpdate = $state(false)
  let applyingUpdate = $state(false)
  let updateMsg = $state('')
  let updateChecked = $state(false) // one-shot: don't re-auto-check on error
  // Back the app up before updating. On by default; unticking it is the owner's choice
  // to update an app whose backup keeps failing, and to give up the automatic restore
  // if the new version fails to start.
  let backupFirst = $state(true)

  // The update source, editable: one locator — `<store>/-/<folder>/<app id>` —
  // which is the same string the store's own address bar carries, so retargeting an
  // app is a copy and a paste rather than three fields.
  let refInput = $state('')
  let savingRef = $state(false)
  let sources = $state<StoreSource[]>([])
  const refDirty = $derived(refInput.trim() !== (update?.ref ?? ''))
  const refValid = $derived(isAddressableRef(refInput))

  async function runCheckUpdate() {
    checkingUpdate = true
    updateChecked = true
    updateMsg = ''
    try {
      update = await checkUpdate(id)
      // Only when the operator has nothing half-typed: a check that lands while
      // they are editing must not take the box back.
      if (!refDirty || !refInput) refInput = update.ref ?? ''
    } catch (e) {
      updateMsg = String(e)
    } finally {
      checkingUpdate = false
    }
  }
  // Auto-check the first time the Update tab is opened, and load the configured
  // stores alongside it so the suggestions below the box name the ones this box
  // already trusts.
  $effect(() => {
    if (tab === 'update' && !updateChecked) {
      runCheckUpdate()
      fetchStoreSources()
        .then((r) => (sources = r.sources ?? []))
        .catch(() => {}) // suggestions are a convenience; their absence is not an error
    }
  })

  /** A suggestion for each configured store: this app's id in that store's default
   *  folder. It is a starting point to edit, not a promise the app is there — which
   *  is why saving resolves it server-side before recording it. */
  const refSuggestions = $derived(
    sources
      .map((src) => `${bare(src.url)}/-/Apps/${update?.store_app_id || id}`)
      .filter((r) => r !== (update?.ref ?? '')),
  )

  async function saveRef() {
    savingRef = true
    updateMsg = ''
    try {
      update = await setUpdateRef(id, refInput.trim())
      refInput = update.ref
      updateMsg = 'Update source saved.'
    } catch (e) {
      updateMsg = String(e)
    } finally {
      savingRef = false
    }
  }

  async function runApplyUpdate() {
    applyingUpdate = true
    updateMsg = ''
    try {
      // A data conversion always takes a backup (the server refuses otherwise).
      const res = await applyUpdate(id, !backupFirst && !update?.migrations?.length)
      // An update runs in the background — its backup goes to every enabled
      // destination and can take as long as an upload does — so the tile and Settings
      // → Updates carry its progress and its outcome.
      updateMsg = res.started
        ? 'Started — follow it on the tile or in Settings → Updates.'
        : res.updated
          ? 'Updated & recreated.'
          : 'Already up to date.'
      if (res.warning) updateMsg += ' ' + res.warning
      await runCheckUpdate() // refresh the status after applying
    } catch (e) {
      updateMsg = e instanceof Error ? e.message : String(e)
      // Refused, nothing changed: the way past it is the checkbox beside the button.
      if (e instanceof ApiError && e.body?.no_rollback === true)
        updateMsg += ' Untick "Backup" to update without a backup.'
    } finally {
      applyingUpdate = false
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

<div class="backdrop sheet-backdrop" onclick={requestClose} role="presentation">
  <div class="modal sheet" onclick={(e) => e.stopPropagation()} role="presentation">
    <header>
      <h2>{name}</h2>
      <button class="close" aria-label="Close" onclick={requestClose}>✕</button>
    </header>

    <nav class="tabs segmented">
      {#each tabs as t (t)}
        <button class:active={tab === t} onclick={() => (tab = t)}>{tabLabel[t]}</button>
      {/each}
    </nav>

    <div class="content">
      {#if tab === 'tips'}
        {#if !configLoaded || !renderedLoaded}
          <p class="hint">Loading…</p>
        {:else if !editingTips}
          {#if savedTips.trim()}
            <TipsView tips={renderedTips} />
          {:else}
            <p class="hint">No tips for this app yet.</p>
          {/if}
          <div class="actions">
            <span class="msg">{tipsMsg}</span>
            <button class="primary" onclick={editTips}>{savedTips.trim() ? 'Edit tips' : 'Add tips'}</button>
          </div>
        {:else}
          <p class="hint">
            <strong>Tips</strong> for this app — setup guidance, credentials reminders, reverse-proxy
            details, whatever you need. Markdown. Seeded from the store and freely editable; saving
            writes them into the <code>docker-compose.override.yml</code>. <code>{'${VAR}'}</code>
            references are shown as-is here and filled in once saved.
          </p>
          <textarea
            bind:value={tips}
            spellcheck="false"
            placeholder="Write tips for this app…"
          ></textarea>
          <div class="actions">
            <span class="msg">{tipsMsg}</span>
            <button disabled={savingTips} onclick={cancelTips}>Cancel</button>
            <button class="primary" disabled={savingTips} onclick={saveTips}>
              {savingTips ? 'Saving…' : 'Save tips'}
            </button>
          </div>
        {/if}
      {:else if tab === 'config'}
        <div class="config-bar">
          <div class="views">
            {#each ['compose', 'env'] as const as sct (sct)}
              <button class:active={section === sct} onclick={() => (section = sct)}>
                {sectionLabel[sct]}{#if (sct === 'compose' && (formDirty || yamlDirty)) || (sct === 'env' && envDirty)}<span
                    class="dot"
                    aria-label="unsaved"
                  ></span>{/if}
              </button>
            {/each}
          </div>
          {#if section === 'compose'}
            <div class="views">
              {#each ['form', 'yaml'] as const as v (v)}
                <button class:active={view === v} disabled={switching} onclick={() => switchView(v)}>
                  {viewLabel[v]}
                </button>
              {/each}
            </div>
          {/if}
        </div>

        {#if section === 'compose' && view === 'form'}
          {#if formErr}
            <p class="hint err">{formErr}</p>
          {:else if form}
            <OverrideForm bind:form />
          {:else}
            <p class="hint">Loading…</p>
          {/if}
        {:else if section === 'compose'}
          <div class="yaml">
            <p class="hint">
              <strong>Your override</strong> — layered on the store's compose with Compose's override
              semantics. The <strong>Form</strong> view writes this same file; anything it can't
              express (long-syntax ports, a list-form command) belongs here.
            </p>
            <textarea
              bind:value={override}
              spellcheck="false"
              placeholder={'services:\n  app:\n    ports:\n      - "8080:80"'}
            ></textarea>
            <p class="hint">
              <strong>Store compose</strong> — <code>docker-compose.yml</code> as shipped,
              <strong>read-only</strong>. {BRAND} never modifies it, so updates stay clean.
            </p>
            <pre class="code readonly">{baseCompose || (configLoaded ? '(empty)' : 'Loading…')}</pre>
            <button class="disclosure" onclick={() => (mergedOpen = !mergedOpen)}>
              {mergedOpen ? '▾' : '▸'} Merged compose
              <span class="sub">what the two files add up to, as last saved</span>
            </button>
            {#if mergedOpen}
              <label class="check">
                <input type="checkbox" checked={resolveVars} onchange={toggleResolve} />
                Resolve variables from the <code>.env</code>
              </label>
              <pre class="code readonly">{merged || mergedMsg || 'Loading…'}</pre>
            {/if}
          </div>
        {:else}
          <p class="hint">
            The app's <code>.env</code> — the variables its compose resolves
            <code>{'${VAR}'}</code> against. Those {BRAND} or the box sets (<code>PUID</code>,
            <code>DATA_ROOT</code>, <code>APP_DOMAIN</code>, …) are written back on every start, so
            they are read-only here. The app's secrets and passwords are hidden; its own
            generated secrets are locked too. The rest are yours. Not a service's
            <code>environment:</code> block — that is in <strong>Compose</strong>.
          </p>
          {#if !envLoaded && envMsg}
            <p class="hint err">{envMsg}</p>
          {:else if !envLoaded}
            <p class="hint">Loading…</p>
          {:else}
          <div class="env-list">
            {#each envVars as v, i (i)}
              {@const locked = envLocked(v)}
              <div class="env-row" class:locked>
                <input
                  bind:value={v.key}
                  spellcheck="false"
                  placeholder="KEY"
                  aria-label="Variable name"
                  readonly={!!v.source}
                  class:bad={v.key !== '' && !KEY_RE.test(v.key)}
                />
                {#if v.masked}
                  <!-- The server never sends a masked value. Left blank it is kept;
                       an unlocked one takes what is typed as a replacement. -->
                  <input
                    type="password"
                    bind:value={v.value}
                    autocomplete="off"
                    placeholder={locked ? '••••••••' : '•••••••• (type to replace)'}
                    aria-label="Value for {v.key} (hidden)"
                    readonly={locked}
                  />
                {:else}
                  <input
                    bind:value={v.value}
                    spellcheck="false"
                    placeholder="value"
                    aria-label="Value for {v.key || 'new variable'}"
                    readonly={locked}
                  />
                {/if}
                {#if v.source === 'secret' && hasCredentials}
                  <button
                    class="env-source secret link"
                    title={envSourceTitle.secret}
                    onclick={() => (tab = 'security')}>{envSourceLabel.secret}</button
                  >
                {:else if v.source}
                  <span class="env-source {v.source}" title={envSourceTitle[v.source]}>
                    {envSourceLabel[v.source]}
                  </span>
                {:else}
                  <button
                    class="remove"
                    aria-label="Remove {v.key || 'variable'}"
                    onclick={() => removeEnvVar(i)}>✕</button
                  >
                {/if}
              </div>
            {:else}
              <p class="hint">No variables — this app's <code>.env</code> is empty.</p>
            {/each}
          </div>
            <button class="add-var" onclick={addEnvVar}>Add variable</button>
          {/if}
        {/if}

        <div class="actions">
          <span class="msg">
            {envError ||
              configMsg ||
              (dirty ? `Unsaved: ${dirtyParts.join(', ')}.` : 'Saving recreates the app — Compose reads all of this only when it brings the app up.')}
          </span>
          {#if section === 'compose' && view === 'yaml'}
            <button disabled={validating || savingConfig} onclick={validate}>
              {validating ? 'Checking…' : 'Validate'}
            </button>
          {/if}
          <button disabled={!dirty || savingConfig} onclick={discardConfig}>Discard</button>
          <button
            class="primary"
            disabled={!dirty || savingConfig || switching || !!envError}
            onclick={saveConfig}
          >
            {savingConfig ? 'Saving…' : 'Save & recreate'}
          </button>
        </div>
      {:else if tab === 'update'}
        <p class="hint">
          Pulls a fresher <code>docker-compose.yml</code> from the app's update source and
          re-applies it (<code>docker compose up -d</code>). Your
          <code>docker-compose.override.yml</code> and <code>.env</code> are left untouched.
          The source starts out as the store the app was installed from, and can be pointed
          at another store — paste what the store's own address bar shows.
        </p>
        {#if checkingUpdate && update === null}
          <p class="hint">Checking the store…</p>
        {:else if update}
          <div class="update-box">
            <div class="update-row">
              <span class="k">Update source</span>
              <div class="ref-edit">
                <input
                  class="mono"
                  bind:value={refInput}
                  list="ref-suggestions-{id}"
                  spellcheck="false"
                  autocomplete="off"
                  placeholder="store.example.com/archive/main.zip/-/Apps/AppName"
                />
                <datalist id="ref-suggestions-{id}">
                  {#each refSuggestions as s (s)}
                    <option value={s}></option>
                  {/each}
                </datalist>
                <button
                  disabled={!refDirty || !refValid || savingRef || checkingUpdate}
                  onclick={saveRef}
                >
                  {savingRef ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>
            <div class="update-row">
              <span class="k">Status</span>
              {#if !update.has_ref}
                <span class="v badge health-unhealthy">No source</span>
              {:else if update.error}
                <span class="v badge health-unhealthy">Couldn't check</span>
              {:else if update.available && update.blocked}
                <span class="v badge health-unhealthy">Blocked</span>
              {:else if update.available}
                <span class="v badge health-starting">Update available</span>
              {:else}
                <span class="v badge health-healthy">Up to date</span>
              {/if}
            </div>
            {#if !update.has_ref}
              <p class="hint">
                This app records no store, so {BRAND} doesn't know where its updates come
                from. Paste a reference above — or reinstall it from the store, which
                records one.
              </p>
            {:else if update.error}
              <p class="hint">{update.error}</p>
            {/if}
            {#if update.available && update.blocked}
              <p class="hint">
                This update can't be applied automatically. The app's maintainer says:
              </p>
              <pre class="hint blocked">{$loc(update.blocked_message, update.blocked)}</pre>
            {:else if update.available && update.migrations?.length}
              <p class="hint">
                This update converts the app's data (version {update.data_from} →
                {update.data_to}: {update.migrations.map((m) => $loc(m.title, m.name)).join(', ')}). It
                may take a while; the app is always backed up first, and the backup is
                restored if a step fails.
              </p>
            {/if}
            {#if refDirty && refInput && !refValid}
              <p class="hint">
                A reference names the app inside the store:
                <code>&lt;store&gt;/-/&lt;folder&gt;/&lt;app id&gt;</code>. A store URL on its
                own doesn't say which app to follow.
              </p>
            {:else if refDirty}
              <p class="hint">
                Saving points this app at a different store. Nothing changes until you
                update — and the update replaces the app's <code>docker-compose.yml</code>,
                so point it at the same app, not a different one.
              </p>
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
          <label
            class="check"
            title="Backs the app up to your backup destinations first, so a new version that fails to start is restored automatically."
          >
            <!-- A data conversion always takes a backup: unticking it would only get the update refused. -->
            <input
              type="checkbox"
              bind:checked={backupFirst}
              disabled={applyingUpdate || !!update?.migrations?.length}
            />
            <span>Backup</span>
          </label>
          <button
            class="primary"
            disabled={applyingUpdate || checkingUpdate || !update?.available || !!update?.blocked}
            onclick={runApplyUpdate}
          >
            {applyingUpdate ? 'Updating…' : 'Update now'}
          </button>
        </div>
      {:else if tab === 'backups'}
        <BackupsTab {id} />
      {:else if tab === 'security'}
        <SecurityTab {id} {name} />
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
        {#if servicesLoaded}
          <AppStatsHistory {id} {stat} service={selected} multiService={services.length > 1} />
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
    border-radius: var(--r-xl);
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
  /* Drawn by the global .segmented control; this only places it. */
  .tabs {
    align-self: flex-start;
    margin: 0.75rem 0;
  }
  .content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    /* The modal is a fixed height (min(88vh, 680px)), so any tab whose content is
       plain flow rather than its own scroller was simply cut off — the Backups tab
       reached that point first, with a target picker, an engine tab strip and a list
       of archives stacked in one column.

       Tabs that manage their own scrolling (.env-list, .code, .logs) are unaffected:
       they are flex:1 children of this box, so they still size to it and scroll
       internally rather than growing it. */
    overflow-y: auto;
    /* So a scrollbar sits beside the content rather than on top of the buttons at
       the right edge of a row. */
    padding-right: 0.25rem;
    /* A modal that has scrolled to its end should not then start scrolling the page
       behind it. */
    overscroll-behavior: contain;
  }
  /* Form / YAML / Effective — three views of the same override, right-aligned so
     the switch reads as a mode toggle rather than another row of tabs. */
  .views {
    display: flex;
    align-self: flex-end;
    gap: 0.15rem;
    padding: 0.15rem;
    margin-bottom: 0.6rem;
    background: rgb(118 118 128 / 0.12);
    border-radius: var(--r-sm);
  }
  .views button {
    background: none;
    border: none;
    color: var(--text-muted);
    border-radius: calc(var(--r-sm) - 2px);
    padding: 0.3rem 0.8rem;
    font-size: 0.78rem;
    transition: background var(--dur-fast) var(--ease-ios);
  }
  .views button.active {
    background: var(--surface);
    color: var(--text);
    box-shadow: 0 1px 3px rgb(0 0 0 / 0.12);
  }
  .hint {
    color: var(--text-muted);
    font-size: 0.8rem;
    margin: 0 0 0.5rem;
  }
  .hint.err {
    color: var(--red);
  }
  /* Config tab: section switch on the left, the Compose view switch on the right. */
  .config-bar {
    display: flex;
    justify-content: space-between;
    align-items: flex-end;
    gap: 0.5rem;
  }
  .config-bar .views {
    align-self: auto;
  }
  .dot {
    display: inline-block;
    width: 0.4rem;
    height: 0.4rem;
    margin-left: 0.35rem;
    vertical-align: middle;
    border-radius: 50%;
    background: var(--orange);
  }
  /* YAML view: three blocks stacked, scrolled as one. */
  .yaml {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    padding-right: 0.35rem;
  }
  .yaml textarea {
    flex: none;
    min-height: 11rem;
    margin-bottom: 0.75rem;
  }
  .yaml pre.code {
    flex: none;
    max-height: 18rem;
    overflow: auto;
    margin-bottom: 0.5rem;
  }
  .disclosure {
    align-self: flex-start;
    background: none;
    border: none;
    color: var(--text);
    font-size: 0.82rem;
    padding: 0.4rem 0 0.3rem;
  }
  .disclosure .sub {
    color: var(--text-muted);
    font-size: 0.75rem;
    margin-left: 0.4rem;
  }
  /* Styled like the footer's buttons (below), a size smaller — hence the extra
     class in the selector, to win over that later rule. */
  .content .add-var {
    align-self: flex-start;
    margin-top: 0.4rem;
    padding: 0.3rem 0.8rem;
    font-size: 0.8rem;
  }
  pre.blocked {
    white-space: pre-wrap;
    margin: 0;
  }
  code {
    background: var(--surface-2);
    padding: 0 0.25rem;
    border-radius: 4px;
  }
  .mono {
    font-family: ui-monospace, monospace;
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
    grid-template-columns: 14rem 1fr 4.5rem;
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
  .env-row input[readonly] {
    color: var(--text-muted);
    cursor: default;
  }
  .env-row.locked input {
    background: transparent;
    border-style: dashed;
  }
  .env-source {
    font-size: 0.7rem;
    line-height: 1.1;
    color: var(--text-muted);
    text-align: center;
    cursor: help;
  }
  .env-source.seeded {
    color: var(--orange);
  }
  .env-source.link {
    background: none;
    border: none;
    padding: 0;
    font: inherit;
    font-size: 0.7rem;
    text-decoration: underline dotted;
    cursor: pointer;
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
  .check {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.875rem;
    cursor: pointer;
  }
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
    border: none;
    border-radius: 8px;
    padding: 0.5rem 1.1rem;
    font-size: 0.875rem;
  }
  .actions button:not(.primary),
  .add-var {
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
  .ref-edit {
    display: flex;
    gap: 0.4rem;
    align-items: center;
    min-width: 0;
  }
  .ref-edit input {
    flex: 1;
    min-width: 0;
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.35rem 0.6rem;
    font-size: 0.8rem;
  }
  .ref-edit button {
    flex: none;
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.35rem 0.8rem;
    font-size: 0.8rem;
  }
  .ref-edit button:disabled {
    opacity: 0.5;
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
