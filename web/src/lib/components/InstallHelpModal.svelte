<script lang="ts">
  // The iOS half of installing. Safari on iPhone and iPad can add a web app to the
  // home screen but exposes no API for it — there is no event to listen for and no
  // method to call — so the only thing that helps is showing the user which button
  // to press. The Share glyph is drawn inline next to the first step for exactly
  // that reason: the word "Share" is not what they are looking for on the screen.
  import { installHelp } from '../stores/pwa'
  import { t } from '../i18n'

  const close = () => installHelp.set(false)
</script>

<div class="backdrop" onclick={close} role="presentation">
  <div class="dialog" onclick={(e) => e.stopPropagation()} role="presentation">
    <h2>{$t('install_ios_title')}</h2>

    <ol>
      <li>
        <!-- The flex box is an inner span, not the <li>: display:flex on a list item
             drops its marker AND its slot in the counter, so step 1 loses its number
             and the next two become 1. and 2. -->
        <span class="step">
          {$t('install_ios_step_share')}
          <svg class="share" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M12 15V3M8.5 6.5 12 3l3.5 3.5" />
            <path d="M7 11H5.5A1.5 1.5 0 0 0 4 12.5v7A1.5 1.5 0 0 0 5.5 21h13a1.5 1.5 0 0 0 1.5-1.5v-7A1.5 1.5 0 0 0 18.5 11H17" />
          </svg>
        </span>
      </li>
      <li>{$t('install_ios_step_add')}</li>
      <li>{$t('install_ios_step_confirm')}</li>
    </ol>

    <p class="note">{$t('install_ios_note')}</p>

    <div class="actions">
      <button class="ghost" onclick={close}>{$t('back')}</button>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 110;
    background: var(--scrim);
    display: grid;
    place-items: center;
  }
  .dialog {
    width: min(92vw, 26rem);
    background: #fff;
    border-radius: 14px;
    padding: 1.25rem 1.4rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.1rem;
  }
  ol {
    margin: 0;
    padding-left: 1.2rem;
    font-size: 0.9rem;
    line-height: 1.5;
  }
  li {
    margin-bottom: 0.5rem;
  }
  .step {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
  }
  .share {
    flex: none;
    color: #007aff; /* iOS system blue, so the glyph reads as the one on screen */
  }
  .note {
    margin: 0.9rem 0 0;
    color: var(--text-subtle);
    font-size: 0.85rem;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 1.1rem;
  }
  .actions button {
    padding: 0.5rem 1.1rem;
    border-radius: 8px;
    border: none;
    font-size: 0.875rem;
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
</style>
