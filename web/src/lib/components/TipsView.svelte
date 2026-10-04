<script lang="ts">
  import { renderMarkdown } from '../markdown'

  // An app's tips as the operator reads them: Markdown, rendered, with the ${VAR}
  // references already resolved by the server (renderTips). Shared by the tile's
  // Tips dialog and the Settings → Tips page so the two never render differently.
  let { tips }: { tips: string } = $props()
</script>

<div class="tips markdown">{@html renderMarkdown(tips, { breaks: true })}</div>

<style>
  .tips {
    min-height: 0;
    overflow: auto;
    padding: 0.75rem 0.85rem;
    background: var(--surface-2);
    border-radius: 8px;
    font-size: 0.85rem;
    line-height: 1.5;
    word-break: break-word;
  }
  .markdown :global(h3),
  .markdown :global(h4),
  .markdown :global(h5) {
    color: var(--text);
    font-weight: 600;
    margin: 0.9rem 0 0.4rem;
  }
  .markdown :global(:first-child) {
    margin-top: 0;
  }
  .markdown :global(p) {
    margin: 0 0 0.6rem;
  }
  .markdown :global(:last-child) {
    margin-bottom: 0;
  }
  .markdown :global(ul) {
    margin: 0 0 0.6rem;
    padding-left: 1.25rem;
  }
  .markdown :global(li) {
    margin: 0.2rem 0;
  }
  .markdown :global(code) {
    background: var(--border);
    padding: 0 0.25rem;
    border-radius: 4px;
    font-size: 0.82rem;
  }
  .markdown :global(pre) {
    margin: 0 0 0.6rem;
    padding: 0.6rem 0.7rem;
    overflow-x: auto;
    background: var(--border);
    border-radius: 6px;
  }
  .markdown :global(pre code) {
    padding: 0;
    background: none;
  }
  .markdown :global(table) {
    width: 100%;
    margin: 0 0 0.6rem;
    border-collapse: collapse;
    font-size: 0.82rem;
  }
  .markdown :global(th),
  .markdown :global(td) {
    padding: 0.35rem 0.5rem;
    text-align: left;
    border: 1px solid var(--border);
  }
  .markdown :global(th) {
    color: var(--text);
    font-weight: 600;
    background: var(--surface-sunken);
  }
  .markdown :global(a) {
    color: var(--primary);
  }
  .markdown :global(strong) {
    color: var(--text);
  }
</style>
