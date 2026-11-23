---
title: Slow embedded Dolt starves the daemon's bd calls
date: 2026-11-22
category: runtime-errors
module: internal/tracker (shell transport, attempt records), internal/symphony (recovery), cmd/lambe (doctor)
problem_type: performance_issue
severity: high
tags: [bd, dolt, embedded, timeout, concurrency, attempts, doctor]
---

# Slow embedded Dolt starves the daemon's bd calls

## Observed

On a 762 MB embedded-Dolt hub with about 400 attempt issues and 80 in-progress
claims, on a machine at load 10, a freshly claimed issue never reached its
agent. Lambe refuses to prompt an agent until its attempt record is written, and
that write (`bd create --id <prefix>-att-<issue>-1`, the `bd show` that
followed, `bd heartbeat`, and the `bd list --metadata-field ...` scans around
them) hit the fixed 60s per-call limit with `context deadline exceeded`, over
and over, for an hour.

The cause is how embedded Dolt works. Every `bd` invocation opens the database
in-process (about 1 to 5 seconds of CPU) and writers serialize behind
`.beads/*.gate.lock`. The daemon fans several `bd` processes into one tick, so
N concurrent calls take about N times one call: queueing inside `bd`, where
Lambe cannot see it, prioritize it, or exclude it from the timeout.

Measured on an APFS clone of that hub (load 7-9, one `bd` call alone is 0.8-1.7s):

| N concurrent `bd list --limit 1` | wall time | median call |
| --- | --- | --- |
| 1 | 1.2s | 1.2s |
| 4 | 5.2s | 3.3s |
| 8 | 10.2s | 5.8s |

`show`, `list --id`, metadata `list`, `create`, and `heartbeat` all scale the
same way (8 concurrent `show`: 18s wall). Wall time is linear in N: the calls
are serialized, not parallel.

## What changed

- **Bounded, prioritized subprocesses.** The shell transport runs at most
  `supervisor.bd_max_concurrency` (default 3) `bd` processes at once. Waiters
  are served by lane: writes (claim, create, update, close, defer, reclaim,
  batch), then reads and polls (list, show, events), then heartbeats. Within a
  lane it is first come, first served. A backlog of 50 heartbeats no longer
  delays a claim's attempt record.
- **Timeout counts the run, not the wait.** `supervisor.bd_timeout` (reads and
  heartbeats, default 60s) and `supervisor.bd_write_timeout` (writes, default:
  same as `bd_timeout`) start when the process starts. A separate
  `supervisor.bd_queue_wait` (default 10m) bounds the wait for a slot; a call
  that gives up fails with an error that wraps `context.DeadlineExceeded`, like
  a timed-out run.
- **Fewer calls per attempt.** A new attempt record is one `bd create` that
  already carries `--status blocked` and the metadata (was create, then show on
  an existing record, then update). A re-record (the daemon's pgid persist) is
  one `bd update`; `already exists` on create and `no issue found` on update are
  both handled, so the write is idempotent whichever side has the record.
  `Attempts` reads one `bd list --id` over the deterministic ids
  (`<prefix>-att-<session>-<n>`, n up to 100) instead of a metadata-field scan.
  Recovery looks claims up 25 at a time with one read that includes closed
  issues, instead of one or two calls per claim.
- **Time-boxes follow the knob.** The attempt persist round trip and the
  recovery inspection time-box stretch to a raised `bd_timeout` instead of
  cutting a slow but healthy call short.
- **`lambe doctor`** times one `bd list --limit 1` and one metadata query on an
  embedded tracker and warns when either takes more than 5 seconds. A hub already in server mode (`dolt_mode` other than `embedded`) is not probed and is never told to switch.

Claim to attempt-recorded to agent-promptable, measured on that clone with
`TestSlowHubClaimToAttempt` (`internal/tracker/slowhub_bench_test.go`, gated by
`LAMBE_BENCH_HUB`), v0.10.1 against this change:

| | v0.10.1 | now |
| --- | --- | --- |
| `bd` calls per sequence (claim, stop-check read, attempt record, persist read, persist pgid) | 8 | 5 |
| solo, load 7-9 | 13.2s | 8.3s |
| 4 busy CPU loops plus 6 workers heartbeating and polling | first run failed after the 60s limit (`bd update ... context deadline exceeded`, the production symptom) | 6 of 6 runs succeeded, median 17s, max 41s |

## Embedded versus server mode

| | Embedded (default) | Dolt server mode |
| --- | --- | --- |
| Setup | none | a `dolt sql-server` per hub (`bd init --server`, or `--external` for a shared one; see `docs/MULTI-MACHINE.md` and [dolt-server-mode.md](dolt-server-mode.md)) |
| Cost per `bd` call | opens the database each time: seconds on a large hub | a connection to a warm server: milliseconds |
| Concurrency | writers serialize behind a file lock; callers queue invisibly | server handles concurrent clients |
| Multiple hosts | one host per database (sync through Dolt remotes) | one server shared by every host, atomic claims server-side |
| Failure modes | slow under load or at size, never "down" | the server can be down or wedged; needs supervision |

Embedded mode is the right default for a small hub on one machine. Once `bd
list --limit 1` takes seconds, the fix is server mode, not bigger timeouts:
the timeout and concurrency knobs below keep Lambe correct on a slow hub, but
they cannot make each call cheaper.

## Tuning an embedded hub that is slow today

```yaml
supervisor:
  bd_timeout: 180s          # reads and heartbeats
  bd_write_timeout: 300s    # claim / attempt record / close
  bd_max_concurrency: 2     # fewer parallel opens of the same database
  bd_queue_wait: 15m
```

Lower concurrency usually helps more than a longer timeout on a CPU-bound
machine, because every extra process slows all the others. Raise it only on a
hub that stays fast with concurrent callers. `lambe doctor` prints the measured
latencies and the fix.

## Limits

- Strict lane priority: a sustained flood of writes could delay heartbeats; the
  lease TTL (5 minutes) is much longer than a write burst in practice.
- The cap is per process. A second `lambe` command (`status`, `claims`) or an
  agent running `bd` itself adds its own processes outside the cap.
- `Attempts` probes attempt numbers 1 to 100 (the `attempt_cap` ceiling) under
  the hub's current issue prefix; records created under another prefix are not
  found by id.
