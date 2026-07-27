<script lang="ts">
  // The domains apps are published on. Was DomainsField, in the TopBar dropdown;
  // the logic is unchanged, the layout is no longer squeezed into 17rem.
  import { settings, saveDomains, type Domain } from '../../stores/settings'
  import { t } from '../../i18n'

  // The two domains a Yundera box answers on besides its own: both are wildcard
  // DNS services that encode the public IP in the hostname, so an app stays
  // reachable with no DNS set up at all. They differ only in TLS — nip.io goes
  // through the gateway's certificate, sslip.io gets its own from Let's Encrypt —
  // which is exactly what `directives` carries.
  const PRESETS: Domain[] = [
    { name: 'sslip', domain: '${APP_PUBLIC_IP_DASH}.sslip.io' },
    { name: 'nip', domain: '${APP_PUBLIC_IP_DASH}.nip.io', directives: { import: 'gateway_tls' } },
  ]

  let busy = $state(false)
  let error = $state('')
  let adding = $state(false)
  let name = $state('')
  let host = $state('')

  const current = $derived($settings.domains ?? [])
  const unused = $derived(PRESETS.filter((p) => !current.some((d) => d.domain === p.domain)))

  async function apply(list: Domain[]) {
    busy = true
    error = ''
    try {
      await saveDomains(list)
    } catch (e) {
      // The API's messages are written for the operator; show that, not the
      // request line the client wraps around anything without one.
      error = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
  }

  function add(d: Domain) {
    apply([...current, d])
  }

  function remove(d: Domain) {
    apply(current.filter((x) => x.domain !== d.domain))
  }

  function addCustom() {
    if (!name.trim() || !host.trim()) return
    add({ name: name.trim(), domain: host.trim() })
    name = ''
    host = ''
    adding = false
  }
</script>

<section class="card">
  <header>
    <h3>{$t('domains')}</h3>
    <p class="hint">{$t('domains_hint')}</p>
  </header>

  {#if current.length}
    <ul class="rows">
      {#each current as d (d.domain)}
        <li class="row">
          <span class="name">{d.name}</span>
          <code title={d.domain}>{d.domain}</code>
          <button class="trash" aria-label={$t('remove')} disabled={busy} onclick={() => remove(d)}>✕</button>
        </li>
      {/each}
    </ul>
  {:else}
    <p class="empty">{$t('domains_empty')}</p>
  {/if}

  <div class="add">
    {#each unused as p (p.domain)}
      <button class="chip" disabled={busy} onclick={() => add(p)}>+ {p.name}</button>
    {/each}
    {#if !adding}
      <button class="chip" disabled={busy} onclick={() => (adding = true)}>+ {$t('custom')}</button>
    {/if}
  </div>

  {#if adding}
    <div class="custom">
      <input placeholder={$t('domain_name')} bind:value={name} />
      <input placeholder="lan.example.com" bind:value={host} onkeydown={(e) => e.key === 'Enter' && addCustom()} />
      <button class="go" disabled={busy} onclick={addCustom}>{busy ? '…' : $t('add')}</button>
      <button class="ghost" onclick={() => (adding = false)}>{$t('cancel')}</button>
    </div>
  {/if}

  {#if busy}<p class="note">{$t('republishing')}</p>{/if}
  {#if error}<p class="err">{error}</p>{/if}
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
  .hint {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.55rem 0;
    border-bottom: 1px solid hsla(208, 16%, 94%, 1);
  }
  .row:first-child {
    border-top: 1px solid hsla(208, 16%, 94%, 1);
  }
  .name {
    flex: 0 0 8rem;
    color: var(--grey-800);
    font-size: 0.875rem;
  }
  code {
    flex: 1;
    min-width: 0;
    font-size: 0.8rem;
    color: var(--grey-600);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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
  .custom {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
  }
  .custom input {
    min-width: 0;
    flex: 1;
    height: 2rem;
    border: 1px solid #cfcfcf;
    border-radius: 4px;
    padding: 0 0.5rem;
    font-size: 0.85rem;
  }
  .go {
    border: none;
    background: var(--primary);
    color: #fff;
    border-radius: 6px;
    padding: 0 0.9rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .ghost {
    border: 1px solid #cfcfcf;
    background: #fff;
    color: var(--grey-600);
    border-radius: 6px;
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
