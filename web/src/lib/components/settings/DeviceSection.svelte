<script lang="ts">
  /**
   * Settings › This device — the settings that belong to this browser rather than
   * to the box.
   *
   * Every other section on this page configures the server, and the page's own doc
   * comment says so. This one is the deliberate exception, which is why it is
   * named for the device and opens by saying what it is: without the distinction
   * drawn out loud, an install button among the domain and backup settings reads
   * as something that changed the server.
   *
   * Unlike the entry in the TopBar menu, the card here is ALWAYS rendered. Someone
   * who navigated to a page called "This device" is asking whether they can
   * install, and a page that answers by showing nothing has not answered.
   */
  import { t } from '../../i18n'
  import {
    canInstall,
    installed,
    installHelp,
    installHost,
    isFallbackHost,
    isIOSSafari,
    promptInstall,
  } from '../../stores/pwa'
</script>

<header class="head">
  <h3>{$t('settings_device')}</h3>
  <p class="hint">{$t('settings_device_hint')}</p>
</header>

<section class="card">
  <h4>{$installed ? $t('install_installed') : $t('install_app')}</h4>

  {#if $installed}
    <p class="hint">{$t('install_installed_hint')}</p>
  {:else}
    <p class="hint">{$t('install_app_hint')}</p>

    {#if $canInstall}
      <p class="hint host">{$t('install_host', { host: installHost })}</p>
      {#if isFallbackHost}
        <p class="warn">{$t('install_host_fallback', { host: installHost })}</p>
      {/if}
      <div class="run">
        <button class="primary" onclick={() => promptInstall()}>{$t('install_now')}</button>
      </div>
    {:else if isIOSSafari}
      <!-- Shown inline rather than behind the dialog: this is a page the user
           navigated to on purpose, so making them open a second thing to read
           three sentences would be worse. The dialog exists for the TopBar entry,
           where there is no room. -->
      <ol class="steps">
        <li>{$t('install_ios_step_share')}</li>
        <li>{$t('install_ios_step_add')}</li>
        <li>{$t('install_ios_step_confirm')}</li>
      </ol>
      <p class="hint">{$t('install_ios_note')}</p>
      <div class="run">
        <button class="ghost" onclick={() => installHelp.set(true)}>{$t('install_show_how')}</button>
      </div>
    {:else}
      <p class="hint">{$t('install_unavailable')}</p>
    {/if}
  {/if}
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
  .hint {
    margin: 0 0 0.5rem;
    color: var(--text-subtle);
    font-size: 0.85rem;
    line-height: 1.5;
  }
  .host {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .warn {
    margin: 0 0 0.5rem;
    padding: 0.5rem 0.7rem;
    border-radius: 8px;
    background: hsla(38, 92%, 50%, 0.12);
    color: var(--text);
    font-size: 0.85rem;
    line-height: 1.5;
  }
  .steps {
    margin: 0 0 0.5rem;
    padding-left: 1.2rem;
    color: var(--text-subtle);
    font-size: 0.85rem;
    line-height: 1.6;
  }
  .run {
    margin-top: 0.75rem;
  }
  button {
    padding: 0.5rem 1.1rem;
    border-radius: 8px;
    border: none;
    font-size: 0.875rem;
    cursor: pointer;
  }
  .primary {
    background: var(--primary);
    color: var(--text-on-accent);
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
</style>
