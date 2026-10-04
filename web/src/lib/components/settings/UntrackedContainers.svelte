<script lang="ts">
  /**
   * Settings › Resources › Apps — the containers no app accounts for.
   *
   * The usage table above it is one row per app; this is everything else on the
   * host, so the page accounts for every container: a compose project Maison
   * neither installed nor recognises, or a container started with a plain
   * `docker run`. Read-only on purpose — none of them is an app, so there is no
   * lifecycle Maison could start, stop or remove one within.
   *
   * Mounted only while the Apps tab is showing, so it polls only then.
   */
  import { onMount } from 'svelte'
  import { fetchUntracked, type UntrackedContainer } from '../../stores/storage'
  import { t } from '../../i18n'

  let list = $state<UntrackedContainer[] | null>(null)
  let error = $state('')

  async function load() {
    try {
      list = await fetchUntracked()
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  onMount(() => {
    void load()
    const poll = setInterval(load, 10_000)
    return () => clearInterval(poll)
  })
</script>

<h4>{$t('untracked_title')}</h4>
<p class="hint">{$t('untracked_hint')}</p>
{#if error}
  <p class="error">{error}</p>
{:else if !list}
  <p class="hint">{$t('loading')}</p>
{:else if !list.length}
  <p class="hint">{$t('untracked_empty')}</p>
{:else}
  <div class="scroll">
    <table>
      <thead>
        <tr>
          <th>{$t('untracked_col_name')}</th>
          <th>{$t('untracked_col_image')}</th>
          <th>{$t('untracked_col_state')}</th>
          <th>{$t('untracked_col_project')}</th>
        </tr>
      </thead>
      <tbody>
        {#each list as c (c.id)}
          <tr>
            <td class="mono" title={c.id}>{c.name || c.id}</td>
            <td class="mono dim">{c.image}</td>
            <td>
              <span class="state" class:running={c.state === 'running'}></span>
              {c.status || c.state}
            </td>
            <td class="mono dim">{c.project ? (c.service ? `${c.project} / ${c.service}` : c.project) : '—'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  h4 {
    margin: 0 0 0.25rem;
    font-size: 0.9rem;
  }
  .hint {
    color: var(--text-muted);
    font-size: 0.8rem;
    margin: 0 0 0.5rem;
  }
  .error {
    color: var(--red);
    font-size: 0.8rem;
    margin: 0;
  }
  .scroll {
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.8rem;
  }
  th {
    text-align: left;
    font-weight: 500;
    font-size: 0.75rem;
    color: var(--text-muted);
    padding: 0 0.5rem 0.35rem;
    border-bottom: 1px solid var(--border);
  }
  td {
    padding: 0.4rem 0.5rem;
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  .mono {
    font-family: ui-monospace, monospace;
  }
  .dim {
    color: var(--text-muted);
  }
  .state {
    display: inline-block;
    width: 0.5rem;
    height: 0.5rem;
    margin-right: 0.35rem;
    border-radius: 50%;
    background: var(--text-muted);
  }
  .state.running {
    background: var(--green);
  }
</style>
