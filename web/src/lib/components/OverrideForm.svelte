<script lang="ts">
  import {
    getOverrideForm,
    setOverrideForm,
    type EnvField,
    type FormService,
    type ListField,
    type OverrideForm,
    type Scalar,
  } from '../stores/apps'

  let { id, onsaved }: { id: string; onsaved?: () => void } = $props()

  let form = $state<OverrideForm | null>(null)
  let loadErr = $state('')
  let svcIndex = $state(0)
  let advanced = $state(false)
  let saving = $state(false)
  let msg = $state('')

  let loaded = false
  $effect(() => {
    if (loaded) return
    loaded = true
    getOverrideForm(id).then(normalize).then((f) => (form = f)).catch((e) => (loadErr = String(e)))
  })

  const scalarKeys = [
    'image',
    'restart',
    'privileged',
    'command',
    'mem_limit',
    'cpus',
  ] as const satisfies readonly (keyof FormService)[]
  const listKeys = ['ports', 'volumes', 'devices', 'cap_add'] as const satisfies readonly (keyof FormService)[]

  /** Two adjustments the form makes to what the server sends:
   *  - a field the store sets but the user hasn't touched shows the store's value as
   *    a ghost placeholder rather than as text, so an empty box reads as "inherited"
   *    — and clearing one is exactly how you reset it (the server drops the key);
   *  - an absent list arrives as null (Go's empty slice), which the row editors and
   *    the change detection below both need as an array. */
  function normalize(f: OverrideForm): OverrideForm {
    for (const s of f.services) {
      for (const k of scalarKeys) if (!s[k].overridden) s[k].value = ''
      for (const k of listKeys) {
        s[k].value ??= []
        s[k].base ??= []
      }
      s.environment.value ??= []
      s.environment.base ??= []
    }
    return f
  }

  const svc = $derived(form?.services[svcIndex])

  // --- what's overridden (drives the chips, the reset arrows, and the footer count)

  const sameList = (a: string[], b: string[]) =>
    a.length === b.length && a.every((v, i) => v === b[i])

  function scalarSet(f: Scalar): boolean {
    return f.value.trim() !== '' && f.value !== f.base
  }
  function listSet(f: ListField): boolean {
    return !sameList(clean(f.value), f.base)
  }
  function envSet(f: EnvField): boolean {
    const v = f.value.filter((e) => e.key.trim() !== '')
    return (
      v.length !== f.base.length ||
      v.some((e, i) => e.key !== f.base[i].key || e.value !== f.base[i].value)
    )
  }
  /** A row the store ships and the user hasn't moved or edited. */
  const fromStore = (f: ListField, i: number) => f.base[i] === f.value[i]
  const envFromStore = (f: EnvField, i: number) => {
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

  function addRow(f: ListField) {
    f.value = [...f.value, '']
  }
  function dropRow(f: ListField, i: number) {
    f.value = f.value.filter((_, j) => j !== i)
  }
  function addVar(f: EnvField) {
    f.value = [...f.value, { key: '', value: '' }]
  }
  function dropVar(f: EnvField, i: number) {
    f.value = f.value.filter((_, j) => j !== i)
  }

  function resetScalar(f: Scalar) {
    f.value = ''
  }
  function resetList(f: ListField) {
    f.value = [...f.base]
  }
  function resetEnv(f: EnvField) {
    f.value = f.base.map((e) => ({ ...e }))
  }

  async function save() {
    if (!form) return
    saving = true
    msg = ''
    try {
      await setOverrideForm(id, form)
      msg = 'Saved & recreated.'
      onsaved?.()
    } catch (e) {
      msg = String(e)
    } finally {
      saving = false
    }
  }
</script>

{#if loadErr}
  <p class="hint err">{loadErr}</p>
{:else if !form || !svc}
  <p class="hint">Loading…</p>
{:else}
  <p class="hint">
    Empty means <strong>inherit from the store</strong>. Rows marked <span class="chip">store</span>
    come from <code>docker-compose.yml</code>; editing or removing one replaces the store's list
    outright (Compose would otherwise keep both). Everything here is written to
    <code>docker-compose.override.yml</code> — switch to <strong>YAML</strong> to see the file.
  </p>

  {#if form.services.length > 1}
    <nav class="services">
      {#each form.services as s, i (s.name)}
        <button class:active={i === svcIndex} onclick={() => (svcIndex = i)}>{s.name}</button>
      {/each}
    </nav>
  {/if}

  <div class="fields">
    {@render scalar('Image', svc.image, 'e.g. jellyfin/jellyfin:10.9.6')}
    {@render select('Restart', svc.restart, ['unless-stopped', 'always', 'on-failure', 'no'])}
    {@render list('Ports', svc.ports, 'host:container — e.g. 8096:8096 or 8096:8096/udp')}
    {@render list('Volumes', svc.volumes, 'host:container[:ro] — e.g. /DATA/AppData/app/config:/config')}
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

  <div class="actions">
    <span class="msg">{msg}</span>
    <span class="count">
      {changed === 0 ? 'Nothing overridden' : `${changed} setting${changed > 1 ? 's' : ''} overridden`}
    </span>
    <button class="primary" disabled={saving} onclick={save}>
      {saving ? 'Saving…' : 'Save & recreate'}
    </button>
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
          <div class="row">
            <input bind:value={f.value[i]} spellcheck="false" {placeholder} />
            {#if fromStore(f, i)}<span class="chip">store</span>{/if}
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
          <div class="row">
            <input class="key" bind:value={f.value[i].key} spellcheck="false" placeholder="KEY" />
            <input bind:value={f.value[i].value} spellcheck="false" placeholder="value" />
            {#if envFromStore(f, i)}<span class="chip">store</span>{/if}
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
  .hint.err {
    color: var(--red);
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
  .actions {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 0.75rem;
    margin-top: 0.6rem;
  }
  .msg,
  .count {
    font-size: 0.8rem;
    color: var(--text-muted);
  }
  .msg {
    margin-right: auto;
  }
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
    border: none;
    border-radius: 8px;
    padding: 0.5rem 1.1rem;
    font-size: 0.875rem;
  }
  .primary:disabled {
    opacity: 0.5;
  }
</style>
