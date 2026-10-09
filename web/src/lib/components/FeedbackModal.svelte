<script lang="ts">
  // The feedback form. Who reads it is whoever operates this box — the dialog names
  // them, because the user should know where their words are going before they write
  // them. What Maison attaches is listed too: nothing leaves quietly.
  import { feedback, feedbackOpen, sendFeedback } from '../stores/feedback'
  import { t } from '../i18n'

  const close = () => feedbackOpen.set(false)

  // Where the user was when they opened it — the one piece of context worth having
  // that they did not type.
  const page = location.pathname

  let category = $state('idea')
  let message = $state('')
  let sending = $state(false)
  let sent = $state(false)
  let error = $state('')

  const limit = $derived($feedback.maxLength ?? 5000)
  const length = $derived([...message.trim()].length)
  const canSend = $derived(!sending && length > 0 && length <= limit)

  async function submit(e: Event) {
    e.preventDefault()
    if (!canSend) return
    sending = true
    error = ''
    try {
      await sendFeedback(message, category, page)
      sent = true
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    } finally {
      sending = false
    }
  }
</script>

<div class="backdrop" onclick={close} role="presentation">
  <form class="dialog" onclick={(e) => e.stopPropagation()} onsubmit={submit}>
    <h2>{$t('feedback_title')}</h2>

    {#if sent}
      <p>{$t('feedback_sent', { operator: $feedback.operator ?? '' })}</p>
      <div class="actions">
        <button type="button" class="primary" onclick={close}>{$t('back')}</button>
      </div>
    {:else}
      <p class="lead">{$t('feedback_lead', { operator: $feedback.operator ?? '' })}</p>

      <div class="categories" role="radiogroup" aria-label={$t('feedback_category')}>
        {#each $feedback.categories ?? ['idea', 'bug', 'other'] as c}
          <label class="chip" class:active={category === c}>
            <input type="radio" name="category" value={c} bind:group={category} />
            {$t(`feedback_category_${c}`)}
          </label>
        {/each}
      </div>

      <label class="message">
        <textarea bind:value={message} rows="6" placeholder={$t('feedback_placeholder')} disabled={sending}></textarea>
        <span class="count" class:over={length > limit}>{length} / {limit}</span>
      </label>

      <p class="note">{$t('feedback_attached', { version: $feedback.version ?? '', page })}</p>
      {#if $feedback.privacyNote}<p class="note">{$feedback.privacyNote}</p>{/if}
      {#if $feedback.supportUrl}
        <p class="note">
          <a href={$feedback.supportUrl} target="_blank" rel="noopener">{$t('feedback_support_instead')}</a>
        </p>
      {/if}

      {#if error}<p class="error">{error}</p>{/if}

      <div class="actions">
        <button type="button" class="ghost" onclick={close}>{$t('cancel')}</button>
        <button type="submit" class="primary" disabled={!canSend}>
          {sending ? $t('feedback_sending') : $t('feedback_send')}
        </button>
      </div>
    {/if}
  </form>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 110;
    display: grid;
    place-items: center;
    background: var(--scrim);
  }
  .dialog {
    width: min(92vw, 28rem);
    background: #fff;
    border-radius: 14px;
    padding: 1.25rem 1.4rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    color: var(--grey-800);
  }
  h2 {
    margin: 0;
    font-size: 1.1rem;
  }
  p {
    margin: 0;
    font-size: 0.9rem;
    line-height: 1.45;
  }
  .categories {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .chip {
    padding: 0.3rem 0.75rem;
    border-radius: 999px;
    background: hsla(208, 16%, 94%, 1);
    font-size: 0.8rem;
    cursor: pointer;
  }
  .chip.active {
    background: var(--primary);
    color: #fff;
  }
  .chip input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }
  .message {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  textarea {
    padding: 0.5rem 0.6rem;
    border: 1px solid hsla(208, 16%, 85%, 1);
    border-radius: 8px;
    font: inherit;
    font-size: 0.9rem;
    resize: vertical;
  }
  .count {
    align-self: flex-end;
    font-size: 0.75rem;
    color: var(--text-subtle);
  }
  .count.over {
    color: var(--red);
  }
  .note {
    color: var(--text-subtle);
    font-size: 0.8rem;
  }
  .error {
    color: var(--red);
    font-size: 0.85rem;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.25rem;
  }
  .actions button {
    padding: 0.5rem 1.1rem;
    border-radius: 8px;
    border: none;
    font-size: 0.875rem;
  }
  .actions button:disabled {
    opacity: 0.5;
  }
  .ghost {
    background: hsla(208, 16%, 94%, 1);
    color: var(--grey-800);
  }
  .primary {
    background: var(--primary);
    color: #fff;
  }
</style>
