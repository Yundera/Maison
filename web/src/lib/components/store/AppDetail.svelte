<script lang="ts">
  import {
    fetchStoreApp,
    fetchStoreSources,
    type StoreApp,
    type StoreSource,
  } from '../../stores/store'
  import { t } from '../../i18n'
  import { renderMarkdown } from '../../markdown'
  import InstallButton from './InstallButton.svelte'
  import { refOf, type StoreRef } from '../../storeref'

  let {
    ref,
    installed = false,
    onback,
  }: { ref: StoreRef; installed?: boolean; onback: () => void } = $props()

  let app = $state<StoreApp | null>(null)
  let loading = $state(true)
  let error = $state('')
  let sources = $state<StoreSource[] | null>(null) // null until known — don't warn early

  $effect(() => {
    loading = true
    // A reference with a locator (from /store/<locator>/-/<apps>/<id>) pins the
    // lookup to one store, which may be a store the user has not added; without
    // one the merged catalog answers.
    fetchStoreApp(ref)
      .then((a) => (app = a))
      .catch((e) => (error = String(e)))
      .finally(() => (loading = false))
  })

  // A deep link can point at a store that is not one of the configured sources.
  // That still installs, but the user should see whose app they are about to run.
  $effect(() => {
    fetchStoreSources()
      .then((s) => (sources = s.sources))
      .catch(() => (sources = []))
  })

  const unlisted = $derived(
    !!app?.store && !!sources && !sources.some((s) => s.url === app!.store),
  )
</script>

<div class="detail">
  <button class="back" onclick={onback}>‹ {$t('back')}</button>

  {#if loading}
    <p class="muted">{$t('loading')}</p>
  {:else if app}
    {#if unlisted}
      <div class="warning" role="alert">
        <span class="sign">⚠</span>
        <div>
          <strong>{$t('unlisted_store')}</strong>
          <span class="src" title={app.store}>{app.store_name || app.store}</span>
          <p class="hint">{$t('unlisted_store_hint')}</p>
        </div>
      </div>
    {/if}

    <header class="app-header">
      <img class="plate" src={app.icon} alt="" />
      <div class="meta">
        <h1>{app.name}</h1>
        <p class="tagline">{app.tagline}</p>
        <InstallButton ref={refOf(app)} {installed} size="normal" />
      </div>
    </header>

    <nav class="info">
      <div class="item">
        <span class="label">{$t('category')}</span>
        <span class="value">{app.category || '—'}</span>
      </div>
      <div class="item">
        <span class="label">{$t('developer')}</span>
        <span class="value">{app.developer || '—'}</span>
      </div>
      {#if app.min_memory}
        <div class="item">
          <span class="label">Require memory</span>
          <span class="value">{app.min_memory} MB</span>
        </div>
      {/if}
    </nav>

    {#if app.screenshots?.length}
      <div class="shots">
        {#each app.screenshots as s}
          <img src={s} alt="screenshot" loading="lazy" />
        {/each}
      </div>
    {/if}

    {#if app.description}
      <!-- eslint-disable-next-line svelte/no-at-html-tags -->
      <div class="description markdown">{@html renderMarkdown(app.description)}</div>
    {/if}
  {:else}
    <p class="error">{error || 'Not found'}</p>
  {/if}
</div>

<style>
  .detail {
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
    color: var(--grey-800);
  }
  .back {
    align-self: flex-start;
    background: none;
    border: none;
    color: var(--primary);
    font-size: 0.95rem;
    cursor: pointer;
    padding: 0;
  }
  /* Shown when the app comes from a store that is not one of the user's sources
     — a deep link can carry any store URL. */
  .warning {
    display: flex;
    gap: 0.6rem;
    align-items: flex-start;
    padding: 0.7rem 0.9rem;
    border-radius: 8px;
    background: hsla(45, 100%, 51%, 0.14);
    border: 1px solid hsla(45, 90%, 40%, 0.35);
    color: hsl(38, 62%, 28%);
    font-size: 0.85rem;
  }
  .warning .sign {
    font-size: 1rem;
    line-height: 1.2;
  }
  .warning strong {
    font-weight: 600;
  }
  .warning .src {
    margin-left: 0.35rem;
    font-family: monospace;
    font-size: 0.8rem;
    /* A store that ships no store.json is named by its URL, which is long and has
       no spaces to break on. Wrapping beats overflowing the warning. */
    overflow-wrap: anywhere;
  }
  .warning .hint {
    margin: 0.15rem 0 0;
    color: hsl(38, 35%, 35%);
  }
  .app-header {
    display: flex;
    gap: 1.5rem;
    align-items: center;
    padding-bottom: 1rem;
    border-bottom: 1px solid #cfcfcf;
  }
  .plate {
    width: 128px;
    height: 128px;
    flex: none;
    object-fit: cover;
    border-radius: 18.75%;
    background: linear-gradient(180deg, #f7fafc 0%, #f0f2f5 100%);
    box-shadow: 1px 2px 4px rgba(0, 0, 0, 0.2);
  }
  .meta {
    flex: 1;
    min-width: 0;
  }
  h1 {
    margin: 0 0 0.4rem;
    font-size: 1.5rem;
    font-weight: 600;
    color: #29343d;
  }
  .tagline {
    margin: 0 0 0.9rem;
    color: hsl(0, 0%, 45%);
    font-size: 0.9rem;
  }
  .info {
    display: flex;
    gap: 3rem;
    padding: 0.5rem 0 1.25rem;
    border-bottom: 1px solid #cfcfcf;
  }
  .item {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    text-align: center;
  }
  .label {
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: hsl(0, 0%, 48%);
  }
  .value {
    font-size: 0.95rem;
    color: #29343d;
  }
  .shots {
    display: flex;
    gap: 1.5rem;
    overflow-x: auto;
    padding-bottom: 0.5rem;
    scroll-snap-type: x mandatory;
  }
  .shots img {
    flex: 0 0 calc(33.333% - 1rem);
    min-width: 280px;
    aspect-ratio: 16 / 9;
    object-fit: cover;
    border-radius: 8px;
    scroll-snap-align: start;
  }
  .description {
    color: #3a444d;
    font-size: 0.875rem;
    line-height: 1.5rem;
  }
  .markdown :global(h3),
  .markdown :global(h4),
  .markdown :global(h5) {
    color: #29343d;
    font-weight: 600;
    margin: 1rem 0 0.4rem;
  }
  .markdown :global(p) {
    margin: 0 0 0.75rem;
  }
  .markdown :global(ul) {
    margin: 0 0 0.75rem;
    padding-left: 1.25rem;
  }
  .markdown :global(li) {
    margin: 0.2rem 0;
  }
  .markdown :global(code) {
    background: hsla(208, 16%, 94%, 1);
    padding: 0 0.25rem;
    border-radius: 4px;
    font-size: 0.82rem;
  }
  .markdown :global(a) {
    color: var(--primary);
  }
  .markdown :global(strong) {
    color: #29343d;
  }
  .muted {
    color: var(--text-subtle);
  }
  .error {
    color: var(--red);
  }
</style>
