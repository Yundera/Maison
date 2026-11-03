<script lang="ts">
  import { settings } from '../../stores/settings'
  import { t } from '../../i18n'

  // Whether the store lists apps declaring `audience: technical` — root shells,
  // host-level and agent tooling — like any other. Off, they are hidden the way an
  // app this box cannot run is: under "Show all", and found by searching for them.
  function toggle() {
    settings.update((s) => ({ ...s, show_technical_apps: !s.show_technical_apps }))
  }
</script>

<section class="card">
  <label class="row">
    <input type="checkbox" checked={$settings.show_technical_apps} onchange={toggle} />
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
    padding: 1rem 1.5rem;
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
    font-size: 0.95rem;
    margin-bottom: 0.2rem;
  }
  .hint {
    display: block;
    font-size: 0.8rem;
    line-height: 1.45;
    color: var(--grey-600);
  }
</style>
