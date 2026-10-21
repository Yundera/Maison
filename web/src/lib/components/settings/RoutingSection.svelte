<script lang="ts">
  // How the box reaches its apps: the routing setting (internal/routing).
  //
  // An app's listing says what it needs to be reachable — a service, a name, a port
  // — and never how. How is this box's choice, and it is one of three:
  //
  //  - ports: each route's port is published on the host, opened as http://box:port.
  //  - labels: proxy labels on every route, on every domain in the list. The first
  //    domain is the primary — the click URL and the app's own APP_URL come from it.
  //  - external: nothing is written; an operator's own router reads /api/routes.
  //
  // The preview is rendered by the server, through the same generator every app
  // goes through, so this panel carries no copy of it and cannot drift from it.
  //
  // Applying is explicit rather than per-keystroke: the save rewrites every app's
  // override and recreates every running container.
  import { onMount } from 'svelte'
  import {
    loadRouting,
    previewRouting,
    saveRouting,
    type Domain,
    type RoutingSetting,
    type RoutingView,
  } from '../../stores/settings'
  import { t } from '../../i18n'

  type LabelRow = { key: string; value: string }
  type DomainDraft = { name: string; domain: string; labels: LabelRow[] }
  type Draft = {
    renderer: string
    bind: string
    urlHost: string
    low: string
    high: string
    dialect: string
    domains: DomainDraft[]
    urlTemplate: string
  }

  let draft = $state<Draft>(blank())
  let view = $state<RoutingView | null>(null)
  let preview = $state<RoutingView['preview'] | null>(null)
  let previewError = $state('')

  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')
  let applied = $state(false)
  // JSON of what the server last accepted — the dirty comparison and revert target.
  let saved = $state('')

  function blank(): Draft {
    return { renderer: 'ports', bind: '', urlHost: '', low: '', high: '', dialect: 'caddy', domains: [], urlTemplate: '' }
  }

  function toDraft(s: RoutingSetting): Draft {
    const [low, high] = s['port-range'] ?? [0, 0]
    return {
      renderer: s.renderer,
      bind: s.bind ?? '',
      urlHost: s['url-host'] ?? '',
      low: low ? String(low) : '',
      high: high ? String(high) : '',
      dialect: s.dialect || 'caddy',
      domains: (s.domains ?? []).map((d) => ({
        name: d.name,
        domain: d.domain,
        labels: Object.entries(d.labels ?? {}).map(([key, value]) => ({ key, value })),
      })),
      urlTemplate: s['url-template'] ?? '',
    }
  }

  // Blank label rows are someone who clicked "+ label" and changed their mind; the
  // server drops them too — this keeps the dirty flag from lighting up over one.
  function toSetting(d: Draft): RoutingSetting {
    const out: RoutingSetting = { renderer: d.renderer }
    if (d.bind.trim()) out.bind = d.bind.trim()
    if (d.urlHost.trim()) out['url-host'] = d.urlHost.trim()
    const low = Number(d.low), high = Number(d.high)
    if (low && high) out['port-range'] = [low, high]
    if (d.dialect && d.dialect !== 'caddy') out.dialect = d.dialect
    const doms: Domain[] = d.domains.map((x) => {
      const labels: Record<string, string> = {}
      for (const { key, value } of x.labels) if (key.trim()) labels[key.trim()] = value.trim()
      const dom: Domain = { name: x.name.trim(), domain: x.domain.trim() }
      if (Object.keys(labels).length) dom.labels = labels
      return dom
    })
    if (doms.length) out.domains = doms
    if (d.urlTemplate.trim()) out['url-template'] = d.urlTemplate.trim()
    return out
  }

  const payload = $derived(toSetting(draft))
  const dirty = $derived(JSON.stringify(payload) !== saved)
  const managedBy = $derived(view?.managedBy ?? '')
  const complete = $derived(draft.renderer !== 'labels' || (draft.domains.length > 0 && draft.domains.every((d) => d.name.trim() && d.domain.trim())))

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  function adopt(v: RoutingView) {
    view = v
    preview = v.preview
    previewError = ''
    // With nothing stored the editor opens on what the box runs on — the effective
    // setting — so the first save states the default instead of starting from blank.
    draft = initial(v)
    saved = JSON.stringify(toSetting(draft))
  }

  // The effective setting fills url-host with the address the dashboard was seen
  // at; that is a fallback, not a choice, and saving the default must not pin it.
  function initial(v: RoutingView): Draft {
    const d = toDraft(v.setting ?? v.effective)
    if (!v.setting && d.urlHost === v.seenHost) d.urlHost = ''
    return d
  }

  onMount(async () => {
    try {
      adopt(await loadRouting())
    } catch (e) {
      error = message(e)
    } finally {
      loading = false
    }
  })

  // Re-render the preview for the draft, debounced; a draft the server refuses shows
  // its reason where the preview would be.
  let previewTimer: number | undefined
  $effect(() => {
    const body = JSON.stringify(payload)
    if (loading || body === saved) {
      if (view) preview = view.preview
      previewError = ''
      return
    }
    clearTimeout(previewTimer)
    previewTimer = window.setTimeout(async () => {
      try {
        const v = await previewRouting(JSON.parse(body))
        preview = v.preview
        previewError = ''
      } catch (e) {
        previewError = message(e)
      }
    }, 300)
  })

  async function apply() {
    busy = true
    error = ''
    try {
      adopt(await saveRouting(payload))
      applied = true
      setTimeout(() => (applied = false), 4000)
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  async function useDefault() {
    busy = true
    error = ''
    try {
      adopt(await saveRouting(null))
      applied = true
      setTimeout(() => (applied = false), 4000)
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  function revert() {
    if (view) draft = initial(view)
    error = ''
  }

  const addDomain = () => (draft.domains = [...draft.domains, { name: '', domain: '{name}.', labels: [] }])
  const removeDomain = (i: number) => (draft.domains = draft.domains.filter((_, n) => n !== i))
  function move(i: number, by: number) {
    const list = [...draft.domains]
    const [d] = list.splice(i, 1)
    list.splice(i + by, 0, d)
    draft.domains = list
  }
  const addLabel = (d: DomainDraft) => (d.labels = [...d.labels, { key: '', value: '' }])
  const removeLabel = (d: DomainDraft, i: number) => (d.labels = d.labels.filter((_, n) => n !== i))

  const RENDERERS = ['ports', 'labels', 'external'] as const
</script>

<section class="card">
  <header>
    <h3>{$t('routing')}</h3>
    <p class="hint">{$t('routing_hint')}</p>
  </header>

  {#if loading}
    <p class="note">{$t('loading')}</p>
  {:else}
    {#if managedBy}
      <p class="hint">{$t('settings_managed_by', { owner: managedBy })}</p>
    {:else if !view?.setting}
      <p class="hint">{$t('routing_default')}</p>
    {/if}

    <fieldset class="lock" disabled={!!managedBy}>
      <div class="modes" role="radiogroup" aria-label={$t('routing_renderer')}>
        {#each RENDERERS as r (r)}
          <label class="mode" class:on={draft.renderer === r}>
            <input type="radio" name="renderer" value={r} bind:group={draft.renderer} />
            <span class="mname">{$t(`routing_mode_${r}`)}</span>
            <span class="mhint">{$t(`routing_mode_${r}_hint`)}</span>
          </label>
        {/each}
      </div>

      {#if draft.renderer === 'ports'}
        <div class="grid">
          <label>
            <span>{$t('routing_bind')}</span>
            <input bind:value={draft.bind} placeholder="0.0.0.0" spellcheck="false" autocapitalize="off" />
          </label>
          <label>
            <span>{$t('routing_url_host')}</span>
            <input bind:value={draft.urlHost} placeholder={view?.seenHost || $t('routing_url_host_unseen')} spellcheck="false" autocapitalize="off" />
          </label>
          <label>
            <span>{$t('routing_port_range')}</span>
            <span class="range">
              <input bind:value={draft.low} placeholder="20000" inputmode="numeric" />
              <span>–</span>
              <input bind:value={draft.high} placeholder="29999" inputmode="numeric" />
            </span>
          </label>
        </div>
        <p class="hint">{$t('routing_url_host_hint')}</p>
        {#if view?.publicWarning}<p class="warn">{$t('routing_public_warning')}</p>{/if}
      {:else if draft.renderer === 'labels'}
        {#if (view?.dialects?.length ?? 0) > 1}
          <label class="inline">
            <span>{$t('routing_dialect')}</span>
            <select bind:value={draft.dialect}>
              {#each view?.dialects ?? [] as d (d)}<option value={d}>{d}</option>{/each}
            </select>
          </label>
        {/if}
        <p class="hint">
          {$t('routing_domains_hint')}
          {#each Object.keys(view?.vars ?? {}) as v (v)}<code class="var">$&#123;{v}&#125;</code>{/each}
        </p>
        <ul class="rows">
          {#each draft.domains as d, i (i)}
            <li class="row">
              <div class="top">
                {#if i === 0}<span class="badge">{$t('routing_primary')}</span>{/if}
                <input class="name" placeholder={$t('domain_name')} aria-label={$t('domain_name')} bind:value={d.name} spellcheck="false" autocapitalize="off" />
                <input class="host" placeholder="&#123;name&#125;-$&#123;APP_DOMAIN&#125;" aria-label={$t('domain_host')} bind:value={d.domain} spellcheck="false" autocapitalize="off" />
                <button class="icon" aria-label={$t('move_up')} disabled={busy || i === 0} onclick={() => move(i, -1)}>↑</button>
                <button class="icon" aria-label={$t('move_down')} disabled={busy || i === draft.domains.length - 1} onclick={() => move(i, 1)}>↓</button>
                <button class="trash" aria-label={$t('remove')} disabled={busy || draft.domains.length === 1} onclick={() => removeDomain(i)}>✕</button>
              </div>
              <div class="labels">
                <span class="dlabel">{$t('routing_labels')}</span>
                {#each d.labels as l, j (j)}
                  <div class="drow">
                    <input class="dkey" placeholder="import" aria-label={$t('routing_label_key')} bind:value={l.key} spellcheck="false" autocapitalize="off" />
                    <input class="dval" placeholder="gateway_tls" aria-label={$t('routing_label_value')} bind:value={l.value} spellcheck="false" autocapitalize="off" />
                    <button class="trash" aria-label={$t('remove')} disabled={busy} onclick={() => removeLabel(d, j)}>✕</button>
                  </div>
                {/each}
                <button class="chip" disabled={busy} onclick={() => addLabel(d)}>+ {$t('routing_add_label')}</button>
              </div>
            </li>
          {/each}
        </ul>
        <p class="hint">{$t('routing_labels_hint')}</p>
        <div class="add">
          <button class="chip" disabled={busy} onclick={addDomain}>+ {$t('add_domain')}</button>
        </div>
      {:else}
        <label class="inline wide">
          <span>{$t('routing_url_template')}</span>
          <input bind:value={draft.urlTemplate} placeholder="https://&#123;name&#125;.home.lan" spellcheck="false" autocapitalize="off" />
        </label>
        <p class="hint">{$t('routing_external_api')} <a href="/api/routes" target="_blank" rel="noreferrer">/api/routes</a></p>
      {/if}

      <h4 class="sub">{$t('routing_preview', { app: preview?.app ?? 'myapp' })}</h4>
      {#if previewError}
        <p class="err">{previewError}</p>
      {:else if preview}
        {#if preview.override}
          <pre class="preview">{preview.override}</pre>
        {:else}
          <p class="note">{$t('routing_preview_nothing')}</p>
        {/if}
        {#if Object.keys(preview.vars ?? {}).length}
          <pre class="preview">{Object.entries(preview.vars).map(([k, v]) => `${k}=${v}`).join('\n')}</pre>
        {/if}
      {/if}

      <footer class="actions">
        <button class="go" disabled={busy || !dirty || !complete} onclick={apply}>
          {busy ? '…' : $t('save_recreate')}
        </button>
        {#if dirty}
          <button class="ghost" disabled={busy} onclick={revert}>{$t('revert')}</button>
        {:else if view?.setting}
          <button class="ghost" disabled={busy} onclick={useDefault}>{$t('routing_use_default')}</button>
        {/if}
        {#if busy}
          <span class="note">{$t('republishing')}</span>
        {:else if applied}
          <span class="note">{$t('routing_applied')}</span>
        {:else if dirty}
          <span class="note">{$t('routing_apply_hint')}</span>
        {/if}
      </footer>
    </fieldset>

    {#if error}<p class="err">{error}</p>{/if}
  {/if}
</section>

<style>
  .card {
    max-width: 46rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
    padding: 1.25rem 1.5rem 1.5rem;
    display: flex;
    flex-direction: column;
    gap: 1rem;
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
  .sub {
    margin: 0.5rem 0 0;
    font-size: 0.8rem;
    font-weight: 600;
    color: var(--grey-600);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  /* Only there to disable every control at once when the setting is managed. */
  .lock {
    border: 0;
    margin: 0;
    padding: 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 0.9rem;
  }
  .hint {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
  .warn {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--red);
  }
  .modes {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
    gap: 0.5rem;
  }
  .mode {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    padding: 0.7rem 0.85rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
    cursor: pointer;
  }
  .mode.on {
    border-color: var(--primary);
    background: hsla(208, 60%, 97%, 1);
  }
  .mode input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }
  .mname {
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--grey-800);
  }
  .mhint {
    font-size: 0.75rem;
    line-height: 1.4;
    color: var(--grey-600);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));
    gap: 0.6rem;
  }
  .grid label,
  .inline {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    font-size: 0.75rem;
    color: var(--grey-600);
  }
  .range {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .range input {
    width: 6rem;
  }
  .wide input {
    width: 100%;
  }
  .var {
    font-size: 0.75rem;
    background: hsla(208, 16%, 94%, 1);
    border-radius: 4px;
    padding: 0.05rem 0.3rem;
    margin-right: 0.3rem;
    white-space: nowrap;
  }
  .badge {
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--primary);
    border: 1px solid var(--primary);
    border-radius: 999px;
    padding: 0.1rem 0.5rem;
    flex: none;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .row {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    padding: 0.85rem 1rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
  }
  .top {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .labels {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.4rem;
  }
  .dlabel {
    font-size: 0.75rem;
    color: var(--grey-600);
  }
  .drow {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
  }
  input,
  select {
    min-width: 0;
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    padding: 0 0.5rem;
    font-size: 0.85rem;
  }
  .name {
    flex: 0 0 7rem;
  }
  .host,
  .dval {
    flex: 1;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.8rem;
  }
  .dkey {
    flex: 0 0 9rem;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.8rem;
  }
  .preview {
    margin: 0;
    padding: 0.55rem 0.7rem;
    border-radius: 6px;
    background: hsla(208, 16%, 96%, 1);
    color: var(--grey-600);
    font-size: 0.75rem;
    line-height: 1.5;
    white-space: pre-wrap;
    word-break: break-all;
    overflow-x: auto;
  }
  .icon,
  .trash {
    border: none;
    background: none;
    font-size: 0.85rem;
    cursor: pointer;
    flex: none;
  }
  .icon {
    color: var(--grey-600);
  }
  .trash {
    color: var(--red);
  }
  .icon:disabled,
  .trash:disabled {
    opacity: 0.3;
    cursor: default;
  }
  .add {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
  }
  .chip {
    border: 1px dashed #cfcfcf;
    background: none;
    color: var(--primary);
    border-radius: 999px;
    padding: 0.3rem 0.75rem;
    font-size: 0.8rem;
    cursor: pointer;
  }
  .chip:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .actions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.6rem;
  }
  .go {
    border: none;
    background: var(--primary);
    color: #fff;
    border-radius: 6px;
    height: 2rem;
    padding: 0 0.9rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .go:disabled {
    opacity: 0.45;
    cursor: default;
  }
  .ghost {
    border: 1px solid #cfcfcf;
    background: #fff;
    color: var(--grey-600);
    border-radius: 6px;
    height: 2rem;
    padding: 0 0.9rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .note {
    margin: 0;
    font-size: 0.8rem;
    color: var(--grey-600);
  }
  .err {
    margin: 0;
    font-size: 0.8rem;
    color: var(--red);
  }
</style>
