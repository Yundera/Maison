<script lang="ts">
  /**
   * Settings › Backups — one page for the whole feature.
   *
   * It used to be two sections. `backups` listed archives and `cloud` configured the
   * engine, which put the schedule one click away from its own output and implied that
   * a "cloud backup" is a different kind of thing. It is not: there is one backup
   * mechanism, and the engine — local disk or a remote repository — is a setting it
   * reads. That rule still holds and the word "cloud" still does not appear here.
   *
   * What changed since is the *order*, and the split between what is global and what
   * belongs to an engine.
   *
   *   1. STATE FIRST. The question this page is opened with is "am I protected, and
   *      when was the last backup?" It used to be unanswerable: the page opened with a
   *      dropdown and the only state on it was "No backups yet" seven hundred pixels
   *      down. The band at the top answers it before anything can be configured.
   *
   *   2. THE RUN IS GLOBAL. When it happens, whether it happens, and whether it
   *      includes the user's own files. Global because there is exactly one Scheduler
   *      and it serialises by stopping containers — two per-engine schedules would be
   *      two things stopping the same app on two different nights.
   *
   *   3. EVERYTHING ELSE BELONGS TO AN ENGINE, so it lives in that engine's tab.
   *      Retention above all: tiers are handed to a repository's own policy engine and
   *      mean twenty-three full copies on a local disk, so one set of numbers for the
   *      box was a number that was either inert or dangerous depending on which engine
   *      you had. The encryption key too — it is
   *      AppDataShared/backup/<engine>/repository.password, it exists only for an
   *      engine that has a repository, and showing it above a tab holding unencrypted
   *      folders read as a promise that covered them.
   *
   * The list stays engine-agnostic (the server merges every engine, see backup.Set), so
   * a backup restores from wherever it actually is rather than from whichever engine
   * happens to be selected today.
   *
   * THERE IS NO SAVE BUTTON. Every control commits when it changes. The button was the
   * page's most reported bug — you ticked "run backups automatically", read the
   * sentence next to it, and left with the schedule still off — and no amount of
   * dirty-state decoration fixes a control that lies about having taken effect.
   */
  import { t } from '../../i18n'
  import {
    fetchBackupStatus,
    saveBackupConfig,
    runBackupNow,
    emailBackupKey,
    showBackupKey,
    type BackupStatus,
    type BackupConfig,
    type EngineInfo,
    type EngineSettings,
  } from '../../stores/backupengine'
  import {
    fetchAllBackups,
    restoreBackup,
    deleteBackup,
    engineLabel,
    renderStamp,
    type EngineBackups,
    type Backup,
  } from '../../stores/backups'
  import { backupLive, subscribeBackup } from '../../stores/backuplive'
  import { renderSize } from '../../format'
  import BackupRows from '../BackupRows.svelte'
  import RunPanel from './RunPanel.svelte'
  import UserDataCard from './UserDataCard.svelte'

  /** A plain deep copy of a value that may be reactive.
   *
   *  Both halves matter. structuredClone alone throws on a $state proxy — "could not
   *  be cloned" — and $state.snapshot alone returns a shallow-frozen view, while a
   *  spread would leave conf.keep and conf.engines aliasing the fetched status so
   *  every edit silently rewrote the value being compared against. */
  const clone = <T,>(v: T): T => structuredClone($state.snapshot(v)) as T

  let status = $state<BackupStatus | null>(null)
  let conf = $state<BackupConfig | null>(null)
  let busy = $state(false)
  let error = $state('')
  let savedAt = $state(0)

  let engines = $state<EngineBackups[]>([])
  let tab = $state('')
  let free = $state<number | null>(null)
  let loading = $state(true)
  let listError = $state('')

  async function loadStatus() {
    try {
      status = await fetchBackupStatus()
      conf = clone(status.config)
    } catch (e) {
      error = (e as Error).message
    }
  }

  // Deliberately the expensive read — it measures folder archives, which is a tree
  // walk each — because it is what answers "what is eating the disk" and this page
  // is opened by hand.
  async function loadArchives() {
    loading = true
    try {
      const r = await fetchAllBackups()
      engines = r.engines ?? []
      free = r.free ?? null
      // Keep whichever tab is open across a reload — this reloads on a poll while a
      // restore runs, and a tab that jumped back to the default mid-restore would take
      // the progress off screen. Otherwise open the engine that receives new backups,
      // which is the one the user is most likely to be asking about.
      if (!engines.some((e) => e.engine === tab)) {
        tab = engines.some((e) => e.engine === status?.active)
          ? (status?.active ?? '')
          : (engines[0]?.engine ?? '')
      }
      listError = ''
    } catch (e) {
      listError = e instanceof Error ? e.message : String(e)
    } finally {
      loading = false
    }
  }

  loadStatus()
  loadArchives()

  // Run and restore progress arrive on the live channel while this page is open.
  //
  // It used to poll /api/backup/status every two seconds instead, which was wrong
  // twice over: that endpoint probes every engine's repository — a subprocess for a
  // remote one — so it was never something to call on a timer, and a bar fed at 0.5 Hz
  // looks broken however good the numbers behind it are.
  $effect(() => subscribeBackup())

  // The live payload is the authority on the run while the socket is up; the fetched
  // status still provides it on first paint, before the first push arrives.
  const runState = $derived($backupLive?.run ?? status?.run ?? null)

  // Deliberately a plain `let`, not $state: the effect below both reads and writes it,
  // and a reactive flag would re-trigger the effect on its own write.
  let wasRunning = false

  // When the run finishes, the archive list is stale by definition, so reload it:
  // "Back up now" that leaves the list unchanged reads as a backup that did nothing.
  // The engine status is re-read too, since it is what the rest of this page renders
  // from and the live channel deliberately does not carry it.
  $effect(() => {
    const running = !!runState?.running
    if (wasRunning && !running) {
      loadArchives()
      loadStatus()
    }
    wasRunning = running
  })

  // A user-data restore has no tile to carry a progress bar, so this card is the only
  // place it shows, and polling the archive list is what refreshes it.
  //
  // Slower than the run poll above on purpose: this read asks every engine, which for a
  // remote one is a subprocess per call, and it measures folder archives on the way. The
  // message it is polling for changes once per restored folder, so a tighter loop would
  // spend a container start per tick to re-render the same sentence.
  $effect(() => {
    // Any engine's restore, not just the open tab's: there is one restore at a time on
    // the box, and switching tabs while it runs must not stop the polling that reports
    // it. The restore's *progress* now arrives live; this poll is only what keeps the
    // archive list itself current, so it stays slow.
    const running = $backupLive?.restore.running ?? engines.some((e) => e.user_data.restore.running)
    if (!running) return
    const id = setInterval(loadArchives, 5000)
    return () => clearInterval(id)
  })

  // --- committing -----------------------------------------------------------------
  //
  // Every control calls this. PUT /api/backup/config replaces the whole document with
  // no merge — which is what makes a plain bool expressible — so the patch is applied
  // to a clone of the current configuration and the whole thing is sent.
  //
  // A failed write restores what was on screen before it. Leaving the new value there
  // under an error message is the worst of both: the control says the setting is one
  // thing and the box does another, which is exactly the failure the Save button used
  // to cause by accident.
  async function commit(patch: Partial<BackupConfig>) {
    if (!conf) return
    const before = conf
    const next = { ...clone(conf), ...patch }
    conf = next
    busy = true
    error = ''
    try {
      conf = clone(await saveBackupConfig(next))
      savedAt = Date.now()
      // The resolved retention, "receives", and the next run are all derived from the
      // configuration server-side, so they are re-read rather than guessed at here.
      await loadStatus()
    } catch (e) {
      conf = before
      error = (e as Error).message
    } finally {
      busy = false
    }
  }

  /** Writes one engine's own settings, leaving every other engine's untouched. */
  function commitEngine(id: string, patch: Partial<EngineSettings>) {
    const engines = clone(conf?.engines ?? {})
    engines[id] = { ...(engines[id] ?? {}), ...patch }
    return commit({ engines })
  }

  /** Writes a trigger tick, freezing the ticks that are currently in force.
   *
   *  A box that has never been configured runs on a fallback: whichever engine it was
   *  provisioned with receives everything, with none of that written down anywhere. The
   *  first tick anyone sets switches the box off that fallback — so writing ONLY the box
   *  that was clicked would silently turn off every engine that had been receiving
   *  implicitly. Ticking "also keep a local copy" would have quietly stopped the offsite
   *  backups, which is the single worst thing this page could do.
   *
   *  So a trigger write always sends the whole picture: every engine's current effective
   *  answer, with the click applied on top. What the checkboxes show is what gets
   *  stored. */
  function commitTrigger(id: string, patch: { schedule?: boolean; uninstall?: boolean }) {
    const engines = clone(conf?.engines ?? {})
    for (const e of status?.engines ?? []) {
      const mine = e.id === id
      engines[e.id] = {
        ...(engines[e.id] ?? {}),
        schedule: mine && patch.schedule !== undefined ? patch.schedule : e.receives_schedule,
        uninstall: mine && patch.uninstall !== undefined ? patch.uninstall : e.receives_uninstall,
      }
    }
    return commit({ engines })
  }

  // Shown for a couple of seconds and only next to what was actually saved. A
  // permanent "Saved" is indistinguishable from a stale one.
  //
  // A timeout rather than an interval: there is exactly one moment to re-render at,
  // and a ticker left running would keep waking the page for the rest of the session
  // to recompute a boolean that stopped changing two seconds in.
  const SAVED_FOR = 2500
  let now = $state(Date.now())
  $effect(() => {
    if (!savedAt) return
    const id = setTimeout(() => (now = Date.now()), SAVED_FOR)
    return () => clearTimeout(id)
  })
  const justSaved = $derived(savedAt > 0 && now - savedAt < SAVED_FOR)

  const runNow = async () => {
    busy = true
    error = ''
    try {
      await runBackupNow()
    } catch (e) {
      error = (e as Error).message
    } finally {
      busy = false
    }
  }

  // --- the key --------------------------------------------------------------------
  // Shown on demand rather than with the rest of the page: this is the one secret on
  // the box that has no recovery path, and it has no business being on screen behind
  // whoever walks past while its owner is reading about retention tiers.
  let key = $state('')
  let copied = $state(false)
  let keyBusy = $state(false)
  let keyNote = $state('')

  async function toggleKey() {
    if (key) {
      key = ''
      copied = false
      return
    }
    keyBusy = true
    error = ''
    try {
      key = (await showBackupKey()).key
    } catch (e) {
      error = (e as Error).message
    } finally {
      keyBusy = false
    }
  }

  async function sendKey() {
    keyBusy = true
    error = ''
    keyNote = ''
    try {
      await emailBackupKey()
      keyNote = $t('backup_key_sent')
      await loadStatus()
    } catch (e) {
      error = (e as Error).message
    } finally {
      keyBusy = false
    }
  }

  // Copying is offered as well as selecting because the key is a long random string
  // and a half-selected one fails silently — the restore that needs it happens months
  // later, on a different machine, with no way to tell a wrong key from a lost one.
  async function copyKey() {
    try {
      await navigator.clipboard.writeText(key)
      copied = true
    } catch {
      // Not available on an insecure origin. Selecting the text still works, so this
      // is a missing convenience rather than a failure worth an error banner.
      copied = false
    }
  }

  /** When a copy of the key was last mailed, for the line under the buttons. */
  const keySentAt = $derived(
    status?.key_sent ? new Date(status.key_sent.sent_at).toLocaleString() : '',
  )

  // --- the protection band ----------------------------------------------------------

  const engineInfo = $derived(
    Object.fromEntries((status?.engines ?? []).map((e) => [e.id, e])) as Record<string, EngineInfo>,
  )

  /** The engine that receives new backups, and whether it can actually write. */
  const writer = $derived(status ? engineInfo[status.active] : undefined)
  const misconfigured = $derived(!!writer && !writer.connected)

  /** The user-data set as the *writing* engine sees it. Deliberately not the open
   *  tab's: the schedule writes to the default engine whichever tab is on screen, so
   *  reading this off the tab made a statement about the run change when the user
   *  clicked something that cannot affect it. */
  const writerUserData = $derived(engines.find((e) => e.engine === status?.active)?.user_data)

  /** The newest backup an engine holds, from the listing rather than from the run.
   *
   *  Rendered from the stamp rather than parsed into a Date: stamps carry the server's
   *  local time and no zone, so constructing a Date would silently shift them. */
  function newestIn(e: EngineBackups): string {
    let best = ''
    for (const g of e.apps) if (g.backups[0] && g.backups[0].stamp > best) best = g.backups[0].stamp
    const ud = e.user_data.backups[0]
    if (ud && ud.stamp > best) best = ud.stamp
    return best
  }

  /** One row per engine — the protection summary.
   *
   *  A row per destination rather than one sentence about the box, because that is what
   *  independent engines actually are: a night where the repository failed and the local
   *  archive succeeded has no single honest verdict, and the four facts this replaced
   *  ("last backup", "offsite copy", "next run", "last scheduled run") existed only
   *  because they had nowhere else to live.
   *
   *  It also stops the page scolding the common case. A verdict built around "is there
   *  an offsite copy" tells every default install — local engine, no repository, which
   *  is how a PCS ships — that it is failing, permanently. Offsite is a fact about an
   *  engine, not a grade for the server. */
  type EngineRow = {
    id: string
    label: string
    level: 'ok' | 'warn' | 'bad' | 'idle'
    last: string
    note: string
    receives: boolean
  }

  const rows = $derived.by((): EngineRow[] => {
    if (!status) return []
    // Whether the archive listing has arrived. Until it has, a row cannot tell "nothing
    // has ever been written here" from "not asked yet", and asserting the first is the
    // same lie the banner above used to tell — worse here, because it is per engine and
    // reads as data loss. Asking a repository is a subprocess, so this is seconds, not a
    // flicker.
    const known = !loading || engines.length > 0
    return (status.engines ?? []).map((e) => {
      const held = engines.find((x) => x.engine === e.id)
      const last = held ? newestIn(held) : ''
      const receives = e.receives_schedule || e.receives_uninstall
      let level: EngineRow['level'] = 'ok'
      let note = ''
      if (!known) {
        return { id: e.id, label: label(e.id, e.name), level: 'idle', last: '', note: '', receives }
      }
      if (!receives) {
        // Not a fault. An engine can hold history without being written to any more,
        // and saying "no backups" about it would read as data loss.
        level = 'idle'
        note = last ? $t('backup_row_history_only') : $t('backup_row_unused')
      } else if (!e.connected) {
        level = 'bad'
        note = e.detail || $t('backup_row_unreachable')
      } else if (e.last_run?.failed) {
        level = 'bad'
        note = $t('backup_row_failing')
      } else if (!last) {
        level = 'warn'
        note = $t('backup_row_nothing_yet')
      } else if (!conf?.enabled && e.receives_schedule) {
        level = 'warn'
        note = $t('backup_row_manual_only')
      }
      return { id: e.id, label: label(e.id, e.name), level, last, note, receives }
    })
  })

  /** The one sentence above the rows, and only when something is actually wrong.
   *
   *  A healthy box gets its rows and no lecture. This is the opposite of the verdict it
   *  replaced, which graded every install against having an offsite copy and so had
   *  something disappointed to say about the default one forever. */
  /** Whether the archive listing has arrived — see the note in `rows`. */
  const listKnown = $derived(!loading || engines.length > 0)

  const alert = $derived.by(() => {
    if (!status) return null
    if (loading && !engines.length) return { level: 'checking', text: $t('backup_state_checking') }
    if (!rows.some((r) => r.receives)) return { level: 'bad', text: $t('backup_state_no_destination') }
    if (rows.some((r) => r.level === 'bad')) return { level: 'bad', text: $t('backup_state_destination_failing') }
    if (rows.every((r) => !r.receives || !r.last)) return { level: 'bad', text: $t('backup_state_none') }
    if (!conf?.enabled) return { level: 'warn', text: $t('backup_state_manual_only') }
    return null
  })

  /** The next run, as a local time. Unlike a stamp this is a real RFC3339 instant with
   *  a zone, so it is safe to hand to Date — and it already carries this box's jitter,
   *  which is why it is asked of the server rather than computed from hour:minute. */
  const nextRun = $derived(status?.next_run ? new Date(status.next_run).toLocaleString() : '')

  // --- the open tab -----------------------------------------------------------------

  /** The open tab's archives, falling back to the first engine so the page renders
   *  during the window between the engine list arriving and a tab being chosen. */
  const active = $derived(engines.find((e) => e.engine === tab) ?? engines[0] ?? null)
  /** …and the same engine's status, which carries retention, encryption and health. */
  const activeInfo = $derived(active ? engineInfo[active.engine] : undefined)

  const label = (id: string, name?: string) => engineLabel(id, name, (k) => $t(k))

  /** The retention modes worth offering for this engine.
   *
   *  Tier modes are left out where the storage cannot afford them — the server would
   *  collapse them to a count anyway, and offering a choice that is silently rewritten
   *  is worse than not offering it. */
  const modes = $derived(
    activeInfo?.retention?.tiered
      ? ['smart', 'custom', 'count', 'age', 'all']
      : ['count', 'age', 'all'],
  )

  /** What the open engine's own settings say, as opposed to what resolved for it. The
   *  select shows the resolved mode; writing one stores it against this engine. */
  const ownMode = $derived(conf?.engines?.[activeInfo?.id ?? '']?.mode ?? '')
  const shownMode = $derived(ownMode || activeInfo?.retention?.mode || '')
  const inherited = $derived(!ownMode && (activeInfo?.retention?.source ?? '') !== 'engine')

  const keepOf = $derived(
    conf?.engines?.[activeInfo?.id ?? '']?.keep ??
      activeInfo?.retention?.keep ?? { latest: 2, daily: 7, weekly: 4, monthly: 12, annual: 0 },
  )

  function setMode(id: string, mode: string) {
    // Clearing the mode is how "follow the deployment" is expressed — the layer below
    // decides again, rather than today's numbers being frozen in as an override.
    if (!mode) return commitEngine(id, { mode: '', keep: undefined, count: undefined, max_age_days: undefined })
    const patch: Partial<EngineSettings> = { mode }
    if (mode === 'custom') patch.keep = { ...keepOf }
    if (mode === 'count') patch.count = activeInfo?.retention?.count || 2
    if (mode === 'age') patch.max_age_days = activeInfo?.retention?.max_age_days || 30
    return commitEngine(id, patch)
  }

  // --- archive actions ----------------------------------------------------------
  async function run(fn: () => Promise<void>) {
    busy = true
    listError = ''
    try {
      await fn()
    } catch (e) {
      listError = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
    await loadArchives()
  }

  // The engine travels with the row: two engines can hold the same stamp, and each
  // copy is restored and deleted on its own.
  const restore = (app: string, b: Backup) => run(() => restoreBackup(app, b.name, b.engine))
  const remove = (app: string, b: Backup) => run(() => deleteBackup(app, b.name, b.engine ?? ''))
</script>

<header class="head">
  <h3>{$t('backups')}</h3>
  <p class="hint">{$t('backups_hint')}</p>
</header>

<!-- ── Where your backups are ─────────────────────────────────────────────────────
     Before any control, because it is the question the page is opened with and it used
     to be the one thing the page could not answer. One row per destination: engines are
     independent, so a night where one succeeded and another did not has no single
     honest verdict. -->
<section class="state">
  {#if alert}
    <p class="verdict {alert.level}">{alert.text}</p>
  {/if}

  <ul class="engines">
    {#each rows as r (r.id)}
      <li class="engine {r.level}">
        <span class="dot" aria-hidden="true"></span>
        <span class="who">{r.label}</span>
        <span class="when">
          {#if r.last}{$t('backup_row_last', { when: renderStamp(r.last) })}
          {:else if listKnown}{$t('backup_fact_never')}
          {:else}{$t('backup_row_checking')}{/if}
        </span>
        {#if r.note}<span class="why">{r.note}</span>{/if}
      </li>
    {/each}
  </ul>

  {#if nextRun}
    <p class="next">{$t('backup_row_next', { when: nextRun })}</p>
  {/if}

  <div class="actions">
    <button class="go" onclick={runNow} disabled={busy || runState?.running}>
      {$t('backup_run_now')}
    </button>
  </div>

    <!-- The plan, live: what is being backed up, how far in, and what is still to
         come. It belongs here rather than beside the schedule — it is state, not a
         setting — and it is shown after a run as well, because a finished list is the
         record of what happened. -->
    {#if runState && (runState.running || runState.ran)}
      <RunPanel run={runState} />
      {#if !runState.running && runState.failures > 0 && runState.last_error}
        <p class="err">{runState.last_error}</p>
      {/if}
    {/if}
</section>

<!-- ── The run ────────────────────────────────────────────────────────────────────
     Global, and the comment at the top of this file says why: one Scheduler, which
     serialises because it stops containers. -->
<section class="card">
  <h4>{$t('backup_run')}</h4>
  <p class="hint">{$t('backup_run_hint')}</p>

  {#if conf && status}
    <label class="check">
      <input
        type="checkbox"
        checked={conf.enabled}
        disabled={busy}
        onchange={(e) => commit({ enabled: e.currentTarget.checked })}
      />
      {$t('backup_schedule_enabled')}
    </label>

    <label class="row">
      <span>{$t('backup_schedule')}</span>
      <span class="time">
        <input
          type="number"
          min="0"
          max="23"
          value={conf.hour}
          disabled={busy}
          onchange={(e) => commit({ hour: Number(e.currentTarget.value) })}
        />
        :
        <input
          type="number"
          min="0"
          max="59"
          value={conf.minute}
          disabled={busy}
          onchange={(e) => commit({ minute: Number(e.currentTarget.value) })}
        />
      </span>
    </label>

    <label class="check">
      <input
        type="checkbox"
        checked={conf.user_data}
        disabled={busy}
        onchange={(e) => commit({ user_data: e.currentTarget.checked })}
      />
      {$t('backup_include_user_data')}
    </label>
    <!-- Said at the control rather than in a note further down the page. Ticking a box
         that cannot do anything, and finding out why only after scrolling past three
         cards, is how someone comes to believe their files are backed up. -->
    {#if conf.user_data && writerUserData && !writerUserData.available}
      <p class="note at-control">{writerUserData.reason}</p>
    {/if}

    <!-- "New backups go to" used to live here, and is gone. It was the single-writer
         model showing through: with each engine saying for itself which triggers it
         receives, there is nothing left for one picker to decide — see the Receives
         checkboxes in each engine's tab. -->
    <p class="saved" aria-live="polite">
      {#if error}<span class="err">{error}</span>
      {:else if busy}{$t('saving')}
      {:else if justSaved}{$t('saved')}
      {:else}{$t('backup_autosaved')}{/if}
    </p>
  {:else if error}
    <p class="err">{error}</p>
  {:else}
    <p class="hint">{$t('loading')}</p>
  {/if}
</section>

<!-- ── One tab per engine ─────────────────────────────────────────────────────────
     Each tab is that engine's repository and nothing else: what lands in it, how it is
     protected, how long it keeps things, and what it holds. Engines are independent —
     what is in one has no bearing on what is in another — which is why a merged view
     had to describe a backup as being "in two places at once", a phrase that stopped
     being expressible the moment a second remote engine was possible. -->
<div class="tabs" role="tablist">
  {#each engines as e (e.engine)}
    <button
      role="tab"
      class="tab"
      class:on={active?.engine === e.engine}
      aria-selected={active?.engine === e.engine}
      onclick={() => (tab = e.engine)}
    >
      {label(e.engine, e.name)}
      <!-- The default engine is marked rather than reordered: which engine receives the
           next backup is the one thing the tabs cannot show by themselves, and moving it
           would make the strip reshuffle when the setting changes. -->
      {#if e.engine === status?.active}
        <span class="badge">{$t('backup_engine_active')}</span>
      {/if}
    </button>
  {/each}
</div>

{#if listError}
  <section class="card"><p class="err">{listError}</p></section>
{:else if loading && !engines.length}
  <section class="card"><p class="empty">{$t('loading')}</p></section>
{:else if active}
  <!-- What this engine is, in three facts that differ between engines and that the
       page used to state once for the box — wrongly for at least one of them. -->
  <section class="card">
    <h4>{label(active.engine, active.name)}</h4>

    <dl class="props">
      <div>
        <dt>{$t('backup_prop_survives')}</dt>
        <dd>{active.offsite ? $t('backup_survives_yes') : $t('backup_survives_no')}</dd>
      </div>
      <div>
        <dt>{$t('backup_prop_encryption')}</dt>
        <dd>
          <!-- Three states, not two. "Not encrypted" is true of the local engine and
               false of a repository engine that merely has no repository yet — saying
               it of the second is the kind of wrong that stops someone setting one up. -->
          {#if !activeInfo?.encrypted}{$t('backup_encrypted_no')}
          {:else if activeInfo.has_key}{$t('backup_encrypted_yes')}
          {:else}{$t('backup_encrypted_no_key')}{/if}
        </dd>
      </div>
    </dl>

    <!-- What this engine is for. Checkboxes rather than the single "default engine"
         picker they replaced: a backup can land in several destinations at once, so
         "which one" was never the right question — "does this one receive the nightly
         run" is, and each engine answers it for itself.

         Ticking either of these anywhere switches the box off the setting it was
         provisioned with, so an engine that has never been configured shows the
         fallback it is actually running on rather than an unticked box that would read
         as "nothing goes here". -->
    <h4>{$t('backup_prop_receives')}</h4>
    {#if conf && activeInfo}
      <label class="check">
        <input
          type="checkbox"
          checked={activeInfo.receives_schedule}
          disabled={busy}
          onchange={(e) => commitTrigger(activeInfo.id, { schedule: e.currentTarget.checked })}
        />
        {$t('backup_receives_schedule_label')}
      </label>
      <label class="check">
        <input
          type="checkbox"
          checked={activeInfo.receives_uninstall}
          disabled={busy}
          onchange={(e) => commitTrigger(activeInfo.id, { uninstall: e.currentTarget.checked })}
        />
        {$t('backup_receives_uninstall_label')}
      </label>
      <!-- Not a choice, and shown as a fact so it stops looking like one is missing: a
           rollback has to be a rename, and restoring from a repository is a download the
           app stays broken for. -->
      {#if activeInfo.receives_rollback}
        <p class="hint quiet at-control">{$t('backup_receives_rollback_fact')}</p>
      {/if}
      {#if !activeInfo.receives_schedule && !activeInfo.receives_uninstall}
        <p class="note at-control">{$t('backup_receives_nothing')}</p>
      {/if}
    {/if}

    <!-- Retention, per engine and in one vocabulary.
         There used to be four numbers for the box: three tiers that were handed to a
         repository's own policy engine and did nothing at all on a box without one,
         and a fourth ("copies on this disk") that was the only one doing any work.
         Same control now, resolved for whichever engine's tab you are in — and the
         tier modes are simply absent where the storage keeps full copies. -->
    <h4>{$t('backup_retention')}</h4>
    <p class="hint">
      {activeInfo?.retention?.self_expiring
        ? $t('backup_retention_by_engine')
        : $t('backup_retention_by_maison')}
    </p>

    {#if conf && activeInfo}
      <label class="row">
        <span>{$t('backup_retention_mode')}</span>
        <select
          value={shownMode}
          disabled={busy || !!activeInfo.retention?.locked?.includes('mode')}
          onchange={(e) => setMode(activeInfo.id, e.currentTarget.value)}
        >
          {#each modes as m (m)}
            <option value={m}>{$t(`backup_mode_${m}`)}</option>
          {/each}
        </select>
      </label>

      {#if shownMode === 'custom'}
        <div class="tiers">
          {#each ['daily', 'weekly', 'monthly'] as tier (tier)}
            <label>
              <span>{$t(`backup_keep_${tier}`)}</span>
              <input
                type="number"
                min="0"
                value={keepOf[tier as 'daily' | 'weekly' | 'monthly']}
                disabled={busy}
                onchange={(e) =>
                  commitEngine(activeInfo.id, {
                    keep: { ...keepOf, [tier]: Number(e.currentTarget.value) },
                  })}
              />
            </label>
          {/each}
        </div>
      {:else if shownMode === 'count'}
        <label class="row">
          <span>{$t('backup_keep_count')}</span>
          <input
            type="number"
            min="1"
            value={activeInfo.retention?.count ?? 2}
            disabled={busy}
            onchange={(e) => commitEngine(activeInfo.id, { count: Number(e.currentTarget.value) })}
          />
        </label>
      {:else if shownMode === 'age'}
        <label class="row">
          <span>{$t('backup_keep_age')}</span>
          <input
            type="number"
            min="1"
            value={activeInfo.retention?.max_age_days ?? 30}
            disabled={busy}
            onchange={(e) =>
              commitEngine(activeInfo.id, { max_age_days: Number(e.currentTarget.value) })}
          />
        </label>
      {/if}

      <!-- Whose number this is. A value the deployment chose, presented as though the
           user had typed it, is a value they will not think to question. -->
      {#if inherited}
        <p class="hint quiet">
          {$t(`backup_retention_from_${activeInfo.retention?.source ?? 'default'}`)}
        </p>
      {/if}
    {/if}

    <!-- The encryption key belongs to the engine that has a repository, not to the box:
         it is AppDataShared/backup/<engine>/repository.password, and the local engine
         has none because its archives are not encrypted at all. -->
    {#if activeInfo?.encrypted}
      <h4>{$t('backup_key')}</h4>
      <p class="hint">{$t('backup_key_hint')}</p>
      <div class="actions">
        <!-- Showing it first, mailing it second: reading the key off the screen keeps
             it on the box, where mailing it puts a plaintext secret in an inbox. Both
             are offered because the mail is the copy that survives losing the box. -->
        <button onclick={toggleKey} disabled={keyBusy || !activeInfo.has_key}>
          {key ? $t('backup_key_hide') : $t('backup_key_show')}
        </button>
        <button onclick={sendKey} disabled={keyBusy || !activeInfo.has_key}>
          {$t('backup_key_send')}
        </button>
      </div>
      <!-- Why the buttons are dead, rather than leaving them dead and unexplained. -->
      {#if !activeInfo.has_key}
        <p class="hint quiet">{$t('backup_key_absent')}</p>
      {/if}

      {#if key}
        <div class="key">
          <code>{key}</code>
          <button onclick={copyKey}>{copied ? $t('copied') : $t('copy')}</button>
        </div>
      {/if}

      <!-- Whether a copy has ever left the box is the only fact that matters here, so
           it is stated either way rather than only when reassuring. -->
      <p class="hint key-state">
        {#if keyNote}{keyNote}
        {:else if status?.key_sent}
          {$t('backup_key_sent_on', { when: keySentAt, to: status.key_sent.to ?? '' })}
        {:else if activeInfo.has_key}{$t('backup_key_never_sent')}{/if}
      </p>
    {/if}
  </section>

  <!-- Your files first: the backend's two sets are apps and user data, and the user
       data one is not an app — putting it inside the list below would file it under a
       heading that is a list of apps. -->
  <UserDataCard data={active.user_data} engine={active.engine} onchanged={loadArchives} />

  <!-- Every backup this engine holds, grouped by app. This list is what an app's own
       Backups tab cannot be: it reaches the backups of an app that is gone
       (uninstalling renames the folder, so the tile and its tab disappear with it — and
       "I uninstalled it and regret it" is the most common reason to want a restore),
       and it puts the cost on the same screen as the delete button. -->
  <section class="card">
    <h4>{$t('backups_stored')}</h4>

    <!-- What this engine holds, and — only where it is a different number — what that
         costs on this disk.
         They are two genuinely different questions for a repository, whose snapshots
         cost nothing here. For an engine that keeps its archives on this disk they are
         the same number, and printing it twice beside "free" read as a tautology:
         "0 B in backups · 0 B on this disk". -->
    <p class="totals">
      {$t('backups_total', { used: renderSize(active.total) })}
      {#if active.total !== active.used}
        · {$t('backups_local_used', { used: renderSize(active.used) })}
      {/if}
      {#if free !== null}
        · {$t('backups_free', { free: renderSize(free) })}
      {/if}
    </p>

    {#if !active.apps.length}
      <p class="empty">{$t('backups_empty')}</p>
    {:else}
      {#each active.apps as group (group.app)}
        <div class="group">
          <h5>
            {group.app}
            {#if group.orphan}
              <span class="tag" title={$t('backups_orphan_hint')}>{$t('backups_orphan')}</span>
            {/if}
            <span class="size">{renderSize(group.total)}</span>
          </h5>
          <!-- showEngine is off: this tab is already one engine, so naming it above
               every app would repeat the tab label down the page. -->
          <BackupRows
            backups={group.backups}
            showEngine={false}
            {busy}
            onrestore={(b) => restore(group.app, b)}
            ondelete={(b) => remove(group.app, b)}
          />
        </div>
      {/each}
    {/if}
  </section>
{/if}

<style>
  .head {
    max-width: 46rem;
    margin-bottom: 1rem;
  }
  .card,
  .state {
    max-width: 46rem;
  }
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1rem 1.25rem;
  }
  .card + .card {
    margin-top: 1rem;
  }

  /* The protection band. No card and no border: it is a verdict about the page, not
     another panel competing with the ones below it. */
  .state {
    margin-bottom: 1.5rem;
  }
  .verdict {
    margin: 0 0 0.6rem;
    font-size: 0.95rem;
    font-weight: 600;
    line-height: 1.4;
    color: var(--text);
  }
  .verdict.bad {
    color: var(--red);
  }
  .verdict.warn {
    color: var(--orange);
  }
  .verdict.checking {
    color: var(--text-muted);
    font-weight: 400;
  }

  /* One row per destination. A list rather than a grid of figures, because the unit
     the user reasons about is "where my backups are", and there is one of those per
     engine. */
  .engines {
    list-style: none;
    margin: 0 0 0.75rem;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .engine {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 0.5rem;
    font-size: 0.85rem;
    line-height: 1.4;
  }
  /* The chip. Colour is never the only signal — every row that is not plain "ok"
     carries a sentence saying what is wrong. */
  .dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background: var(--green);
    flex: none;
    align-self: center;
  }
  .engine.warn .dot {
    background: var(--yellow);
  }
  .engine.bad .dot {
    background: var(--red);
  }
  .engine.idle .dot {
    background: var(--border-strong);
  }
  .who {
    font-weight: 600;
    color: var(--text);
  }
  .when {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
  .why {
    color: var(--text-muted);
  }
  .engine.bad .why {
    color: var(--red);
  }
  .next {
    margin: 0 0 0.25rem;
    font-size: 0.8rem;
    color: var(--text-subtle);
    font-variant-numeric: tabular-nums;
  }

  /* Facts, not statistics: four short answers, wrapping rather than scrolling. */
  .props {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 0.5rem 1.25rem;
    margin: 0 0 0.75rem;
  }
  .props dt {
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-subtle);
  }
  .props dd {
    margin: 0.1rem 0 0;
    font-size: 0.85rem;
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }
  .props {
    margin-bottom: 1.25rem;
  }
  .props dd {
    font-variant-numeric: normal;
    line-height: 1.4;
  }

  .tabs {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    margin: 1.5rem 0 0.9rem;
  }
  .tab {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    border: 0;
    background: none;
    padding: 0.45rem 0.7rem;
    margin-bottom: -1px;
    border-bottom: 2px solid transparent;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text-muted);
    cursor: pointer;
  }
  .tab:hover {
    color: var(--text);
  }
  .tab.on {
    color: var(--text);
    border-bottom-color: var(--primary);
  }
  .badge {
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 0.05rem 0.4rem;
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
  /* Every h4 after the first opens a group inside the card, so it needs air above
     it that the card's own padding already provides for the first. */
  h4 ~ h4 {
    margin-top: 1.5rem;
  }
  .hint,
  .empty,
  .note {
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    line-height: 1.5;
  }
  .quiet {
    font-size: 0.8rem;
    color: var(--text-subtle);
  }
  /* A note that belongs to the control directly above it, indented to say so. */
  .note.at-control {
    margin: -0.1rem 0 0.6rem 1.6rem;
    font-size: 0.8rem;
    color: var(--orange);
  }
  .totals {
    margin: 0 0 1.2rem;
    font-size: 0.85rem;
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin: 0.5rem 0;
    /* Wrapping is what keeps this on a phone: the label is 9rem and a <select>'s
       min-content width is its widest option string, and neither shrinks. */
    flex-wrap: wrap;
  }
  .row > span:first-child {
    min-width: 9rem;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.4rem 0;
    font-size: 0.9rem;
  }
  .time {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
  }
  .time input {
    width: 4rem;
  }
  .tiers {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
    gap: 0.5rem;
  }
  .tiers label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.85rem;
  }
  input,
  select {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.35rem 0.5rem;
    /* Both are flex items in .row and default to min-width:auto, which for a select
       is its widest option — wide enough to push the card past a phone viewport. */
    min-width: 0;
    max-width: 100%;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.75rem;
    flex-wrap: wrap;
  }
  button {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.4rem 0.8rem;
    cursor: pointer;
  }
  button.go {
    background: var(--primary);
    border-color: var(--primary);
    color: var(--text-on-accent);
    font-weight: 600;
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }

  /* The receipt that replaces the Save button. It says what the page does even when
     nothing has just happened, because "there is no Save button" is itself something
     the user has to be told once. */
  .saved {
    margin: 0.9rem 0 0;
    min-height: 1.1em;
    font-size: 0.8rem;
    color: var(--text-subtle);
  }

  .group {
    margin-bottom: 1.4rem;
  }
  h5 {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0 0 0.45rem;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text);
  }
  .size {
    margin-left: auto;
    font-size: 0.8rem;
    font-weight: 400;
    font-variant-numeric: tabular-nums;
    color: var(--text-muted);
  }
  .tag {
    padding: 0.1rem 0.4rem;
    border-radius: 4px;
    background: hsla(38, 92%, 90%, 1);
    color: hsl(30, 80%, 32%);
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  /* The key itself: monospaced and selectable in one gesture, because it is
     transcribed by hand often enough that a proportional font is a real hazard —
     l/1 and O/0 decide whether a restore works. */
  .key {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin: 0.75rem 0 0;
    padding: 0.5rem 0.6rem;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: 6px;
  }
  .key code {
    flex: 1;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.85rem;
    word-break: break-all;
    user-select: all;
    color: var(--text);
  }
  .key-state {
    margin-top: 0.75rem;
    margin-bottom: 0;
  }
  .err {
    margin: 0 0 0.9rem;
    font-size: 0.85rem;
    color: var(--red);
  }
  .saved .err {
    margin: 0;
  }
  .warn {
    color: var(--orange);
    font-size: 0.85rem;
    margin: 0 0 0.6rem;
  }

  /* Phone. 560px is this app's established narrow breakpoint (AppUsageRows,
     StorePanel). The label above the control rather than beside it: at 390px the
     panel's inner width is about 318px, which a 9rem label plus a select does not
     fit however hard it wraps. */
  @media (max-width: 560px) {
    .row {
      flex-direction: column;
      align-items: stretch;
      gap: 0.25rem;
    }
    .row > span:first-child {
      min-width: 0;
    }
    .time {
      align-self: flex-start;
    }
  }
</style>
