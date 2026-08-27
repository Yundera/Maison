<script lang="ts">
  import type { Snippet } from 'svelte'
  let {
    title = '',
    arrow = false,
    onarrowclick,
    children,
  }: {
    title?: string
    arrow?: boolean
    /** When given, the arrow becomes a button opening the widget's detail view.
     *  Without it the arrow stays the decoration it has always been. */
    onarrowclick?: () => void
    children?: Snippet
  } = $props()
</script>

<section class="widget">
  <div class="glass"></div>
  <div class="widget-content">
    {#if title}
      <header class="widget-title">
        <span>{title}</span>
        {#if arrow}
          {#if onarrowclick}
            <button class="arrow" aria-label={title} onclick={onarrowclick}>›</button>
          {:else}
            <span class="arrow">›</span>
          {/if}
        {/if}
      </header>
    {/if}
    {@render children?.()}
  </div>
</section>

<style>
  .widget {
    position: relative;
    border-radius: var(--radius-card);
    margin-bottom: 1rem;
    transition: box-shadow 0.2s;
  }
  .widget:hover {
    box-shadow: 0 0 17px 0 rgba(0, 0, 0, 0.2);
  }
  .widget-content {
    position: relative;
    z-index: 1;
    padding: 1rem 1.25rem;
  }
  .widget-title {
    color: var(--grey-100);
    font-size: 1.125rem;
    line-height: 1.75rem;
    font-weight: 500;
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 0.5rem;
  }
  .arrow {
    color: var(--grey-400);
    font-size: 1.25rem;
  }
  button.arrow {
    background: none;
    border: none;
    padding: 0 0.25rem;
    line-height: 1;
    cursor: pointer;
    transition:
      color 0.15s,
      transform 0.15s;
  }
  button.arrow:hover {
    color: var(--grey-100);
    transform: translateX(2px);
  }
</style>
