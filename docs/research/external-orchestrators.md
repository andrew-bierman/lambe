# External evidence dossier: agent-orchestration systems vs. durable supervision
Researched 2026-11-15. Sources read directly (clones at /tmp/opencode/orch-research) or fetched as cited.
Legend: [V] = confirmed by reading source/spec text at cited path · [D] = documentation claim · [I] = issue report (real-world) · [S] = single-source.

## 1. OpenAI Symphony — github.com/openai/symphony (28.1k stars; 47 commits 2026-03-04→2026-09-15, release 0.0.3; spec + Elixir reference impl)

- Caller claim CONFIRMED — spec explicitly scopes durability to the tracker, not the scheduler:
  SPEC.md §2.1 Goals: "Support tracker/filesystem-driven restart recovery without requiring a
  persistent database; **exact in-memory scheduler state is not restored**." [V, verified: https://github.com/openai/symphony/blob/main/SPEC.md]
- SPEC.md §4.1.8 "Orchestrator Runtime State": "Single authoritative **in-memory** state owned by
  the orchestrator" — fields: `running` map, `claimed` set, `retry_attempts` map; RetryEntry
  §4.1.7 holds `due_at_ms (monotonic clock)` + `timer_handle` — both unrecoverable after process death. [V, same URL]
- SPEC.md §8.5/§8.6: reconciliation every poll tick (stall detection + tracker refresh); startup does
  only terminal-workspace cleanup — restart re-discovers work purely by re-polling the tracker;
  in-flight agent turns are lost and re-dispatched fresh. [V, same URL]
- SPEC.md §2.2 Non-Goals: "General-purpose workflow engine or distributed job scheduler." [V, same URL]
- Reference implementation matches spec: `elixir/lib/symphony_elixir/orchestrator.ex` is one GenServer
  whose `defstruct State` holds running/claimed/blocked/retry_attempts in memory, rebuilt empty in
  `init/1`; no DETS/ETS/persistence anywhere in lib/. [V, verified: https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/orchestrator.ex]
- Maturity: engineering preview, own warning "low-key engineering preview for testing in trusted
  environments" (README). 8 months old, ~2 months since last commit. [V/D, verified: https://github.com/openai/symphony]

## 2. symphony-beads — github.com/ahkohd/symphony-beads (2 stars, 0 forks, v0.1.7; ALL 178 commits on 2026-03-18/19/20; dormant ~8 months)

- Caller claim CONFIRMED — execution + retry state is process-local:
  `src/orchestrator.ts:30` `private retries = new Map<string, RetryEntry>()`; retries armed with
  `setTimeout` (`src/orchestrator.ts:346`); running agents tracked in an in-memory `running` map;
  `stop()` kills child processes and clears timers only. [V, verified: https://github.com/ahkohd/symphony-beads/blob/main/src/orchestrator.ts]
- What survives restart [V, src/lock.ts + README]: only `.symphony.lock` (pid/path/timestamp) and
  `~/.symphony/instances/<hash>.json` (instance registry). No task, attempt, or workflow state is
  persisted anywhere by the orchestrator. Grep across src/ found writes only for prompt/result
  scratch files (`src/runner.ts:100,110`) and lock/registry.
- Durable state lives OUTSIDE the process: Beads issue states (open/in_progress/review/closed) drive
  dispatch on every 30s poll (`src/tracker.ts`); after a crash, issues still active in Beads are
  re-dispatched at attempt 1 (attempt counter resets; no attempt cap → unbounded re-runs across
  restarts). Default runner is `pi --no-session` (WORKFLOW.md front-matter default) → no agent
  session to resume; the per-issue workspace + `issue/<id>` git branch are the only work carryover
  (before_run hook re-checkouts it). [V, verified: README + src/workspace.ts + src/orchestrator.ts]
- Retry/backoff mirrors Symphony spec: failure delay `min(10000*2^(attempt-1), max_retry_backoff_ms)`,
  continuation 1s (`src/orchestrator.ts:329-346`). PR-review rework state re-queried live from `gh`
  each poll (`src/pr-monitor.ts`) — no persisted PR state. [V]
- Maturity: 305 `it(...)` test cases across 10 files, CI workflows, npm-published — but tests mirror
  orchestrator logic in standalone functions rather than exercising the real methods
  (src/orchestrator.test.ts header comment), and the project is a 3-day burst, dormant since March
  2026. 1 open issue, 0 forks. Reference-implementation grade, not foundation grade. [V]

## 3. Axel — github.com/lgalabru/axel (305 stars, 19 forks, created 2026-01-29, active: pushed 2026-11-13; 6 open issues) + axel.md

- Architecture is interactive/thread-oriented, orthogonal to unattended supervision [V, verified:
  https://github.com/lgalabru/axel/blob/main/docs/DESIGN.md]:
  - DESIGN.md line 25: "**No daemon in v0**; one appears only for always-on sync."
  - No scheduling/cron/heartbeat/queue mechanism anywhere in DESIGN.md (grep: zero hits).
  - "Autopilot" (line 243, §Autopilot: no babysitting) = permission mode (bypassPermissions + guard
    hook), NOT autonomous scheduling/supervision.
- Persistence is real but interaction-centric [V, DESIGN.md §schema + crates/store/src (rusqlite)]:
  SQLite store of threads/sessions/messages/status/usage; sessions keep `harness_session_id`,
  `transcript_path`, `resumed_from_session_id` enabling `claude --resume`/`thread/resume`;
  `axel thread resume THREAD` continues the last conversation.
- Recovery exists but is message-triggered, not supervisor-driven [V, DESIGN.md lines 631-634]:
  "Recovering a session: **a message** for a managed session whose host has died ... ends that
  session and resumes the agent on the thread ... with the message as the first prompt." i.e. an
  unattended dead session stays dead until a human/agent sends a message. Sync followers "restart
  after a reboot" only when `axel ui` runs (line 338). [I/S: no independent Axel run reports found]
- Session lifecycle model: `starting → running ⇄ blocked ⇄ idle → ended` per session; thread state
  coarse (open|done|archived). No desired-state reconciliation loop for ongoing responsibilities. [V]

## 4. OpenClaw — github.com/openclaw/openclaw (394.5k stars; created 2025-11-24; canonical repo confirmed; docs docs.openclaw.ai, openclaw/docs)

- Scheduling architecture [D, verified: https://docs.openclaw.ai/architecture + https://github.com/openclaw/openclaw/blob/main/docs/cli/cron.md + docs/automation/cron-jobs.md]:
  - Gateway daemon owns one `GatewayScheduler`; "For durable work, stores retain deadlines and their
    owners reconstruct schedules at startup rather than persisting a second scheduler state."
  - Cron jobs persist in `~/.openclaw/cron/jobs.json`, runtime state in `jobs-state.json`; restarts
    don't lose schedules; overdue isolated jobs on startup are rescheduled (deferred), not replayed.
  - Recurring jobs retry with exponential backoff 30s→1m→5m→15m→60m after consecutive errors.
  - Heartbeat (since v2026.8.1) is a system-owned automation job: periodic main-session turn, busy
    deferral, event wakes rate-limited; config is "desired state" written through to monitor rows.
- Real-world unattended failures: a RECURRING failure class, ≥6 distinct incidents in 2026, all
  "scheduler silently stops while gateway reports healthy":
  - #7588 (Feb 2026): cron never fires, nextWakeAtMs null. [I, verified: https://github.com/openclaw/openclaw/issues/7588]
  - #12276: timer initialized but never fires; `cron status` looks healthy; fixed by #12303 re-arm
    safety net. [I, verified: https://github.com/openclaw/openclaw/issues/12276]
  - #11013 (Feb 2026): auto-dispatch stalls after SIGUSR1 restart; user workaround = system crontab
    calling `openclaw cron run --force`. [I, verified: https://github.com/openclaw/openclaw/issues/11013]
  - #26004 (Feb 2026) + #52819 (v2026.3.13): heartbeat fires only on restart. [I]
  - #78187 (May 2026): heartbeat silently stops after SIGUSR1 restart, 9.5h gap while cron jobs
    kept running; fixed by #88133 in v2026.6.1 (bounded heartbeat timeout, lifecycle reset). [I,
    verified: https://github.com/openclaw/openclaw/issues/78187]
  - #132859 (v2026.7.1-2): heartbeat never fires a single tick on a fresh process; fixed by
    cron-owned monitor + lost-heartbeat repair (2026-07-29). [I]
  - #141633 (v2026.9.2): 31 of 33 enabled automation jobs with no execution evidence; stall
    persisted across fresh gateway processes; fixed by #140825 in 2026.9.3 (owner-resolution stall). [I,
    verified: https://github.com/openclaw/openclaw/issues/141633]
- Read: even the most-deployed personal-agent gateway (hundreds of thousands of users) shipped
  repeated silent-supervisor-death bugs through v2026.9; each needed its own detection + repair
  mechanism (doctor --fix, lost-heartbeat repair, two-liveness-signal checks).

## 5. Hermes — github.com/NousResearch/hermes-agent (NOT TheAxitor; Nous Research; release v2026.8.31 "Pantheon")

- Scheduling architecture [D, verified: https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/cron.md]:
  - Gateway daemon ticks scheduler every 60s; jobs persist in `~/.hermes/cron/jobs.json`; file lock
    `.tick.lock` prevents overlapping ticks; `hermes cron tick` allows an external system crontab.
  - Durability semantics are explicit and PARTIAL: "Agent-backed runs retain their fire identity
    until output and a durable delivery handoff are recorded. The scheduler reconciles unfinished
    receipts on each tick, **including after an owner restart**. A known terminal result is handed
    to the delivery queue without running the agent again... **Delivery interrupted after a send
    claim stays unknown and is not retried automatically.** An interrupted execution ... stays
    paused for operator review. **Paused jobs are not automatically resumed**."
  - Event-triggered cron (webhook fires job); no-agent script mode; catch-up-on-resume config
    (`cron.catch_up_missed`); `continuity` + durable notepads give scheduled agents memory (v2026.8.31).
- Real-world unattended failures: the SAME silent-ticker-death class, ≥7 incidents, multiple fixed:
  - #20302: no health monitoring for cron ticker; observed 7.5h silent outage from a syntax error. [I]
  - #32895 + #60703: ticker thread dies silently; `hermes gateway status` says running; heartbeat
    age grows; fix #60855 bounded the jobs lock + claim freshness. [I, verified:
    https://github.com/NousResearch/hermes-agent/issues/60703]
  - #36854 (June 2026): one scanner-blocked job killed the ticker thread → total cron outage until
    restart; fixed in 413990c94 (3-layer containment). [I]
  - PR #50016: ticker caught only `Exception` and logged at DEBUG, so `SystemExit` killed it
    silently; fix added `BaseException` catch + two liveness signals (ticker_heartbeat vs
    last_success) + STALLED status detection. [I, verified:
    https://github.com/NousResearch/hermes-agent/pull/50016]
  - #87644 (Aug 2026): EMFILE → scheduler stalled ~24h while gateway heartbeat stayed fresh;
    multiplex startup recovery had no `except` at all; containment fixed by #74888; maintainer
    concedes the health-plane projection gap ("nothing was projecting it into the surface your
    monitors watch") remains open. [I, verified:
    https://github.com/NousResearch/hermes-agent/issues/87644]
  - #18004 (open, updated 2026-10-15): cron timeout marks job failed while the agent thread keeps
    running side effects (ThreadPoolExecutor can't kill a running thread). [I]
  - #2788: cron silently never fires in CLI-only mode (gateway required) — fixed with warnings. [I]

## Cross-project: what none of them solve

1. Durable ongoing RESPONSIBILITIES. All five are task/run-oriented (issue, thread, job, message).
   Symphony/beads: finite tickets; a ticket reaching review/closed is done. None models "keep CI
   healthy" as a maintained invariant with an owner that must exist every tick regardless of queue. [V across all five]
2. Supervision of the supervisor. The dominant 2026 failure mode in the two largest deployed
   systems (OpenClaw, Hermes) is the scheduler itself silently dying while the host process and
   generic health checks report healthy — ≥13 distinct incidents above. Both needed months of
   liveness-signal hardening (two-signal heartbeats, STALLED detection, doctor repair, lost-work
   repair). Nothing here hands you a working external watchdog; you would still build the
   "external system maintains desired state" layer — which is exactly the caller's proposal. [I, corroborated across both repos]
3. Resume-vs-rerun semantics. Symphony/beads re-dispatch from scratch (in-memory state lost, agent
   session discarded). Axel resumes only when a message arrives. OpenClaw/Hermes have genuine
   restart recovery but with documented holes (Hermes: paused-stays-paused, delivery-after-claim
   unknown; OpenClaw: overdue jobs deferred, stall persisted across processes until v2026.9.3).
4. Maturity gradient: Symphony = 8-month-old spec + preview reference impl; symphony-beads = 3-day
   dormant v0.1.7 (reference grade); Axel = 10 months, active, but explicitly no daemon/no scheduler
   in v0; OpenClaw/Hermes = heavily hardened but general-purpose chat-assistant schedulers, not
   coding-agent supervisors with workspace/PR/CI semantics.
