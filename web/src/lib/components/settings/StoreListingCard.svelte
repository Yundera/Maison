<script lang="ts">
  import { settings } from '../../stores/settings'
  import { t } from '../../i18n'

  // What the store's browse leaves out, as two box settings. Search always lists
  // everything, greyed with the reason, whatever these say.
  //
  // - show_unsupported_apps: apps this box cannot run (a missing capability such
  //   as a public IP or a GPU, another architecture). This used to be a
  //   per-browser "Show hidden apps" checkbox in the store toolbar; the toolbar now
  //   only says how many are hidden and links here.
  // - show_technical_apps: apps declaring `audience: technical` — root shells,
  //   host-level and agent tooling.
  type Key = 'show_unsupported_apps' | 'show_technical_apps'
  function toggle(key: Key) {
    settings.update((s) => ({ ...s, [key]: !s[key] }))
  }
</script>

<section class="card">
  <h3>{$t('store_listing_title')}</h3>
  <label class="row">
    <input
      type="checkbox"
      checked={$settings.show_unsupported_apps}
      onchange={() => toggle('show_unsupported_apps')}
    />
    <span>
      <strong>{$t('show_unsupported_apps')}</strong>
      <span class="hint">{$t('show_unsupported_apps_hint')}</span>
    </span>
  </label>
  <label class="row">
    <input type="checkbox" checked={$settings.show_technical_apps} onchange={() => toggle('show_technical_apps')} />
    <span>
      <strong>{$t('show_technical_apps')}</strong>
      <span class="hint">{$t('show_technical_apps_hint')}</span>
    </span>
  </label>
</section>

<style>
  .card {
    margin-top: 1.5rem;
    max-width: 46rem;
    border: 1px solid hsla(208, 16%, 90%, 1);
    border-radius: 10px;
    padding: 1.25rem 1.5rem;
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: #29343d;
  }
  .row {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    cursor: pointer;
  }
  .row input {
    margin-top: 0.2rem;
  }
  strong {
    display: block;
    font-size: 0.875rem;
    font-weight: 500;
    margin-bottom: 0.2rem;
  }
  .hint {
    display: block;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
</style>
