<script lang="ts">
  /**
   * What "Back up now" is actually doing.
   *
   * This used to be one line — `A backup is running — app:immich` — and that was the
   * whole of it. It named a compose project rather than an app, it gave no way to tell
   * whether that was the first target of two or the eighth of nine, and it showed
   * nothing at all about how far into the app it was, because the run called the
   * untracked backup path and no progress ever left the engine.
   *
   * The plan now arrives before the first target starts, so the list below is complete
   * from the moment the button is pressed: finished targets keep their result, the
   * running one is expanded, and the rest are visibly waiting. Three things are worth
   * more than the numbers:
   *
   *  - The app's own name and icon, resolved here from the app list, because "immich"
   *    is what the user installed and "app:immich" is what the scheduler calls it.
   *  - Whether the app is *stopped right now*. That is the `sync` phase, it is the only
   *    part of a backup the user can feel, and its ETA is how much longer it lasts.
   *  - That an estimate is absent rather than zero. An engine reports what it can, and
   *    for the opening seconds of a snapshot that is nothing — so the bar goes
   *    indeterminate instead of sitting at 0% claiming 0 B/s.
   */
  import { apps as appsStore } from '../../stores/apps'
  import { targetsDone, type RunState, type TargetState } from '../../stores/backupengine'
  import { renderSize, renderRate, renderDuration, renderElapsed } from '../../format'
  import { t } from '../../i18n'

  let { run }: { run: RunState } = $props()

  const targets = $derived(run.targets ?? [])
  const done = $derived(targetsDone(run))

  // Ticks the elapsed clock. The run state itself only arrives when something
  // changes, and a target that is quietly copying for ten minutes changes nothing —
  // so without this the elapsed time would freeze and read as a stalled backup.
  let now = $state(Date.now())
  $effect(() => {
    if (!run.running) return
    const id = setInterval(() => (now = Date.now()), 1000)
    return () => clearInterval(id)
  })

  /** The app's installed name, falling back to the compose project. The user-data
   *  target is not an app and has no tile, which is exactly why it needs naming here. */
  function label(tg: TargetState): string {
    if (tg.kind === 'userdata') return $t('backup_target_user_data')
    return $appsStore.find((a) => a.id === tg.app)?.name ?? tg.app ?? tg.id
  }

  function icon(tg: TargetState): string | undefined {
    if (tg.kind === 'userdata') return undefined
    return $appsStore.find((a) => a.id === tg.app)?.icon
  }

  /** Plain words for the step, from the same phase vocabulary the tiles use. */
  function phaseLabel(tg: TargetState): string {
    switch (tg.phase) {
      case 'sync':
        return $t('backup_phase_sync')
      case 'start':
        return $t('backup_phase_start')
      case 'compress':
        return $t('compressing')
      case 'copy':
        return $t('backup_phase_copy')
      default:
        return $t('backup_phase_preparing')
    }
  }

  /** The byte line: "4.2 GB of 11 GB · 38 MB/s · about 3 min left".
   *
   *  Assembled from whatever is actually known rather than from a fixed template with
   *  blanks in it — an engine that counts no bytes still gets its ETA shown, and one
   *  that has not measured a rate yet does not get "0 B/s". */
  function detail(tg: TargetState): string {
    const bits: string[] = []
    if (tg.total) bits.push($t('backup_of_size', { done: renderSize(tg.done ?? 0), total: renderSize(tg.total) }))
    const rate = renderRate(tg.rate)
    if (rate) bits.push(rate)
    const eta = renderDuration(tg.eta)
    if (eta) bits.push($t('backup_time_left', { time: eta }))
    return bits.join(' · ')
  }

  /** How long a finished target took, for the row that is already done. */
  function took(tg: TargetState): string {
    return renderElapsed(tg.started, tg.finished)
  }

  // The overall bar counts targets, not bytes. Weighting it by size would need every
  // target measured up front — a full tree walk per app, and the user-data set is
  // where the terabytes are — to make a number that is still an estimate. Counting is
  // honest about what it is.
  const overall = $derived(targets.length ? (done / targets.length) * 100 : 0)
  const elapsed = $derived.by(() => {
    void now // re-read on every tick
    return renderElapsed(run.started, run.running ? undefined : run.finished)
  })
</script>

<div class="run" class:live={run.running}>
  <div class="head">
    <strong>
      {run.running
        ? $t('backup_run_progress', { done: String(done), total: String(targets.length) })
        : run.failures > 0
          ? $t('backup_last_run_failed')
          : $t('backup_last_run_ok')}
    </strong>
    {#if elapsed}
      <span class="elapsed">{$t('backup_elapsed', { time: elapsed })}</span>
    {/if}
  </div>

  {#if run.running}
    <div class="bar overall"><div class="fill" style="width:{overall}%"></div></div>
  {/if}

  <ul class="targets">
    {#each targets as tg (tg.id)}
      <li class="target {tg.status}">
        <div class="row">
          <span class="mark" aria-hidden="true">
            {#if tg.status === 'done'}✓{:else if tg.status === 'failed'}!{:else if tg.status === 'skipped'}–{:else if tg.status === 'running'}▸{:else}·{/if}
          </span>
          {#if icon(tg)}
            <img class="ico" src={icon(tg)} alt="" />
          {/if}
          <span class="name">{label(tg)}</span>
          <span class="state">
            {#if tg.status === 'running'}
              {phaseLabel(tg)}
            {:else if tg.status === 'done'}
              {took(tg)}
            {:else if tg.status === 'failed'}
              {$t('backup_target_failed')}
            {:else if tg.status === 'skipped'}
              {$t('backup_target_skipped')}
            {:else}
              {$t('backup_target_pending')}
            {/if}
          </span>
        </div>

        {#if tg.status === 'running'}
          <!-- Indeterminate until the engine has something to report. A bar pinned at
               0% is indistinguishable from a bar that is stuck. -->
          <div class="bar" class:unknown={tg.pct < 0} title={tg.message ?? ''}>
            <div class="fill" style={tg.pct >= 0 ? `width:${tg.pct}%` : ''}></div>
          </div>
          {#if detail(tg)}
            <p class="detail">{detail(tg)}</p>
          {/if}
          {#if tg.phase === 'sync'}
            <!-- The one part of a backup the user can feel. Worth saying outright,
                 and worth saying only while it is true. -->
            <p class="down">{$t('backup_app_stopped', { app: label(tg) })}</p>
          {/if}
        {:else if tg.status === 'failed'}
          <p class="err">{tg.error}</p>
        {:else if tg.status === 'skipped' && tg.error}
          <!-- The reason, in the muted colour: nothing went wrong here. -->
          <p class="detail">{tg.error}</p>
        {/if}
      </li>
    {/each}
  </ul>
</div>

<style>
  .run {
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem 0.9rem;
    margin: 0 0 1rem;
  }
  .head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 0.75rem;
    font-size: 0.9rem;
    margin-bottom: 0.5rem;
  }
  .elapsed {
    font-size: 0.8rem;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
  .bar {
    height: 4px;
    border-radius: 999px;
    background: var(--border);
    overflow: hidden;
  }
  .bar.overall {
    margin-bottom: 0.7rem;
  }
  .fill {
    height: 100%;
    background: var(--primary);
    transition: width 0.4s ease;
  }
  /* No measurable progress: a slice that travels, so "working" and "stuck" do not
     look the same. */
  .bar.unknown .fill {
    width: 30%;
    animation: slide 1.4s ease-in-out infinite;
  }
  @keyframes slide {
    0% {
      transform: translateX(-100%);
    }
    100% {
      transform: translateX(333%);
    }
  }
  .targets {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .target {
    padding: 0.35rem 0;
    border-top: 1px solid var(--border);
  }
  .target:first-child {
    border-top: 0;
  }
  .target.pending {
    opacity: 0.55;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.85rem;
  }
  .mark {
    width: 1em;
    text-align: center;
    color: var(--text-muted);
  }
  .target.done .mark {
    color: var(--green, #3fb950);
  }
  .target.failed .mark {
    color: var(--red);
  }
  .target.skipped {
    opacity: 0.75;
  }
  .ico {
    width: 18px;
    height: 18px;
    border-radius: 4px;
    object-fit: cover;
  }
  .name {
    font-weight: 600;
  }
  .state {
    margin-left: auto;
    color: var(--text-muted);
    font-size: 0.8rem;
    font-variant-numeric: tabular-nums;
  }
  .detail,
  .down,
  .err {
    margin: 0.3rem 0 0;
    font-size: 0.78rem;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
  .down {
    color: var(--amber, #d29922);
  }
  .err {
    color: var(--red);
  }
</style>
