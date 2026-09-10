# Incidents and notifications

> **Status: implemented.** The register (`internal/incident`), the mail sink, the
> coalescing outbox, the live channel, the settings page, the inbound API and both
> families of detector are in the tree and tested. The backup scheduler's own
> notification logic has been ported onto it and no longer exists separately.
>
> **Not yet built:** sinks other than mail (webhook / ntfy / Telegram), predictive
> disk ("full in about five days"), and an alert for backups whose encryption key the
> owner has never saved. See [Deliberately not done](#deliberately-not-done).

Its companions:

- [`backup.md`](./backup.md) — the subsystem this grew out of. Its
  §Scheduling describes the run whose outcome is now asserted here.
- [`app-model.md`](./app-model.md) / [`lifecycle.md`](./lifecycle.md) — where the
  pushed app-level reports come from.

---

## The problem

Maison could mail exactly one thing: that backups had started or stopped failing.
That logic was good — edge-triggered, restart-safe, and careful never to let a broken
relay fail a backup — but it was welded to the scheduler.

Meanwhile a dozen other real failures were `log.Printf`-and-swallowed, in a container
log nobody reads:

- an update that could not take a rollback point, so the app can no longer be undone
- an update that failed **and** whose rollback failed, leaving the app in neither state
- a `post_install` or `post_up` hook that failed, leaving an app half-configured
- a nightly store refresh that has been 404ing for a month
- a disk at 98%

Each of those could have grown its own mailer, its own idea of how often to repeat
itself, and its own relay configuration to get wrong. This is the one place instead.

## An incident is a state, not an event

The single design decision everything else follows from. A reporter does not send a
notification; it **asserts a condition** and later clears it:

```go
inc.Report(incident.Report{ID: "disk.full:/dev/sda1", Kind: incident.KindDiskFull, …})
inc.Resolve("disk.full:/dev/sda1")
```

- `ID` is the dedup key. Two calls with the same ID are the same incident, however far
  apart. Shape it as `kind:subject`.
- Re-asserting an open incident bumps its counters and **tells nobody**. That is what
  lets a detector run every five minutes and re-state the same truth all week without
  anybody's inbox noticing.
- `Resolve` on an ID that is not open is a **no-op**. That is the whole of the "a box
  whose first ever backup succeeds must not announce a recovery" rule, which the
  scheduler used to spell out by hand.
- The state is on disk (`${STATE_DIR}/incidents.json`, 0600), so a restart does not
  re-announce a failure the owner has already been told about, nor stay silent about a
  recovery it never saw the failure for.

`Report` **returns nothing**, deliberately. The old rule — "a broken SMTP
configuration must never turn a successful backup into a failed one" — used to depend
on one caller remembering to discard an error. Now no caller can propagate a delivery
problem even by accident.

The litmus test for what belongs here: *if it can fire more than a handful of times a
week, it is a metric, not an incident.*

## Delivery

`Report` and `Resolve` queue a transition; they never send. `Store.Deliver`, on a
two-minute ticker in `server.deliverIncidents`, drains the queue into **one** mail.

That window is the point. One root cause fans out — a disk fills, the next check finds
four apps unhealthy — and without coalescing that is five emails about one problem.

Also in the outbox:

- **Anything that opens and closes inside the same window is dropped.** A condition
  that healed before anyone could have read about it is not news, and "X broke" /
  "X is fine" in one message trains the reader to skip the next one.
- **A failed send is retried, up to three passes, then dropped.** A relay that is
  restarting should not cost the owner an alert; one that has been misconfigured for a
  week should not build a backlog. The queue is persisted, so a restart mid-window
  loses nothing.
- **Muted kinds are still recorded and still listed**, just not mailed. A mute that
  erased the evidence would be indistinguishable from the detector being broken, which
  is the one thing an owner most needs to rule out.

Retention: resolved incidents are kept 30 days, the whole register is capped at 200
records, and the cap trims resolved ones first — it can never drop an open incident,
because that is the current truth about the box.

## Mail is a sink, not the feature

The register works fully on a box with no relay, and the bell in the top bar is its
primary surface. This is not neutrality: a PCS relays through whatever the deployment
gave it into a consumer mailbox that may file it as spam, and **none of that is
visible from the box**. A design that treated "we sent it" as "they were told" would
be wrong in exactly the cases that matter.

`Store.Notify` is the seam a webhook, ntfy or Telegram sink slots into later.

## What raises incidents

Two families. Push wherever the failure is already in hand at a call site; poll only
what nobody is present for.

### Pushed

| ID | Kind | Where |
|---|---|---|
| `backup.run` | `backup.failed` | `backup.Scheduler.reportOutcome`, every run |
| `app.install:<app>` | `app.install` | `installer.StartInstall`, cleared by a later success |
| `app.hook:<app>` | `app.stackup` | `stackup.Up` (`.env` sync, `RunInit`, `post_up`) and `installer` (`post_install`) |
| `app.update:<app>` | `app.update` | `installer.ApplyUpdate` — warning for "no rollback point" and for "failed and rolled back" (the old version is running); **critical** for "the rollback failed too", "rolled back but not running" (`installer.Steady`) and "failed with no rollback point to undo it". Cleared by a later successful update |
| `store.source` | `store.source` | `appstore.StartDailyRefresh`, after two consecutive failures |

`internal/stackup` is package-level functions with no receiver to hang a hook on, so
its reporter arrives as a `Report` closure on `config.Config` — the same shape as the
`Domains` and `AppEnv` live accessors already there, and every stackup function
already takes a `Config`.

The store refresh needs **two** consecutive failures because the first refresh happens
at boot, where the commonest cause of failure is the network not being up yet. At one
attempt a day, two in a row means the catalog has genuinely stopped updating.

### Polled

One five-minute ticker, `server.runDetectors` in `detect.go`. It waits a full interval
before its first pass: at boot the apps are still coming up, and a check that ran
immediately would find half of them unhealthy every time Maison restarted.

| ID | Kind | Rule |
|---|---|---|
| `backup.stale` | `backup.stale` | Backups on, and the last completed run was over 48h ago |
| `backup.engine` | `backup.engine` | The chosen engine is kopia and its repository is not connected |
| `disk.full:<device>` | `disk.full` | Opens at 90% (97% is critical), clears below 85% |
| `app.unhealthy:<app>` | `app.unhealthy` | Docker's health check failing, two passes running |
| `app.partial:<app>` | `app.partial` | Some containers up, some down, two passes running |
| `app.crashloop:<app>` | `app.crashloop` | Docker's restart counter moved between two passes |

Four rules in there are load-bearing:

**Disk incidents are keyed by device, not mountpoint.** On a PCS `/DATA` is a
directory on the root filesystem rather than a mount of its own, so one full disk
appears as several rows of the filesystem table. Keying by mountpoint would announce
the same problem three times.

**The 90/85 gap is hysteresis, not a third threshold.** A reading between them
produces neither a report nor a resolve, so a disk sitting on the line does not open
and close an incident all day — and each edge would be an email.

**App conditions must hold for two consecutive passes.** An app is briefly unhealthy
every time it restarts.

**Polled app conditions are gated on `Registry.HasReached`** — the persisted marker
that the app has been observed reachable at least once. Without it a half-configured
app that never came up would alert forever, and the owner already knows about that
one: they were watching when it failed to install.

### The gap: a stopped app raises nothing

Maison persists **no desired state**. `Registry.Stop` calls `StopProject` and that is
all, so `status: "stopped"` is byte-identical whether the owner switched the app off
on purpose or every container in it crashed. Alerting on it would mail people about
apps they turned off themselves, which is the fastest way to make every later alert
ignorable.

`unhealthy`, `partial` and `crashloop` are the unambiguous subset. Crash-loop escapes
the rule precisely because it is not a state: it is Docker's restart counter *moving*
between two passes, and an app the owner switched off has a counter that stands still.

Closing the gap properly means persisting desired state on `Start`/`Stop`. Worth
doing; a separate change.

## The inbound API

```
GET    /api/incidents                 the whole register
POST   /api/incidents                 assert one   {id, kind, severity, title, detail, args}
DELETE /api/incidents/{id}            clear one
POST   /api/incidents/{id}/ack        hide the badge without resolving
PUT    /api/incidents/mute            {kind, muted}
POST   /api/notifications/test        send a real alert, synchronously
```

`POST` exists for everything on the box that is not Maison — the nightly self-check in
`template-root`, certificate renewal in `mesh-router-caddy`, the auth stack. They
assert a condition and inherit the dedup, the coalescing and the mute switch already
tested here, instead of each growing a mailer.

**The reporter owns the ID.** The same problem must always carry the same ID or it
will be announced twice; re-posting an open incident is the expected steady state for
a caller on a cron, and costs nothing.

Like the rest of this API these routes are unauthenticated, and deliberately: the gate
in front of Maison is the security boundary (see `onboarding.go`), and `rootHandler`'s
host dispatch already means only the dashboard host reaches `/api` at all.

`/api/notifications/test` composes a **real** alert through the real digest writer and
waits for the result, returning the SMTP error verbatim. A button that dialled the
server and hung up would test the socket and nothing else; one that only queued
something would report "sent" when the relay had refused.

## The UI

Settings → Notifications, three cards: what is wrong now, where to send it, what not
to send. Plus a bell in the top bar, rendered **only when something is open** — an
always-present bell that is usually empty trains people not to look at it.

The section is also the first UI the SMTP configuration has ever had. Before it, mail
was `SMTP_HOST` in the environment or a hand-edited `settings.json`.

Two notes on the wiring:

- The badge seeds from `GET /api/incidents` and then follows the `incidents` live
  channel. That channel is the only event-driven one on the hub — nothing pushes on a
  tick — which is what makes it affordable for the top bar to hold a subscription for
  the whole session.
- An incident's dashboard label is `incident_<kind>`, interpolated with its `Args`,
  falling back to the server's English `Title`. The fallback is the **normal** path
  for a kind reported by another PCS component: it will never have a translation here.
  `Detail` is server-generated text and is never translated.

## Deliberately not done

- **No rules engine and no thresholds UI.** Thresholds live in code; there is one mute
  toggle per kind.
- **No alert for filesystems the container cannot measure** (`Filesystems.Unmeasured`).
  On many deployments that is a permanent, unfixable condition, so it would be a
  warning nobody can ever clear — the exact thing that turns into a filter rule.
- **No delivery confirmation.** It is not obtainable from here; see
  [Mail is a sink](#mail-is-a-sink-not-the-feature).
- **Predictive disk.** `internal/metrics` already ring-buffers the history, so "full in
  about five days" is cheap and much more actionable than a threshold. A follow-up.
- **`backup.key` never revealed.** Backups nobody can decrypt are worse than none, and
  Maison knows whether the key has ever been shown. Worth adding.
