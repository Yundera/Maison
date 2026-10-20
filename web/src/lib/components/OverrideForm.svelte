<script lang="ts" module>
  import type { EnvField, FormService, ListField, OverrideForm, Scalar } from '../stores/apps'

  const scalarKeys = [
    'image',
    'restart',
    'privileged',
    'command',
    'mem_limit',
    'cpus',
  ] as const satisfies readonly (keyof FormService)[]
  const listKeys = ['ports', 'volumes', 'devices', 'cap_add'] as const satisfies readonly (keyof FormService)[]
  const webuiKeys = ['scheme', 'host', 'port', 'path'] as const

  /** Two adjustments the form makes to what the server sends:
   *  - a field the store sets but the user hasn't touched shows the store's value as
   *    a ghost placeholder rather than as text, so an empty box reads as "inherited"
   *    — and clearing one is exactly how you reset it (the server drops the key);
   *  - an absent list arrives as null (Go's empty slice), which the row editors and
   *    the change detection below both need as an array.
   *  Exported: the settings window normalizes every form it receives (load, and the
   *  YAML → Form switch) before handing it here. */
  export function normalizeForm(f: OverrideForm): OverrideForm {
    for (const s of f.services) {
      for (const k of scalarKeys) if (!s[k].overridden) s[k].value = ''
      for (const k of listKeys) {
        s[k].value ??= []
        s[k].base ??= []
      }
      s.environment.value ??= []
      s.environment.base ??= []
    }
    if (f.webui) for (const k of webuiKeys) if (!f.webui[k].overridden) f.webui[k].value = ''
    return f
  }
</script>

<script lang="ts">
  import { untrack } from 'svelte'
  import type { EnvField as EF, ListField as LF, OverrideForm as OF, Scalar as SC } from '../stores/apps'

  // Controlled: the settings window owns the form (it saves it, together with the
  // .env, and carries it across the Form / YAML switch).
  let { form = $bindable() }: { form: OF } = $props()

  let svcIndex = $state(0)
  let advanced = $state(false)

  const svc = $derived(form?.services[Math.min(svcIndex, (form?.services.length ?? 1) - 1)])

  // --- what's overridden (drives the chips, the reset arrows, and the footer count)

  const sameList = (a: string[], b: string[]) =>
    a.length === b.length && a.every((v, i) => v === b[i])

  function scalarSet(f: SC): boolean {
    return f.value.trim() !== '' && f.value !== f.base
  }
  function listSet(f: LF): boolean {
    return !sameList(clean(f.value), f.base)
  }
  function envSet(f: EF): boolean {
    const v = f.value.filter((e) => e.key.trim() !== '')
    return (
      v.length !== f.base.length ||
      v.some((e, i) => e.key !== f.base[i].key || e.value !== f.base[i].value)
    )
  }
  /** A row the store ships and the user hasn't moved or edited. */
  const fromStore = (f: LF, i: number) => f.base[i] === f.value[i]
  const envFromStore = (f: EF, i: number) => {
    const b = f.base[i]
    return !!b && b.key === f.value[i].key && b.value === f.value[i].value
  }

  const changed = $derived(
    !svc
      ? 0
      : scalarKeys.filter((k) => scalarSet(svc[k])).length +
        [svc.ports, svc.volumes, svc.devices, svc.cap_add].filter(listSet).length +
        (envSet(svc.environment) ? 1 : 0),
  )

  // --- row editing

  const clean = (rows: string[]) => rows.map((r) => r.trim()).filter((r) => r !== '')

  function addRow(f: LF) {
    f.value = [...f.value, '']
  }
  function dropRow(f: LF, i: number) {
    f.value = f.value.filter((_, j) => j !== i)
  }
  function addVar(f: EF) {
    f.value = [...f.value, { key: '', value: '' }]
  }
  function dropVar(f: EF, i: number) {
    f.value = f.value.filter((_, j) => j !== i)
  }

  function resetScalar(f: SC) {
    f.value = ''
  }
  function resetList(f: LF) {
    f.value = [...f.base]
  }
  function resetEnv(f: EF) {
    f.value = f.base.map((e) => ({ ...e }))
  }

  // A client-side approximation of the click URL while the host is being edited;
  // the server's resolved one (web_url) is shown otherwise.
  const previewUrl = $derived(
    (() => {
      const w = form?.webui
      if (!w) return ''
      const pick = (f: SC) => f.value.trim() || f.base
      const host = pick(w.host)
      if (!host) return ''
      const port = pick(w.port) ? `:${pick(w.port)}` : ''
      let path = pick(w.path) || '/'
      if (!path.startsWith('/')) path = '/' + path
      return `${pick(w.scheme) || 'https'}://${host}${port}${path}`
    })(),
  )
  // The URL fields as the form last arrived (a load, a save, a YAML → Form switch
  // all hand over a new form object): while they still read that way, the server's
  // resolved URL is the truth; once edited, only the client's preview can follow.
  let webuiAsLoaded = $state('')
  $effect.pre(() => {
    const f = form
    webuiAsLoaded = untrack(() => JSON.stringify(f?.webui ?? null))
  })
  const webuiEdited = $derived(JSON.stringify(form?.webui ?? null) !== webuiAsLoaded)
  const webuiChanged = $derived(
    !!form?.webui && webuiKeys.some((k) => scalarSet(form.webui![k])),
  )
</script>

{#if !form || !svc}
  <p class="hint">Loading…</p>
{:else}
  <p class="hint">
    What runs: the store's <code>docker-compose.yml</code> with your changes on top, variables as
    written. Rows marked <span class="chip">store</span> are the store's — remove one to replace the
    store's list outright (Compose would otherwise keep both), or add your own below. Empty means
    <strong>inherit from the store</strong>. Saved into <code>docker-compose.override.yml</code> —
    switch to <strong>YAML</strong> to see the file.
    <span class="count">
      {changed + (webuiChanged ? 1 : 0) === 0
        ? 'Nothing overridden.'
        : `${changed + (webuiChanged ? 1 : 0)} overridden.`}
    </span>
  </p>

  <div class="fields">
    {#if form.webui}
      <div class="group">Opening URL</div>
      {@render select('Scheme', form.webui.scheme, ['https', 'http'])}
      {@render scalar('Host', form.webui.host, 'e.g. myapp-${APP_DOMAIN}')}
      {@render scalar('Port', form.webui.port, '(none — 443 via the gateway)')}
      {@render scalar('Path', form.webui.path, '/')}
      <p class="note url">
        Where the tile's <em>Open</em> points.
        {#if webuiEdited && previewUrl}
          Preview: <span class="mono">{previewUrl}</span>
        {:else if form.web_url}
          Now: <a href={form.web_url} target="_blank" rel="noopener">{form.web_url}</a>
        {:else if previewUrl}
          Preview: <span class="mono">{previewUrl}</span>
        {:else}
          None set — the tile has no <em>Open</em> action.
        {/if}
      </p>
    {/if}

    {#if form.services.length > 1}
      <nav class="services">
        {#each form.services as s, i (s.name)}
          <button class:active={i === svcIndex} onclick={() => (svcIndex = i)}>{s.name}</button>
        {/each}
      </nav>
    {:else}
      <div class="group">Service <span class="mono">{svc.name}</span></div>
    {/if}

    {@render scalar('Image', svc.image, 'e.g. jellyfin/jellyfin:10.9.6')}
    {@render select('Restart', svc.restart, ['unless-stopped', 'always', 'on-failure', 'no'])}
    {@render list('Ports', svc.ports, 'host:container — e.g. 8096:8096 or 8096:8096/udp')}
    {@render list('Volumes', svc.volumes, 'host:container[:ro] — e.g. ${DATA_ROOT}/AppData/app/config:/config')}
    {@render env('Environment', svc.environment)}

    <button class="disclosure" onclick={() => (advanced = !advanced)}>
      {advanced ? '▾' : '▸'} Advanced
      <span class="sub">devices · capabilities · command · privileged · limits</span>
    </button>
    {#if advanced}
      {@render select('Privileged', svc.privileged, ['true', 'false'])}
      {@render scalar('Command', svc.command, "the container's command — overrides the image's")}
      {@render scalar('Memory limit', svc.mem_limit, 'e.g. 2g, 512m')}
      {@render scalar('CPUs', svc.cpus, 'e.g. 1.5')}
      {@render list('Devices', svc.devices, 'e.g. /dev/dri:/dev/dri')}
      {@render list('Capabilities', svc.cap_add, 'cap_add — e.g. NET_ADMIN')}
    {/if}
  </div>
{/if}

<!-- A field the form can't represent faithfully (a long-syntax port, a list-form
     command). Shown as it is on disk, but only the YAML view may edit it. -->
{#snippet complex(label: string, raw: string)}
  <div class="field">
    <span class="label">{label}</span>
    <div class="col">
      <pre class="raw">{raw}</pre>
      <span class="note">Advanced syntax — edit this one in the YAML view.</span>
    </div>
  </div>
{/snippet}

{#snippet scalar(label: string, f: Scalar, placeholder: string)}
  {#if f.complex}
    {@render complex(label, f.raw ?? '')}
  {:else}
    <div class="field">
      <span class="label">{label}</span>
      <input bind:value={f.value} spellcheck="false" placeholder={f.base || placeholder} />
      {@render reset(scalarSet(f), () => resetScalar(f))}
    </div>
  {/if}
{/snippet}

{#snippet select(label: string, f: Scalar, options: string[])}
  {#if f.complex}
    {@render complex(label, f.raw ?? '')}
  {:else}
    <div class="field">
      <span class="label">{label}</span>
      <select bind:value={f.value}>
        <option value="">{f.base ? `${f.base} (store)` : '(unset)'}</option>
        {#each options as o (o)}
          <option value={o}>{o}</option>
        {/each}
      </select>
      {@render reset(scalarSet(f), () => resetScalar(f))}
    </div>
  {/if}
{/snippet}

{#snippet list(label: string, f: ListField, placeholder: string)}
  {#if f.complex}
    {@render complex(label, f.raw ?? '')}
  {:else}
    <div class="field">
      <span class="label">{label}</span>
      <div class="col">
        {#each f.value as _, i (i)}
          {@const store = fromStore(f, i)}
          <div class="row" class:store>
            <input bind:value={f.value[i]} spellcheck="false" {placeholder} readonly={store} />
            {#if store}<span class="chip">store</span>{/if}
            <button class="icon" aria-label="Remove" onclick={() => dropRow(f, i)}>✕</button>
          </div>
        {/each}
        <button class="add" onclick={() => addRow(f)}>+ Add</button>
      </div>
      {@render reset(listSet(f), () => resetList(f))}
    </div>
  {/if}
{/snippet}

{#snippet env(label: string, f: EnvField)}
  {#if f.complex}
    {@render complex(label, f.raw ?? '')}
  {:else}
    <div class="field">
      <span class="label">{label}</span>
      <div class="col">
        {#each f.value as _, i (i)}
          {@const store = envFromStore(f, i)}
          <div class="row" class:store>
            <input class="key" bind:value={f.value[i].key} spellcheck="false" placeholder="KEY" readonly={store} />
            <input bind:value={f.value[i].value} spellcheck="false" placeholder="value" readonly={store} />
            {#if store}<span class="chip">store</span>{/if}
            <button class="icon" aria-label="Remove" onclick={() => dropVar(f, i)}>✕</button>
          </div>
        {/each}
        <button class="add" onclick={() => addVar(f)}>+ Add</button>
      </div>
      {@render reset(envSet(f), () => resetEnv(f))}
    </div>
  {/if}
{/snippet}

{#snippet reset(active: boolean, run: () => void)}
  {#if active}
    <button class="icon revert" title="Reset to the store's value" onclick={run}>↺</button>
  {:else}
    <span class="icon-gap"></span>
  {/if}
{/snippet}

<style>
  .hint {
    color: var(--text-muted);
    font-size: 0.8rem;
    margin: 0 0 0.6rem;
  }
  code {
    background: var(--surface-2);
    padding: 0 0.25rem;
    border-radius: 4px;
  }
  /* Service sub-tabs — one per compose service, in the base file's order. */
  .services {
    display: flex;
    gap: 0.25rem;
    margin-bottom: 0.6rem;
  }
  .services button {
    background: var(--surface-2);
    border: 1px solid transparent;
    color: var(--text-muted);
    border-radius: 999px;
    padding: 0.25rem 0.75rem;
    font-size: 0.78rem;
    font-family: ui-monospace, monospace;
  }
  .services button.active {
    color: var(--text);
    border-color: var(--primary);
  }
  .fields {
    flex: 1;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
    padding-right: 0.35rem;
  }
  .field {
    display: grid;
    grid-template-columns: 7rem 1fr 1.5rem;
    align-items: start;
    gap: 0.75rem;
  }
  .label {
    font-size: 0.8rem;
    color: var(--text-muted);
    padding-top: 0.55rem;
  }
  .col {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
    min-width: 0;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .row input {
    flex: 1;
    min-width: 0;
  }
  .row input.key {
    flex: 0 0 11rem;
  }
  input,
  select {
    background: var(--surface-sunken);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.45rem 0.6rem;
    font-family: ui-monospace, monospace;
    font-size: 0.8rem;
    width: 100%;
  }
  input::placeholder {
    color: var(--text-subtle);
  }
  /* Marks a row as the store's, so an edit to it visibly departs from the base. */
  .chip {
    flex: none;
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 0.05rem 0.4rem;
  }
  .add {
    align-self: flex-start;
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.25rem 0.7rem;
    font-size: 0.75rem;
  }
  .icon {
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 0.85rem;
    padding: 0.2rem;
  }
  .icon.revert {
    color: var(--primary);
    margin-top: 0.35rem;
  }
  .icon-gap {
    display: block;
  }
  .disclosure {
    align-self: flex-start;
    background: none;
    border: none;
    color: var(--text);
    font-size: 0.82rem;
    padding: 0.5rem 0 0.2rem;
  }
  .disclosure .sub {
    color: var(--text-muted);
    font-size: 0.75rem;
    margin-left: 0.4rem;
  }
  .raw {
    background: var(--surface-sunken);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.5rem 0.6rem;
    font-size: 0.75rem;
    margin: 0;
    white-space: pre;
    overflow-x: auto;
  }
  .note {
    font-size: 0.72rem;
    color: var(--text-muted);
  }
  .count {
    margin-left: 0.25rem;
    color: var(--text);
  }
  /* A heading inside the field list: the app-level opening URL, then the service. */
  .group {
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    padding-top: 0.4rem;
    border-bottom: 1px solid var(--border);
  }
  .mono {
    font-family: ui-monospace, monospace;
    text-transform: none;
    letter-spacing: 0;
  }
  .note.url {
    margin: 0 0 0 7.75rem;
  }
  .note.url a {
    color: var(--primary);
  }
  /* The store's own rows: shown as what runs, not as something typed here. */
  .row.store input {
    background: transparent;
    border-style: dashed;
    color: var(--text-muted);
  }
</style>
