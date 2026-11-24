# Architecture

A contributor's map of how Lambe is built. It describes the code as it is;
the planning-era research that produced it is in
[research/architecture-pre-planning.md](research/architecture-pre-planning.md).
Terms are defined in the [glossary](GLOSSARY.md).

## What it is

Lambe is one static Go binary, `lambe`. Run as a daemon, it polls a Beads
tracker for ready work, claims an issue, prepares an isolated workspace, runs
a coding agent in it, and closes the issue only when an external gate proves
the work is done. It owns no database: the tracker is its state, plus a small
private checkpoint directory so a restart resumes where it stopped.

The loop follows the [Symphony specification](https://github.com/openai/symphony),
pinned in [REFERENCE.md](REFERENCE.md#symphony-conformance-and-divergence).
Anything Lambe adds (durable state, claim fencing, verified completion) is a
superset, not a change to the loop's contract. **Changes to
`internal/symphony` and the supervisor loop are spec changes, not refactors.**

## The loop in one picture

```
lambe run
  daemon  (single-instance lock, signals, tick loop, state publishing)
    each tick:
      tracker.Dispatchable()          Beads: ready, unclaimed, unblocked issues
        -> claim (lease, actor)       fenced by the Beads claim
        -> workspace + 4 hooks        sanitized per-issue directory
        -> harness.Run()              Pi / ACP agent, typed event stream
             watchdog: silence timeout -> probe -> kill ladder
        -> agent says done
        -> gate.Evaluate()            shell / gh:run / gh:pr -> green | red | error
             green: close the issue    red: record rejection, retry
      heartbeat claims, reconcile, retry queue (10s * 2^n)
  surfaces read the same state: status, logs/tui, MCP, Rex, HTTP state API
```

## Package map

| Package | Responsibility |
| --- | --- |
| `cmd/lambe` | The single binary and its commands. `cli.go` dispatches, help text lives in `helpTopics` and also drives the shell completions; `main.go` holds `run`, `status`, `init`. Tests drive the CLI through `execute(args, stdin, stdout, stderr)`. |
| `internal/config` | Loads defaults, file, environment; validates; rejects unknown keys and wrong types. `keyTable` in `keys.go` is the single source for validation and for the generated [config reference](reference/config.md). |
| `internal/logging` | Structured JSON `slog` for the daemon. One-shot commands print plain text instead. |
| `internal/proc` | Process supervision primitives: spawn in its own process group, LF-strict JSONL frame reader, the SIGTERM -> grace -> SIGKILL ladder, the silence/max-run watchdog, process identity on macOS and Linux. |
| `internal/tracker` | Typed interface to Beads through the `bd` binary (hub, repo and in-repo layouts): dispatchable work, claims and leases, wake timers, attempt and session records. |
| `internal/symphony` | The Symphony-conformant core: WORKFLOW.md loader with hot reload, workspaces and hooks, strict prompt rendering, retry queue, reconciliation, checkpoints, the orchestration loop. Owns WORKFLOW.md semantics. |
| `internal/harness` | The backend-agnostic agent contract (`driver.go`) and its drivers: `pi` (RPC) and `acp` (Agent Client Protocol, including Codex). |
| `internal/gate` | Verified completion: gate definitions from formula TOML, the three-valued evaluator, escalation counters, tracker writeback. |
| `internal/daemon` | Assembles everything: the instance lock, startup recovery of orphaned process groups, the tick loop, claim heartbeats, the live-run registry, status snapshots, orphan-claim handling. |
| `internal/httpstate`, `internal/stateapi` | The opt-in, bearer-guarded read-only state API (server and Go client). Wire contract: [api/state-api.md](api/state-api.md). |
| `internal/mcp` | MCP server over stdio exposing supervised runs as tool calls. It never owns run state; it proxies into the daemon's control stack. |
| `internal/surface/tui`, `internal/surface/rex` | Terminal dashboard and Rex integration. Read-only views over the same snapshots. |
| `internal/testchild`, `internal/testdolt` | Test helpers: a re-exec fake agent and an isolated Dolt server for multi-host tests. |
| `test/faultsuite`, `test/dashsmoke` | The fault-injection suite (the definition of done for the loop) and dashboard smoke tests. |
| `apps/dashboard` | LambeDash, the read-only SwiftUI app over the state API. |
| `contrib/rex` | Rex palette actions installed by `lambe setup rex`. |

## Key design decisions

- **The tracker is the state.** Claims, attempts, sessions and gate results
  are Beads records. Local files are checkpoints, never a second source of truth.
- **Done means a gate says so.** The agent's final message is never trusted.
  A red gate counts as a failed attempt; an erroring gate retries on the same
  settled output without consuming an attempt.
- **Platform code, not LLMs, watches agents.** The watchdog is a timer ladder
  in `internal/proc`, so a wedged agent cannot talk its way out of a kill.
- **Claims fence ownership.** Two supervisors sharing a tracker cannot take the
  same issue; reclaim is opt-in (`supervisor.auto_reclaim`).
- **Adapters at every seam.** Tracker, harness, gate and surface are interfaces
  with one first-party default each, so new ones add code rather than change the loop.
- **Surfaces own nothing.** TUI, Rex, MCP and the dashboard render or proxy
  daemon state; none can mutate it except through the same control stack.

## Where state lives

| What | Where |
| --- | --- |
| Work, claims, leases, attempts, gate results | The Beads tracker (`.beads/`) |
| Pinned workflow, attempt, turn, thread, retry deadline | Checkpoints under the state directory (`lambe state-dir`) |
| Per-issue workspaces | `workspace.root`, default under the state directory |
| Instance lock and live state-API descriptor | State directory (`lambe.lock`, `live/`) |
| Config | `~/Library/Application Support/lambe/config.yaml` (macOS), `$XDG_CONFIG_HOME/lambe/config.yaml` or `~/.config/lambe/config.yaml` (Linux) |

## Contributing a change

- Toolchain and test commands are in [AGENTS.md](../AGENTS.md); run
  `LAMBE_TEST_BD=1 go test -race ./...` for tracker work.
- A new flag or command: add its page to `helpTopics` in `cmd/lambe/cli.go`
  (completions and `lambe help` follow automatically) and a case to the CLI tests.
- A new config key: add it to `keyTable` in `internal/config/keys.go`, then run
  `go run ./internal/config/gen docs/reference/config.md`; a test fails if you forget.
- Exit codes and output conventions are in [REFERENCE.md](REFERENCE.md#help-and-exit-codes).
