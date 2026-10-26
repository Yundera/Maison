<script lang="ts">
  // What this box offers apps, as the store judges it (Settings › App stores).
  //
  // The store hides an app that requires a capability the box lacks — a game server
  // on a box nobody can dial into — so the judgement has to be visible, and
  // correctable: detection reads the box's own addresses, and cannot see a port
  // forward on a home router. Each capability is auto (detected), yes or no. A
  // deployment that knows better (Yundera knows its VPSes have a public address)
  // manages the setting, and the card is then read-only.
  import { onMount } from 'svelte'
  import { fetchCapabilities, saveCapabilities, type CapabilitiesView } from '../../stores/store'
  import { requirementName, formatSize } from '../../requirements'
  import { t } from '../../i18n'

  let view = $state<CapabilitiesView | null>(null)
  let busy = $state(false)
  let error = $state('')

  const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

  onMount(() => load(false))
  async function load(refresh: boolean) {
    error = ''
    try {
      view = await fetchCapabilities(refresh)
    } catch (e) {
      error = message(e)
    }
  }

  async function set(name: string, value: 'auto' | 'yes' | 'no') {
    if (!view) return
    const overrides: Record<string, string> = {}
    for (const c of view.capabilities) {
      const v = c.name === name ? value : c.setting
      if (v !== 'auto') overrides[c.name] = v
    }
    busy = true
    error = ''
    try {
      view = await saveCapabilities(overrides)
    } catch (e) {
      error = message(e)
    } finally {
      busy = false
    }
  }
</script>

<section class="card">
  <header>
    <h3>{$t('caps_title')}</h3>
    <p class="hint">{$t('caps_hint')}</p>
  </header>

  {#if view}
    <dl class="facts">
      <div><dt>{$t('req_architectures')}</dt><dd>{view.arch}</dd></div>
      <div><dt>{$t('req_cpu')}</dt><dd>{view.cpu || '—'}</dd></div>
      <div><dt>{$t('req_memory')}</dt><dd>{view.memory ? formatSize(view.memory) : '—'}</dd></div>
      <div><dt>{$t('caps_disk_free')}</dt><dd>{view.disk_free ? formatSize(view.disk_free) : '—'}</dd></div>
    </dl>

    {#if view.managedBy}
      <p class="hint">{$t('settings_managed_by', { owner: view.managedBy })}</p>
    {/if}

    <fieldset class="lock" disabled={!!view.managedBy || busy}>
      <ul class="rows">
        {#each view.capabilities as c (c.name)}
          <li class="row">
            <div class="meta">
              <span class="name">{requirementName(c.name, $t)}</span>
              <span class="detected" title={c.detected.source}>
                {$t('caps_detected')}: <strong class={c.detected.value}>{$t(`caps_${c.detected.value}`)}</strong>
                {#if c.detected.source}<span class="src">— {c.detected.source}</span>{/if}
              </span>
            </div>
            <div class="segmented" role="radiogroup" aria-label={requirementName(c.name, $t)}>
              {#each ['auto', 'yes', 'no'] as const as v (v)}
                <button
                  class:active={c.setting === v}
                  role="radio"
                  aria-checked={c.setting === v}
                  onclick={() => set(c.name, v)}>{$t(`caps_set_${v}`)}</button
                >
              {/each}
            </div>
          </li>
        {/each}
      </ul>
    </fieldset>

    <p class="hint">
      {$t('caps_footer')}
      <button class="link" onclick={() => load(true)}>{$t('caps_detect_again')}</button>
    </p>
  {:else if !error}
    <p class="hint">{$t('loading')}</p>
  {/if}

  {#if error}<p class="err">{error}</p>{/if}
</section>

<style>
  .card {
    margin-top: 1.5rem;
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
  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem 1.5rem;
    margin: 0;
  }
  .facts div {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }
  dt {
    font-size: 0.72rem;
    color: var(--grey-600);
  }
  dd {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--grey-800);
  }
  /* Only there to disable every control at once when the setting is managed. */
  .lock {
    border: 0;
    margin: 0;
    padding: 0;
    min-width: 0;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.75rem;
    padding: 0.6rem 0.85rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 8px;
    background: hsla(208, 16%, 97%, 1);
  }
  .meta {
    flex: 1;
    min-width: 12rem;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .name {
    font-size: 0.875rem;
    color: var(--grey-800);
  }
  .detected {
    font-size: 0.75rem;
    color: var(--grey-600);
  }
  .detected .yes {
    color: hsl(118, 55%, 32%);
  }
  .detected .no {
    color: hsl(6, 60%, 45%);
  }
  .src {
    overflow-wrap: anywhere;
  }
  .link {
    background: none;
    border: none;
    padding: 0;
    font: inherit;
    color: var(--primary);
    cursor: pointer;
  }
  .err {
    margin: 0;
    font-size: 0.8rem;
    color: var(--red);
  }
  fieldset:disabled .segmented {
    opacity: 0.6;
  }
</style>
