<script lang="ts">
  /**
   * Settings › Notifications — what is wrong with the box, and where it gets sent.
   *
   * Three cards, in the order the questions are asked: what is wrong now, where
   * should I be told, and what should I not be told about.
   *
   * The incident list comes first deliberately. Mail is secondary here and always
   * will be: a PCS relays through whatever the deployment gave it, into a consumer
   * mailbox that may file it as spam, and none of that is visible from this box. The
   * page and the badge are the surface that cannot silently fail.
   */
  import { t } from '../../i18n'
  import { settings } from '../../stores/settings'
  import {
    incidents,
    subscribeIncidents,
    loadIncidents,
    ackIncident,
    resolveIncident,
    muteKind,
    sendTestNotification,
    KNOWN_KINDS,
    type Incident,
  } from '../../stores/incidents'

  let busy = $state(false)
  let error = $state('')
  let note = $state('')
  let showResolved = $state(false)

  loadIncidents()
  $effect(() => subscribeIncidents())

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  async function act(fn: () => Promise<unknown>, ok = '') {
    busy = true
    error = ''
    note = ''
    try {
      await fn()
      note = ok
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }

  const test = () => act(sendTestNotification, $t('notifications_test_sent'))

  // The SMTP block rides the debounced /api/settings save, like every other
  // preference on this page — `settings.update` is the save. It is a pointer on the
  // server (`*notify.SMTP`) so that "deliberately emptied" stays distinguishable from
  // "never set", which is why this seeds a whole object rather than a field at a time.
  const smtp = $derived(
    $settings.smtp ?? { host: '', port: 587, from: '', to: '', user: '', pass: '', security: '' },
  )

  function setSmtp(patch: Partial<typeof smtp>) {
    settings.update((s) => ({ ...s, smtp: { ...smtp, ...patch } }))
  }

  /** The localised label for an incident, falling back to the server's English title.
   *  A kind reported by another PCS component through the inbound API has no
   *  translation here and never will, so the fallback is the normal path for those
   *  rather than an error case. */
  function label(inc: Incident): string {
    const key = `incident_${inc.kind}`
    const out = $t(key, inc.args ?? {})
    return out === key ? inc.title : out
  }

  function when(iso: string): string {
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
  }

  const open = $derived($incidents.open)
  const recent = $derived($incidents.recent)
  // Kinds this build knows, plus anything already seen that it does not — so an
  // incident raised by another component becomes mutable once it has happened.
  const kinds = $derived([
    ...new Set([
      ...KNOWN_KINDS,
      ...open.map((i) => i.kind),
      ...recent.map((i) => i.kind),
      ...Object.keys($incidents.muted),
    ]),
  ])
</script>

<header class="head">
  <h3>{$t('notifications')}</h3>
  <p class="hint">{$t('notifications_hint')}</p>
</header>

<section class="card">
  <h4>{$t('notifications_open')}</h4>
  {#if open.length === 0}
    <p class="empty">{$t('notifications_all_clear')}</p>
  {:else}
    <ul class="list">
      {#each open as inc (inc.id)}
        <li class="inc" class:acked={inc.acked}>
          <span class="dot" class:critical={inc.severity === 'critical'}></span>
          <div class="body">
            <p class="title">{label(inc)}</p>
            {#if inc.detail}<p class="detail">{inc.detail}</p>{/if}
            <p class="meta">{$t('notifications_since', { when: when(inc.since) })}</p>
          </div>
          <div class="row-actions">
            {#if !inc.acked}
              <button disabled={busy} onclick={() => act(() => ackIncident(inc.id))}>
                {$t('notifications_ack')}
              </button>
            {/if}
            <button disabled={busy} onclick={() => act(() => resolveIncident(inc.id))}>
              {$t('notifications_dismiss')}
            </button>
          </div>
        </li>
      {/each}
    </ul>
  {/if}

  {#if recent.length > 0}
    <button class="link" onclick={() => (showResolved = !showResolved)}>
      {showResolved ? $t('notifications_hide_history') : $t('notifications_show_history', { n: String(recent.length) })}
    </button>
    {#if showResolved}
      <ul class="list history">
        {#each recent as inc (inc.id + inc.resolved)}
          <li class="inc">
            <span class="dot done"></span>
            <div class="body">
              <p class="title">{label(inc)}</p>
              <p class="meta">{$t('notifications_cleared', { when: when(inc.resolved ?? '') })}</p>
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  {/if}
</section>

<section class="card">
  <h4>{$t('notifications_mail')}</h4>
  <p class="hint">{$t('notifications_mail_hint')}</p>

  {#if !$incidents.mail_configured}
    <p class="warn">{$t('notifications_mail_unconfigured')}</p>
  {/if}

  <label class="row">
    <span>{$t('notifications_smtp_host')}</span>
    <input
      value={smtp.host}
      disabled={busy}
      placeholder={$t('notifications_smtp_host_placeholder')}
      oninput={(e) => setSmtp({ host: e.currentTarget.value })}
    />
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_port')}</span>
    <input
      class="short"
      type="number"
      min="1"
      max="65535"
      value={smtp.port || 587}
      disabled={busy}
      oninput={(e) => setSmtp({ port: Number(e.currentTarget.value) })}
    />
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_security')}</span>
    <select value={smtp.security || 'starttls'} disabled={busy} onchange={(e) => setSmtp({ security: e.currentTarget.value })}>
      <option value="starttls">STARTTLS</option>
      <option value="tls">TLS</option>
      <option value="none">{$t('notifications_smtp_security_none')}</option>
    </select>
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_user')}</span>
    <input value={smtp.user ?? ''} disabled={busy} oninput={(e) => setSmtp({ user: e.currentTarget.value })} />
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_pass')}</span>
    <input
      type="password"
      value={smtp.pass ?? ''}
      disabled={busy}
      oninput={(e) => setSmtp({ pass: e.currentTarget.value })}
    />
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_to')}</span>
    <input value={smtp.to ?? ''} disabled={busy} oninput={(e) => setSmtp({ to: e.currentTarget.value })} />
  </label>
  <label class="row">
    <span>{$t('notifications_smtp_from')}</span>
    <input value={smtp.from ?? ''} disabled={busy} oninput={(e) => setSmtp({ from: e.currentTarget.value })} />
  </label>
  <p class="hint">{$t('notifications_smtp_inherit_hint')}</p>

  <div class="actions">
    <button disabled={busy} onclick={test}>{$t('notifications_test')}</button>
  </div>
  {#if error}<p class="err">{error}</p>{/if}
  {#if note}<p class="ok">{note}</p>{/if}
</section>

<section class="card">
  <h4>{$t('notifications_mute')}</h4>
  <p class="hint">{$t('notifications_mute_hint')}</p>
  <div class="kinds">
    {#each kinds as kind (kind)}
      <label class="check">
        <input
          type="checkbox"
          checked={!$incidents.muted[kind]}
          disabled={busy}
          onchange={(e) => act(() => muteKind(kind, !e.currentTarget.checked))}
        />
        {$t(`incident_${kind}_name`) === `incident_${kind}_name` ? kind : $t(`incident_${kind}_name`)}
      </label>
    {/each}
  </div>
</section>

<style>
  .head {
    max-width: 46rem;
    margin-bottom: 1rem;
  }
  .card {
    max-width: 46rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
    margin-bottom: 1.25rem;
  }
  h3 {
    margin: 0 0 0.25rem;
    font-size: 1.05rem;
    font-weight: 600;
    color: var(--text);
  }
  h4 {
    margin: 0 0 0.25rem;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--text);
  }
  .hint,
  .empty {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .warn {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--warning, #d29922);
  }
  .err {
    margin: 0.5rem 0 0;
    font-size: 0.85rem;
    color: var(--red);
  }
  .ok {
    margin: 0.5rem 0 0;
    font-size: 0.85rem;
    color: var(--text-muted);
  }

  .list {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .inc {
    display: flex;
    align-items: flex-start;
    gap: 0.7rem;
    padding: 0.7rem 0;
    border-top: 1px solid var(--border);
  }
  .inc:first-child {
    border-top: 0;
  }
  .inc.acked {
    opacity: 0.6;
  }
  /* Same 8px dot the tiles use for app status, so "something is wrong" reads the
     same way wherever it appears. */
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    margin-top: 0.35rem;
    flex: none;
    background: var(--warning, #d29922);
  }
  .dot.critical {
    background: var(--red);
  }
  .dot.done {
    background: var(--border);
  }
  .body {
    flex: 1;
    min-width: 0;
  }
  .title {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text);
  }
  .detail {
    margin: 0.2rem 0 0;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
    /* The detail is written as prose with real paragraph breaks — what went wrong,
       then what to do about it — and collapsing them would run the two together. */
    white-space: pre-wrap;
  }
  .meta {
    margin: 0.25rem 0 0;
    font-size: 0.78rem;
    color: var(--text-muted);
  }
  .row-actions {
    display: flex;
    gap: 0.35rem;
    flex: none;
  }
  .history {
    margin-top: 0.5rem;
  }

  .row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin: 0.5rem 0;
  }
  .row > span:first-child {
    min-width: 9rem;
    font-size: 0.9rem;
  }
  .row input,
  .row select {
    flex: 1;
    min-width: 0;
  }
  .row input.short {
    flex: none;
    width: 6rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.3rem 0;
    font-size: 0.9rem;
    color: var(--text);
  }
  .kinds {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));
    gap: 0.2rem 1rem;
  }

  input,
  select {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.35rem 0.5rem;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.9rem;
  }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    font-size: 0.85rem;
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .link {
    background: none;
    border: 0;
    padding: 0.4rem 0;
    color: var(--text-muted);
    text-decoration: underline;
    font-size: 0.85rem;
  }
</style>
