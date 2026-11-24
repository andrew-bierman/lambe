# Glossary

**Attempt.** One run of an agent against an issue, from claim to a settled
outcome. Attempts are recorded in the tracker. After `supervisor.attempt_cap`
(default 8) failed attempts the issue becomes *needs-human*.

**Beads.** The git-backed issue tracker Lambe uses as its entire state
([github.com/gastownhall/beads](https://github.com/steveyegge/beads)), driven
through its `bd` command. A *repo* tracker is a `.beads` directory in a project;
a *hub* is a shared tracker several projects or hosts use.

**Checkpoint.** Private continuation state Lambe writes under its state
directory (pinned workflow, attempt, turn, thread, retry deadline) so a restart
resumes instead of starting over.

**Claim.** An issue marked in progress and assigned to a Lambe *actor*. Claims
are what stop two supervisors taking the same work. A claim whose agent is gone
and whose workspace or checkpoint is missing is an *orphaned claim*; `lambe
claims list` shows them and only `lambe claims release` frees one.

**Actor.** The tracker identity Lambe stamps on claims and heartbeats
(`lambe@<machine>` by default). Hosts sharing a tracker need distinct actors.

**Formula.** A TOML file in `.beads/formulas/` that defines a recurring
*responsibility* (steps, a trigger) and/or a *gate*.

**Gate.** An external check that must pass before an issue is closed: `shell`
(a command), `gh:run` (a GitHub Actions run) or `gh:pr` (a pull request).
Results are green, red (counts as a failed attempt) or error (retried without
using an attempt). The agent saying "done" never closes an issue.

**Harness.** The adapter that drives one coding agent: `pi`, `codex` (through
ACP) or `acp` (any configured ACP agent).

**Heartbeat.** Periodic extension of a claim's *lease* (`supervisor.heartbeat_interval`).
Separate from the *loop heartbeat* (`tick_at`) that `lambe doctor` uses to spot
a wedged daemon.

**Lease.** The time-limited right a claim confers. If heartbeats stop, the lease
expires; with `supervisor.auto_reclaim` a shared-server deployment can then
reclaim the issue for another host.

**Needs-human.** An issue Lambe stopped working because it exhausted its
attempts or hit a hard gate failure. It appears in `lambe status` and the
dashboards until a person acts.

**Stall.** A running run with no agent activity for `supervisor.stall_after`
(default 5m). Observation only; the *watchdog* acts later, at
`supervisor.silence_timeout` (default 10m): probe, then kill.

**State directory.** Where Lambe keeps its lock, checkpoints and live
descriptors (`lambe state-dir`; override with `LAMBE_STATE_DIR`). Keep it across upgrades.

**Symphony.** The OpenAI specification for an issue-driven agent orchestrator:
poll, claim, workspace, run, reconcile, retry. Lambe implements it (pinned to a
specific revision) and adds durable state, claim fencing and verified completion.

**Tick.** One pass of the supervisor loop (`supervisor.tick_seconds`, default 10s).

**Workflow.** The `WORKFLOW.md` file: YAML front matter (hooks, limits) and the
agent prompt template. It hot-reloads; in-flight runs finish under the revision
they claimed. A *named workflow* routes issues carrying a label to a different
workflow file, gate directory and harness.

**Workspace.** The sanitized per-issue directory an agent works in, created on
first use and cleaned up when the issue is terminal.
