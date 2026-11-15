# Approval posture

What the lambe supervisor may do unattended, and what it must never do
without a person. This is the contract the daemon, its gates, and its
formulas are held to; if a proposed change widens a "never" column, that
change needs a human decision, not just a green test suite.

## The daemon may, unattended

- **Claim and dispatch** tracker issues labeled for this machine
  (`lambe/machine:<name>` matches, unlabeled runs anywhere).
- **Run agents** in per-issue workspaces under `WORKFLOW.md` semantics:
  bounded turns, bounded attempts, silence and deadline watchdogs.
- **Defer and retry**: park a run until a wake time (limit lockout,
  gate-retry backoff) and resume it later.
- **Kill its own children**: a watchdog escalation or a cancel runs the
  SIGTERM → grace → SIGKILL ladder against the process group it spawned.
- **Heartbeat, sync, recover**: extend its own claims, pull/push the hub
  (never force), adopt or stand down its own orphans at startup.
- **Read and write tracker records**: attempts, status transitions,
  gate outcomes, defers — all through bd, all journaled.

## The daemon must not, without a human

- **Merge, push, or publish** anything the verified-completion gate has not
  passed. A red or erroring gate never completes work; it retries with
  guidance, then escalates to needs-human.
- **Force a sync**: a non-fast-forward push is resolved by pull-then-push,
  exactly once, never `--force`.
- **Kill what it does not own**: unverified orphans, other assignees'
  claims, and any process group it cannot tie to one of its own recorded
  attempts are left alone.
- **Dispatch foreign work**: issues pinned to another machine are skipped,
  always, even when this machine is idle.
- **Exceed its supervision budget**: attempt caps, turn caps, and watchdog
  bounds are hard stops, not suggestions; hitting them defers or escalates,
  never loops.

## Escalation surface

Everything the daemon parked for a person lands in one place: the
`lambe status` morning report (needs-human items with the last gate output,
overnight gate rejections, deferred runs with wake times) and, read-only,
the `/api/v1/state` HTTP surface. A human decision is consumed by acting on
the tracker (relabel, reassign, close, or clear the block); the daemon needs
no restart to notice.
