# Resources

**Settings → Resources.** What the box is doing now, and what it has been doing for the
last thirty days.

The page has four tabs — CPU, Network, Disk, Apps — and a range picker: **Live**, or a
recorded window of 1 h / 24 h / 7 d / 30 d. Those two are different data sources, and the
difference explains most of the design below.

| | Live | Recorded |
|---|---|---|
| Source | `resources` WebSocket channel | ring file on disk |
| Cadence | 2 s | 1 min |
| Sampled when | only while the page is open | always, unless switched off |
| Detail | per interface, per device, per filesystem, per process | summed rates only |
| Retention | ~20 min, in the browser | 30 days |

---

## The idle budget

Everything else in Maison is gated on a live subscription: the dashboard gauges, the
per-app monitor, this page's live tab. Nothing is sampled while nobody is looking.

History is the one exception, and it cannot not be — a graph of the last thirty days has
to have been recorded during those thirty days. So the recorder is the only thing in
Maison that measures the host with nobody watching, which is why it is the only thing with
an off switch (the **Recording** card at the bottom of the page).

It is kept to one cheap reading per minute — `cpu.Times`, `mem`, `net`, `diskstats`, one
`statfs` — writing 32 bytes. Measured on a live PCS with the dashboard closed: RSS did not
grow over seven minutes of recording (7.96 MiB → 7.19 MiB), CPU 0.00–0.08 %.

**Three things are deliberately NOT on that path**, and must not be added to it:

- `sensors.SensorsTemperatures()` — walks `/sys/class/hwmon`.
- The process table — a `/proc/<pid>` read per process on the box.
- `appstats.Sample()` — a Docker stats round-trip per running container.

All three are page-open only. The first two additionally run on a 6 s sub-cadence rather
than the page's 2 s (`slowEvery` in `internal/system/detailed.go`).

---

## The ring file

`${STATE_DIR}/metrics.ring` — a fixed-size binary ring, not a database and not an
in-memory buffer.

```
64-byte header   magic MSNRING1, format version, record size, step, slot count
43,200 records   32 bytes each: timestamp, cpu, mem, load1, swap, disk%,
                 net rx/tx, disk read/write
```

30 days at one-minute resolution = **1.32 MiB**, so no multi-tier downsampling is needed;
one tier, one file. The file is **sparse** — it reaches its full size only once the slots
have been written (12 KB after ten records on a real box).

Three properties are load-bearing:

- **`WriteAt`, not `mmap`.** An mmap'd file counts touched pages against RSS, which is
  exactly what this feature exists to avoid. A pwrite leaves the data in the page cache,
  where the kernel owns it.
- **Slot derived from the timestamp** (`slot = bucket % slots`), and every record repeats
  its own timestamp. So a write never reads first, a lap overwrites the oldest sample with
  no bookkeeping, and — the point — **nothing is loaded at startup and nothing is flushed
  at shutdown**. A slot left over from a previous lap carries a timestamp outside the
  requested window and is skipped, so downtime renders as a gap in the graph rather than
  as stale data presented as current.
- **Staged for five minutes** (5 × 32 B) and flushed as one contiguous write. An ungraceful
  kill loses at most that, and loses it as a gap. Verified: 10 records before a
  `docker rm -f`, 10 records after.

A header that does not match the requested geometry resets the file. The contents are
samples; re-deriving them costs history, not data, and that is much safer than
reinterpreting bytes written under a different layout.

Reads are downsampled **server-side** to ~500 buckets (`GET /api/system/history`). Empty
buckets are omitted rather than sent as zeroes — the response carries `step_ms` so the
client can break the line wherever two points are further apart than one bucket.

### Why history has no per-interface columns

Fixed-width records are what make the ring addressable by arithmetic, and what let thirty
days fit in 1.32 MiB with no database. The cost is that a variable number of interfaces or
devices cannot live in one. History therefore stores **summed** net and disk rates; the
per-interface and per-device breakdown exists in the live view only, and the page says so
rather than showing one interface's line labelled as the box's.

---

## HOST_PROC

The stack mounts the host's `/proc` read-only at `/host/proc` and sets `HOST_PROC`.

Most of `/proc` is **not** namespaced — `/proc/stat`, `/proc/meminfo` and
`/proc/diskstats` already report the host from inside a container — so this changes nothing
for CPU, memory and disk IO, which were always correct. It buys the three readings that
*are* per-namespace or per-process:

1. **Network counters.** `/proc/net` is a symlink to `self/net`, so it resolves in the
   *reader's* network namespace however it is reached — through a bind mount of the host's
   `/proc` included. From `maison-app` on the `pcs` bridge that means a veth, not the
   uplink. Maison reads **`/host/proc/1/net/dev`** explicitly: host PID 1's namespace.
   `HOST_PROC` alone does not fix this.
2. **The process table**, for the top-processes list.
3. **The host mount table** — see below.

Everything feature-detects (`system.HasHostProc`). Without the mount the page hides the
network and process tables and says why, rather than presenting the container's own figures
as the box's.

Process owners fall back to `uid N` when the name will not resolve: `Username()` looks the
uid up in whichever `/etc/passwd` this process can see, which in a container is the
*image's*. Root resolves everywhere; every real account on the box does not.

---

## The filesystem table

The obvious implementation — list the host's mounts and `statfs` each one — does not work
from inside a container, and fails in the worst possible way.

On a Yundera PCS, `/DATA` is **not a mountpoint**: it is a plain directory on `/dev/sda1`,
which is mounted at `/`. (Verified on holyhorse and wisera; `findmnt /DATA` returns
nothing.) So the host's mount table says the data lives on `/` — and `/` exists inside the
container too, as the overlay. The call **succeeds** and returns the wrong filesystem. It
is worse than it sounds: the overlay's upper directory is on the same disk, so the numbers
even look right, and a naive implementation passes testing on a real PCS by luck.

So the direction is inverted (`internal/system/filesystems.go`):

1. Read `/proc/self/mountinfo`; take the data root plus anything mounted under it — what
   this process can actually measure.
2. `statfs` each for the numbers, `stat` each for its **device id**.
3. Read `/host/proc/1/mountinfo`, indexed by device id.
4. Join on device id, dedupe by it, drop pseudo filesystems by type.
5. Report the **host's** device and mountpoint with the **statfs** numbers.

The device id is exact where a path comparison is not: the overlay has its own anonymous
device (`0:53`), matches no host entry, and drops out on its own. On a real PCS this yields
exactly one row — `/dev/sda1 at / (ext4)`, matching the host's own `df` — measured through
`/DATA`.

Host filesystems that cannot be measured from in here (`/boot`, `/boot/efi`) are **counted
and shown** as "not measurable", rather than silently missing. `pseudoFstypes` is a deny
list: anything unlisted is shown, so a new storage filesystem appears on its own and a new
virtual one appears once as an over-count in that number — a far better failure than hiding
a disk that is filling up.

Without the `/proc` mount the table still works; it just names rows by the container's own
paths and knows of nothing missing.

---

## A gopsutil trap

`cpu.Percent(0, false)` keeps **one package-level baseline** (`lastCPUPercent`), which every
call overwrites. `system.Collector.Sample` already calls it every 2 s while the dashboard is
open.

So neither the history sampler nor the Resources page may call it: two callers on different
cadences silently corrupt each other's deltas, and only while somebody has the dashboard
open — which is exactly when nobody would suspect the measurement rather than the machine.

Both therefore track their own `cpu.Times` baseline and differentiate it themselves. See
the note on `system.Counters.CPUBusy`. Net and disk counters are monotonic totals, so their
deltas are local anyway.

---

## Benchmarks

Both are user-initiated only, never on a timer.

- **Disk** — 256 MiB written and read back under `O_DIRECT` in the state directory. The
  aligned buffer is an anonymous `mmap` (page-aligned by definition) rather than the usual
  trick of over-allocating a Go slice and indexing to an aligned offset, which depends on
  the GC never relocating the backing array. If the filesystem refuses `O_DIRECT` (tmpfs,
  some network mounts) it falls back and the result is **labelled** as a cached read rather
  than presented as a disk figure. The scratch file is removed at startup as well as after
  a run, so a crash mid-benchmark cannot leave 256 MiB behind.
- **Network** — 25 MiB each way against `speed.cloudflare.com`, sequential (in parallel the
  two would compete for the link and each report about half the truth). This is outbound
  egress to a third party; the UI says so.

Both are implemented in Go rather than by shelling out: the runtime image is alpine with
busybox, which has **no curl at all**, and whose `dd` supports `iflag=direct` only depending
on how it was built. A benchmark that silently measures the page cache because a flag was
ignored is worse than no benchmark.

Runs are **asynchronous** — the POST only starts one and answers immediately; the client
polls `GET /api/system/bench`. A disk run takes tens of seconds and a network run can take
over a minute, and holding an HTTP request open that long would be at the mercy of every
proxy between the browser and here.

Validated against the host's own `dd` at matched block size: 203 MB/s write / 336 MB/s read
vs dd's 208 / 306 in the same minute.

---

## API

```
GET    /api/system/resources                        one-shot host breakdown
GET    /api/system/history?from=&to=&points=500     downsampled recorded series
DELETE /api/system/history                          discard the recording
GET    /api/system/bench                            both benchmark slots
POST   /api/system/bench/disk
POST   /api/system/bench/network
```

Plus the `resources` WebSocket channel, subscribe-gated like `apps` and `appstats`.

There is deliberately **no unauthenticated variant**. Maison has no auth of its own and sits
entirely behind the AppShield gate; the process list carries command names and uids, which
have no business on an open endpoint. (The orchestrator's `pcs perf` reads
`settings-center-app`'s `/api/perf`, which is a separate thing.)

## Settings

`metrics_history` in `settings.json`, default on.

It is a **`*bool`**, not a `bool`, and that is load-bearing: `usersettings.merge` treats a
zero value as "not supplied", so a plain bool could be switched on and then never off again
— `false` would be indistinguishable from an absent field. Absent means on, so a settings
file written before the field existed keeps history rather than silently losing it on
upgrade.
