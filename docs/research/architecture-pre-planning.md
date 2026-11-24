# lambe — architecture (pre-planning research snapshot)

> Historical: the planning-era document from the 2026-11-15 research run. For how the code is organized today, read [docs/ARCHITECTURE.md](../ARCHITECTURE.md).

Positioning: **a Symphony-compatible, Beads-native supervisor for autonomous coding agents.**
Tagline: *Keep your agents working while you sleep.*

This document is the consolidated architecture from the 2026-11-15 research run.
Evidence with full citations lives in `docs/research/`.

## Mission

Autonomous coding agents stop unattended for reasons infrastructure can mostly
fix — silent supervisor death (13+ documented 2026 incidents across OpenClaw and
Hermes), usage-limit lockouts, crashes, missed events, hung turns — plus two
classes infrastructure alone cannot: premature "done" declarations and
incomplete side effects. No existing system owns the missing layer: an external
desired-state + watchdog that launches, monitors, replaces, and resumes agent
sessions across machines. lambe is that layer, implemented against the OpenAI
Symphony specification, with Beads as its entire state substrate.

## System overview

```
 SURFACES (optional, swappable adapters)
   rex      — session.create w/ labels, OSC 7501 status, rex send = emergency
              steering only; plain PTY fallback when rex absent (Axel pattern:
              structured over scraped, never capture-and-grep)
   mcp      — thin proxy to the daemon; NEVER owns state (stdio MCP servers die
              in ~500ms with the client session). Accept-and-return-handle,
              then status/output/cancel tools; heavy output to artifacts
              (portable de-facto pattern; Claude truncates at 25k tokens,
              Codex caps calls ~300s)
   http     — the opt-in, bearer-guarded local state API (Symphony §13.7
              shape, versioned contract in docs/api/state-api.md) for the
              SwiftUI dashboard and future clients
   ui       — the read-only SwiftUI operator dashboard (apps/dashboard),
              the fifth surface: renders over the state API, owns nothing

 SUPERVISOR DAEMON (Go, one static binary per machine — the only net-new code)
   Symphony-compatible core (SPEC @ 8001b52, pinned; quarterly re-diff):
     poll → dispatchable → claim → sanitized workspace → 4 hooks →
     run agent → reconcile → retry 10s·2ⁿ (+1s continuation) → terminal cleanup
   lambe extensions (the differentiation):
     • durable runtime state — implements spec §18.2's own TODO roadmap
     • multi-orchestrator profile — fencing via adapter-computed
       `dispatchable` (spec has no claim primitive; Beads claims slot in)
     • verified-completion gate — proof-of-work is NOT normative; this is ours:
       done = external checks green (tests/CI/typecheck), never exit code
     • the scheduler bd doesn't have — reclaim timer, bd gate check loop,
       defer-wake reads, silence-timeout watchdog (two liveness signals:
       process heartbeat ≠ last-successful-tick), limit-aware deferral
       → wake-at-reset
     • resume-vs-reconstruct: same-machine restart → harness session resume
       by id; cross-machine takeover → re-run with context from Beads record

 STATE — three homes, nothing duplicated
   HUB (exists today: agent-coordination, Beads on Dolt, git-remote over
   GitHub): work items, claims, deps, progress comments, bd remember.
     Responsibilities = formulas (TOML in .beads/formulas/ inside the hub
     repo, git-versioned; cook → pour on trigger; conditional steps, gates).
   LOCAL (per machine, never pushed): embedded-Dolt Beads tracker
     (in-process, file-lock single-writer, no server): session bindings and
     attempt counters as JSON metadata, wake timers via native
     defer_until/due_at (--defer auto-wakes on next read; --overdue),
     liveness checkpoints on transition only, wisps for ephemeral runs,
     pinned issue ids as idempotency keys (1.4.0 tombstones), types.retained
     protecting the op type from sweeps.

 MULTI-MACHINE MODEL
   Default: machine-affinity routing — each responsibility names a home
   machine; zero contention. Leases are per-replica and never federate, so
   this is the honest default.
   Failover: bd reclaim --any-replica after TTL (5m) + grace (10m);
   invariants TTL > sync interval, grace > sync interval.
   Upgrade path: shared dolt sql-server mode (bd docs: "for sustained
   concurrent agents, use server mode") when tighter fencing is worth a
   server. Fence visibility otherwise = assignee status, stale one sync
   interval — documented, not pretended away.
```

## Adapter interfaces (the swap-everything rule)

Every layer boundary is a Go interface; ours are first-party defaults:

| Seam | Interface | Default | Alternatives |
|---|---|---|---|
| Tracker | Symphony §11 two-read adapter (+ published profile) | Beads hub | Linear/GitHub/Jira/Asana/GitLab (spec has reference adapters) |
| Harness | driver: spawn/events/settle/resume(id)/cancel/limit-signal/deadline | one harness in slice one | three tiers: headless CLI (codex exec, claude -p), vendor protocol (Pi RPC, Codex app-server, opencode serve), ACP (40+ agents, streaming + interrupts) |
| Store | local operational state | embedded Beads tracker | SQLite driver |
| Gates | completion check | external commands (tests, CI, gh:run/gh:pr native gates) | any executable check |
| Surface | presentation/control | none required | Rex, MCP server, HTTP state |

## Version and compatibility targets

- Symphony SPEC pinned to commit 8001b52 (2026-08-12 revision). Phrase:
  "implements the OpenAI Symphony specification (Apache-2.0)" — no implied
  endorsement, NOTICE/attribution retained. De-facto derivative bar
  (WORKFLOW.md + workspaces + two-read adapter + loop) is far below the full
  §18.1 checklist; we implement §18.1.
- Beads: target 1.4.0 (rc.2 current; near-weekly cadence). Needs from 1.4.0:
  --retain-ids tombstones, types.retained, SSE events:watch, HTTP
  heartbeat/reclaim. Degrade to 1.3.x polling gracefully. Note bd is itself
  Go with a library surface — embedding bd as a dependency instead of
  shelling out is an open evaluation for plan time.
- Harness matrix verified Nov 2026: all four document headless spawn, JSONL
  events, session resume by id, external cancel. Hang detection is the
  client's job everywhere (ours). Limit signaling: Codex first-class, Claude
  typed errors, OpenCode experimental, Pi none.

## Non-goals

- No LLM watchdogs (Gas Town token burn); watchdogs are platform code.
- No own state plane (Gas City sledgehammer); Beads is the substrate.
- No session-content interop across harnesses (config-migration only exists;
  live interop does not).
- No UI beyond the named surfaces: terminal logs/TUI, the loopback HTTP state
  API, and the read-only SwiftUI operator dashboard (`apps/dashboard`) as the
  fifth surface; Rex is the window when present. The dashboard renders only —
  it owns no state and mutates nothing.
- Config discipline: batteries-included defaults, zero-config single machine;
  adapter seams discoverable only on demand (the documented anti-example is
  Gas Town's Red-Queen config sprawl).

## Slice one (first implementation target)

One machine, one harness driver (user's daily driver), Symphony core loop
against the existing hub, formulas as responsibilities, gate-based verified
completion, local embedded Beads tracker, fault-injection suite as the
definition of done: supervisor kill -9 mid-reconcile (zero responsibility
loss), agent SIGKILL mid-turn (resume same session, no duplicate side
effects), simulated limit lockout (defer + wake), fake done with failing
tests (gate rejects), duplicate tick (no double dispatch).

## Open decisions

1. ~~Final name~~ — SETTLED: lambe. Collision-checked 2026-11-15: npm, crates,
   PyPI, Homebrew all free (only a small Dart pub.dev query-language package,
   different ecosystem). lyre rejected (npm/crates/PyPI taken; active
   commercial `brew install lyre` AI voice product); cory rejected (active
   Bitcoin tool on crates; first-name searchability).
2. First harness driver for slice one.
3. Hub fencing posture day one: affinity + reclaim (default) vs shared-server.
4. Language: Go selected in-session (user permitted Go/Elixir); confirm.

## Research index (docs/research/)

precedent-activity, project-grounding (Pigeon) · external-orchestrators,
external-durable-execution, external-harnesses-failures, external-gastown,
external-symphony-ecosystem, external-beads-local-mode (landscape) ·
deep-beads, deep-symphony-spec, deep-rex, deep-mcp-subagents (deep dives) ·
branding, branding-round2 (naming)