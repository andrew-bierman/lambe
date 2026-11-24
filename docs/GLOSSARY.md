# Glossary

Terms Lambe uses for Beads and Symphony concepts, each with a link to the
code or documentation it is grounded in where one exists.

## Actor

The tracker identity Lambe stamps on claims and heartbeats (`lambe@<machine>` by default). Hosts sharing a tracker need distinct actors.

## Attempt

An attempt is one supervised agent run on a work item, tracked by number: a
failed attempt backs off and consumes the next number, while a continuation or
gate retry reuses the current one. The cap is 8 by default, after which the
item moves to needs-human ([Configuration](REFERENCE.md#configuration),
[retry kinds](../internal/symphony/retry.go)).

## Beads

Beads is the external task tracker and the source of truth for what is queued
and claimed; Lambe reads the work front through `bd` and writes shared state
back to it instead of keeping a second store. See [How it relates to other
projects](../README.md#how-it-relates-to-other-projects) and the
[tracker package](../internal/tracker/tracker.go).

## Checkpoint

Private continuation state Lambe writes under its state directory (pinned workflow, attempt, turn, thread, retry deadline) so a restart resumes instead of starting over.

## Claim

A claim is the Beads ownership record Lambe takes on an issue before running
an agent; claims are atomic server-side, so only one host wins a contested
claim and every Lambe write is fenced by it. A losing write gets
`tracker.ErrLostRace` and stands down ([Design](MULTI-MACHINE.md#design),
[The sync model](OPERATIONS.md#the-sync-model)). A claim whose agent is gone
and whose workspace or checkpoint is missing is an *orphaned claim*;
`lambe claims list` shows them and only `lambe claims release` frees one.

## Formula

A TOML file in `.beads/formulas/` that defines a recurring *responsibility* (steps, a trigger) and/or a *gate*.

## Gate

A gate is the external completion check that decides whether a task is done —
an agent's exit code is not enough. Gates are defined in git-versioned formulas
as `shell`, `gh:run`, or `gh:pr`, and a rejected gate feeds its output back as
retry guidance ([Completion gates](../README.md#completion-gates),
[gate package](../internal/gate/gate.go)).

## Harness

The adapter that drives one coding agent: `pi`, `codex` (through ACP) or `acp` (any configured ACP agent).

## Heartbeat

Periodic extension of a claim's *lease* (`supervisor.heartbeat_interval`). Separate from the *loop heartbeat* (`tick_at`) that `lambe doctor` uses to spot a wedged daemon.

## Lease

A lease is the time-bounded validity of a claim: Beads fixes its TTL at 5
minutes and a healthy daemon renews it every `supervisor.heartbeat_interval`
(default 150s). Leases live in an ephemeral table on the Beads server and are
not federated across remotes; an expired lease can be reclaimed so another host
can take the item ([Design](MULTI-MACHINE.md#design),
[Heartbeat, TTL, and reclaim](OPERATIONS.md#heartbeat-ttl-and-reclaim)).

## Needs-human

Needs-human is the terminal state for an item Lambe stops retrying on its own —
reached when it hits the attempt cap (default 8) without passing its gates, or
when a gate fails in a way known not to be transient. A person resolves it by
reading `lambe status`, fixing the cause, and resetting the item's attempts in
the tracker; there is no resume command
([Needs-human](OPERATIONS.md#needs-human)).

## Stall

A running run with no agent activity for `supervisor.stall_after` (default 5m). Observation only; the *watchdog* acts later, at `supervisor.silence_timeout` (default 10m): probe, then kill.

## State directory

Where Lambe keeps its lock, checkpoints and live descriptors (`lambe state-dir`; override with `LAMBE_STATE_DIR`). Keep it across upgrades.

## Symphony

The OpenAI specification for an issue-driven agent orchestrator: poll, claim, workspace, run, reconcile, retry. Lambe implements it (pinned to a specific revision) and adds durable state, claim fencing and verified completion.

## Tick

One pass of the supervisor loop (`supervisor.tick_seconds`, default 10s).

## Workflow

The `WORKFLOW.md` file: YAML front matter (hooks, limits) and the agent prompt template. It hot-reloads; in-flight runs finish under the revision they claimed. A *named workflow* routes issues carrying a label to a different workflow file, gate directory and harness.

## Workspace

The sanitized per-issue directory an agent works in, created on first use and cleaned up when the issue is terminal.
