<script lang="ts">
  // The domains apps are published on.
  //
  // Three things this panel has to make true, because the previous version of it
  // made none of them true:
  //
  //  1. The primary domain is *shown*, first and read-only. Every app already
  //     routes there through its own compose label; the list below is added on
  //     top of it and can never replace it. A list of extra names with nothing to
  //     be extra *to* reads like a domain switcher, which is the one thing it is not.
  //
  //  2. No built-in domains. sslip.io / nip.io were hardcoded presets here — which
  //     is a Yundera deployment's policy leaking into the dashboard's UI. Every
  //     entry is now written by the operator; the placeholders show the shape.
  //
  //  3. The Caddy label is the point. An entry is not "a domain", it is a route
  //     group cloned onto a host, and the directives it carries (TLS above all)
  //     differ per domain — the gateway host wants `import: gateway_tls`, a
  //     public one wants nothing so Let's Encrypt takes over. So directives are
  //     editable per entry, and each entry previews the label it generates.
  //
  // Applying is explicit rather than per-keystroke: the save rewrites every app's
  // override and recreates every running container (see internal/caddyroutes).
  import { onMount } from 'svelte'
  import { loadDomains, saveDomains, type Domain, type DomainsView } from '../../stores/settings'
  import { t } from '../../i18n'

  // The app name the previews are drawn with. A real app id would be no more
  // truthful — the label is generated for *every* app — and would go stale.
  const SAMPLE = 'myapp'

  // A row being edited. Directives are a list, not a map, so a half-typed key does
  // not collide with the empty one next to it while it is being typed.
  type Draft = { name: string; host: string; directives: { key: string; value: string }[] }

  let list = $state<Draft[]>([])
  let primaryToken = $state('')
  let primaryHost = $state('')
  let vars = $state<Record<string, string>>({})

  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')
  let applied = $state(false)
  // JSON of what the server last accepted — the dirty comparison and revert target.
  let saved = $state('[]')

  const toDraft = (d: Domain): Draft => ({
    name: d.name,
    host: d.domain,
    directives: Object.entries(d.directives ?? {}).map(([key, value]) => ({ key, value })),
  })

  // Drop blank directive rows on the way out: an unfilled row is someone who
  // clicked "+ directive" and changed their mind, not an empty Caddy sub-directive
  // to write into every app. The server drops them too — this keeps the dirty flag
  // from lighting up over one.
  function toDomain(d: Draft): Domain {
    const directives: Record<string, string> = {}
    for (const { key, value } of d.directives) {
      if (key.trim()) directives[key.trim()] = value.trim()
    }
    const out: Domain = { name: d.name.trim(), domain: d.host.trim() }
    if (Object.keys(directives).length) out.directives = directives
    return out
  }

  const payload = $derived(list.map((d) => toDomain(d)))
  const dirty = $derived(JSON.stringify(payload) !== saved)
  const complete = $derived(payload.every((d) => d.name && d.domain))
  const duplicate = $derived(new Set(payload.map((d) => d.domain)).size !== payload.length)

  /** Resolve a templated host against .env.app, leaving unknown variables visible
   *  as themselves — an unresolved `${FOO}` in the preview is the truth about what
   *  compose will do with it. */
  const resolve = (h: string) => h.replace(/\$\{(\w+)\}/g, (m, k) => vars[k] || m)

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  function adopt(v: DomainsView) {
    primaryToken = v.primaryToken || '${APP_DOMAIN}'
    primaryHost = v.primaryHost || ''
    vars = v.vars ?? {}
    list = (v.domains ?? []).map(toDraft)
    saved = JSON.stringify(v.domains ?? [])
  }

  onMount(async () => {
    try {
      adopt(await loadDomains())
    } catch (e) {
      error = message(e)
    } finally {
      loading = false
    }
  })

  async function apply() {
    busy = true
    error = ''
    try {
      // Trust the server's echo over the form: it is what is on disk, and what the
      // republish now running in the background is generating from.
      adopt(await saveDomains(payload))
      applied = true
      setTimeout(() => (applied = false), 4000)
    } catch (e) {
      // The API's messages are written for the operator; show that, not the
      // request line the client wraps around anything without one.
      error = message(e)
    } finally {
      busy = false
    }
  }

  function revert() {
    list = (JSON.parse(saved) as Domain[]).map(toDraft)
    error = ''
  }

  const addDomain = () => (list = [...list, { name: '', host: '', directives: [] }])
  const removeDomain = (i: number) => (list = list.filter((_, n) => n !== i))
  const addDirective = (d: Draft) => (d.directives = [...d.directives, { key: '', value: '' }])
  const removeDirective = (d: Draft, i: number) => (d.directives = d.directives.filter((_, n) => n !== i))
</script>

<section class="card">
  <header>
    <h3>{$t('domains')}</h3>
    <p class="hint">{$t('domains_hint')}</p>
  </header>

  {#if loading}
    <p class="note">{$t('loading')}</p>
  {:else}
    <!-- The domain everything else is added to. Read-only on purpose: it is set by
         the deployment in .env.app, and each app's own compose routes to it. -->
    <div class="primary">
      <div class="ptop">
        <span class="badge">{$t('main_domain')}</span>
        <code class="phost">{primaryHost || $t('main_domain_unset')}</code>
        <code class="ptoken">{primaryToken}</code>
      </div>
      <p class="hint">{$t('main_domain_hint')}</p>
      <pre class="preview">caddy_0: {SAMPLE}-{primaryToken}{primaryHost ? `   → ${SAMPLE}-${primaryHost}` : ''}</pre>
    </div>

    <h4 class="sub">{$t('extra_domains')}</h4>
    <p class="hint">
      {$t('extra_domains_hint')}
      {#each Object.keys(vars) as v (v)}<code class="var">$&#123;{v}&#125;</code>{/each}
    </p>
    <p class="hint">{$t('directives_hint')}</p>

    {#if list.length}
      <ul class="rows">
        {#each list as d, i (i)}
          <li class="row">
            <div class="top">
              <input
                class="name"
                placeholder={$t('domain_name')}
                aria-label={$t('domain_name')}
                bind:value={d.name}
                spellcheck="false"
                autocapitalize="off"
              />
              <input
                class="host"
                placeholder="lan.example.com"
                aria-label={$t('domain_host')}
                bind:value={d.host}
                spellcheck="false"
                autocapitalize="off"
              />
              <button class="trash" aria-label={$t('remove')} disabled={busy} onclick={() => removeDomain(i)}>✕</button>
            </div>

            <div class="directives">
              <span class="dlabel">{$t('directives')}</span>
              {#each d.directives as dir, j (j)}
                <div class="drow">
                  <input
                    class="dkey"
                    placeholder="import"
                    aria-label={$t('directive_key')}
                    bind:value={dir.key}
                    spellcheck="false"
                    autocapitalize="off"
                  />
                  <input
                    class="dval"
                    placeholder="gateway_tls"
                    aria-label={$t('directive_value')}
                    bind:value={dir.value}
                    spellcheck="false"
                    autocapitalize="off"
                  />
                  <button class="trash" aria-label={$t('remove')} disabled={busy} onclick={() => removeDirective(d, j)}>✕</button>
                </div>
              {/each}
              <button class="chip" disabled={busy} onclick={() => addDirective(d)}>+ {$t('add_directive')}</button>
            </div>

            <!-- What this entry actually writes into every app's
                 docker-compose.override.yml. The index is allocated per app, after
                 the highest one that app already uses, so it is shown as N. -->
            <pre class="preview">caddy_N: {SAMPLE}-{d.host || '…'}{#each d.directives as dir}{#if dir.key.trim()}
caddy_N.{dir.key.trim()}: {dir.value.trim()}{/if}{/each}
caddy_N.reverse_proxy: "&#123;&#123;upstreams 80&#125;&#125;"{#if d.host && resolve(d.host) !== d.host}
   → {SAMPLE}-{resolve(d.host)}{/if}</pre>
          </li>
        {/each}
      </ul>
    {:else}
      <p class="empty">{$t('domains_empty')}</p>
    {/if}

    <div class="add">
      <button class="chip" disabled={busy} onclick={addDomain}>+ {$t('add_domain')}</button>
    </div>

    <footer class="actions">
      <button class="go" disabled={busy || !dirty || !complete || duplicate} onclick={apply}>
        {busy ? '…' : $t('save_recreate')}
      </button>
      {#if dirty}
        <button class="ghost" disabled={busy} onclick={revert}>{$t('revert')}</button>
      {/if}
      {#if busy}
        <span class="note">{$t('republishing')}</span>
      {:else if applied}
        <span class="note">{$t('domains_applied')}</span>
      {:else if dirty}
        <span class="note">{$t('domains_apply_hint')}</span>
      {/if}
    </footer>

    {#if duplicate}<p class="err">{$t('domains_duplicate')}</p>{/if}
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
    margin: 0;
    font-size: 0.8rem;
    font-weight: 600;
    color: var(--grey-600);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .hint {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
  .var {
    font-size: 0.75rem;
    background: hsla(208, 16%, 94%, 1);
    border-radius: 4px;
    padding: 0.05rem 0.3rem;
    margin-right: 0.3rem;
    white-space: nowrap;
  }
  .primary {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    padding: 0.85rem 1rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
    background: hsla(208, 16%, 97%, 1);
  }
  .ptop {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.5rem;
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
  }
  .phost {
    font-size: 0.85rem;
    color: var(--grey-800);
  }
  .ptoken {
    font-size: 0.75rem;
    color: var(--grey-600);
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
  .directives {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.4rem;
    padding-left: 0.1rem;
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
  input {
    min-width: 0;
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    padding: 0 0.5rem;
    font-size: 0.85rem;
  }
  .name {
    flex: 0 0 9rem;
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
  .empty {
    margin: 0;
    font-size: 0.8rem;
    color: var(--grey-600);
  }
  .trash {
    border: none;
    background: none;
    color: var(--red);
    font-size: 0.85rem;
    cursor: pointer;
    flex: none;
  }
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
