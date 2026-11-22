---
title: Orphaned claims starve the hub with heartbeats
date: 2026-11-22
category: runtime-errors
module: internal/symphony (recovery), internal/daemon (heartbeat, recover), cmd/lambe (claims)
problem_type: runtime_error
severity: high
tags: [heartbeat, recovery, orphan, claims, dolt, single-writer]
---

# Orphaned claims starve the hub with heartbeats

## Observed

On v0.9.2 and v0.10.1, 47 stale claims (held by this daemon's actor in earlier
daemon lives, no workspace on disk, 5+ days old) were each logged `session
recovery blocked; preserving claim and workspace` ("no durable continuation
state; inspect any old agent before releasing its stale claim") and then
heartbeated forever. In 14 hours that was about 11,000 failed `bd heartbeat`
calls (context deadline exceeded) on the embedded-Dolt single-writer hub. A
fresh issue's attempt-record writes (`bd create ac-att-...`, `persist: record
pgid`) timed out for 15+ minutes, so its agent was never prompted. Startup
recovery took about 6 minutes walking the claims serially (~4s each), and a
restarted daemon re-read closed issues from local session checkpoints.

## Current behavior (before the fix)

- `symphony.Loop.recoverPending` ran at the start of every `Tick`, serially,
  and re-read every pending claim from the tracker (one or two `bd list`
  calls each) on every tick, whether or not anything had changed.
  `Restore` ran a full pass synchronously before the loop started.
- A claim from an older installation becomes a `legacy-blocked` checkpoint;
  `resumeCheckpoint` returns "no durable continuation state" for it. It stays
  in `Loop.recovery` (so dispatch skips it) and nothing ever clears it.
- `daemon.HeartbeatLoop` is independent of recovery: it heartbeats every
  in-progress issue assigned to the actor, so blocked claims were beaten
  forever. Concurrency was already capped at 4 per round, but a round never
  yielded to a slow hub: each tick launched another full round of calls
  against a database that was already timing out, and live work waited behind
  dead claims.
- A closed issue with no assignee left its checkpoint file on disk.

## Why the conservative stance exists

A claim with no continuation state cannot be resumed, and the daemon cannot
prove its old agent is dead. Releasing the claim would let another dispatch
(this daemon or another host) start a second agent on the same issue. So
recovery preserves the claim and the heartbeat keeps its lease alive. That
safety property stays: lambe never silently releases a claim whose agent might
be alive, and never double-runs.

## Options considered

1. Release stale claims automatically after a timeout. Rejected: it breaks the
   safety property above, and `supervisor.auto_reclaim` (default off) already
   owns that decision.
2. Only bound heartbeat fan-out. Necessary but not sufficient: the dead claims
   still cost calls forever.
3. Stop heartbeating after a TTL, let the lease expire, surface the claim, and
   release only on operator command. Chosen.

## The fix

- Heartbeats: live work first, orphans second, lapsed orphans never; at most 4
  in flight; a round that spends its 30s budget makes the next rounds skip with
  exponential backoff (1, 3, 7, capped at 7 skipped); random start delay up to a
  tenth of the interval.
- `supervisor.orphan_claim_ttl` (default 1h, 0 disables). The clock
  (`blocked_since`) is stored in the checkpoint, so daemon restarts do not
  reset it; legacy checkpoints without it fall back to the file's mtime in
  status. Past the TTL the daemon checks the newest attempt's recorded process
  group without signalling it; only a provably dead (or recycled-pid) agent
  lets the claim lapse. An alive or unverifiable agent keeps heartbeating.
  Lapsing only stops heartbeats: the claim stays assigned and stays in
  `Loop.recovery`, so this daemon cannot dispatch it. The lease expires on its
  own, after which `auto_reclaim` or another host may take it.
- Surfacing: `lambe status` section, `lambe claims list`, `lambe doctor`
  warning. All read checkpoint files, so they work without a daemon.
- `lambe claims release ISSUE [--force]` is the only release path. Without
  `--force` it requires a recorded orphan, no workspace on disk, and a provably
  dead agent; the tracker release is guarded by the actor's identity.
- Recovery: lookups run 4 at a time, each time-boxed to 10s, and a pass is
  capped at 15s per `Restore`/`Tick`, so dispatch proceeds while old claims are
  still being checked (dispatch already skips ids in `Loop.recovery`). Only a
  claim blocked for lack of continuation state is parked for 5 minutes between
  re-checks; transient failures still retry next tick. Startup `Recover`
  (process-group adoption) reads ownership with the same bounded parallelism.
- Checkpoints of closed issues are removed when the issue is not ours.

## Not changed

`WORKFLOW.md` semantics, the supervisor loop's dispatch contract, and
`supervisor.auto_reclaim` are untouched. The loop change is confined to how
and when recovery reads the tracker, not to what recovery decides.
