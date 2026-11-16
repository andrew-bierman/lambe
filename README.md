# lambe

[![CI](https://github.com/andrew-bierman/lambe/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/andrew-bierman/lambe/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/andrew-bierman/lambe.svg)](https://pkg.go.dev/github.com/andrew-bierman/lambe)
[![Go Report Card](https://goreportcard.com/badge/github.com/andrew-bierman/lambe)](https://goreportcard.com/report/github.com/andrew-bierman/lambe)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Keep your agents working while you sleep.**

lambe is an MIT-licensed Go supervisor daemon for autonomous coding agents. It
implements the OpenAI Symphony specification (§18.1, pinned to commit
`8001b52`) and is Beads-native: it works against a Beads hub clone, a standalone
repo, or an in-repo `.beads` directory. lambe is not affiliated with OpenAI;
the Symphony specification is Apache-2.0 and is attributed in source headers.

## Why

Autonomous agents stop for reasons that are nobody's fault: they finish a task
and wait, they hit a tool error and give up, or — the documented dominant
failure — the supervisor overseeing them dies and nothing restarts it. lambe
is the layer that does not stop. It holds durable desired state, drives agents
through work item by work item, and treats a task as done only when external
checks are green — never because a process exited zero. That last property,
verified completion, is the product.

## Quickstart

Install (Go 1.26+):

```sh
go install github.com/andrew-bierman/lambe/cmd/lambe@latest
```

While the repository is private, Git must have access to it (for example,
through `gh auth login` and `gh auth setup-git`). Install with:

```sh
GOPRIVATE=github.com/andrew-bierman/lambe go install github.com/andrew-bierman/lambe/cmd/lambe@latest
```

Prebuilt static binaries for macOS and Linux are published via GoReleaser on
the releases page; a Homebrew tap is planned.

Install `bd` and `pi` and authenticate Pi with your model provider first.
Pi needs a current Node.js runtime; check `node --version` and `pi --help`
in the same shell you will use to start lambe.

Single machine — lambe defaults to the current directory's Beads tracker.
Run from the project root containing `.beads` (initialize it with `bd init`
if needed), then create `WORKFLOW.md` with the
agent instructions and a completion gate before starting:

```sh
cd ~/Code/my-beads-repo
gh api repos/andrew-bierman/lambe/contents/WORKFLOW.md -H 'Accept: application/vnd.github.raw' > WORKFLOW.md
mkdir -p .beads/formulas
# Go example: replace the command with your project's actual checks.
cat > .beads/formulas/verify.formula.toml <<'EOF'
[trigger]
command = "exit 0" # Manual tasks only; no automatic materialization.

[gate]
type = "shell"
command = "go test ./..."
EOF
lambe run
```

Edit `WORKFLOW.md` for your project before dispatching work. A gate is
required: without one, lambe cannot verify completion and parks the task.
Agents work in separate directories; use the workflow's `after_create`
hook to clone your project into each workspace before running project checks.

To use a tracker elsewhere, create a config file and pass it to both commands:

```yaml
# lambe.yaml
trackers:
  - name: main
    type: hub
    path: /absolute/path/to/beads-hub
```

```sh
lambe run -config lambe.yaml
lambe status -config lambe.yaml
```

The morning read:

```sh
lambe status
```

`lambe run` is the daemon (single instance per machine, enforced with flock).
`lambe status` prints the current snapshot: agents, assignments, gate results,
and anything waiting on a human. `lambe version` prints the build version.

## Adapter seams

lambe is built around a small set of seams. What exists today:

| Seam | Default | Alternatives |
| --- | --- | --- |
| Tracker | Beads (hub, repo, or in-repo `.beads`) | Linear / GitHub / Jira via the Symphony adapter surface |
| Harness | Pi | ACP next |
| Store | Embedded Beads local store | — |
| Gates | `shell`, `gh:run`, `gh:pr` | — |
| Surfaces | none yet | MCP, Rex planned |

## Configuration

Precedence: defaults < config file < environment < flags. Environment variables
flatten config paths with underscores (`sync_interval` → `LAMBE_SYNC_INTERVAL`).
Config files inherit the local tracker default unless they set `trackers`.

Key knobs and defaults:

| Setting | Default | Meaning |
| --- | --- | --- |
| `tick` | 10s | Supervisor loop interval |
| Attempt cap | 8 | Max attempts per work item before needs-human |
| Silence timeout | 10m | Agent produces no output for this long → reclaim |
| Turn timeout | 1h | Hard cap on a single agent turn |
| Heartbeat | 150s | Liveness interval written to the tracker |

The supervisor dispatches agent workers without waiting for their turns. Set
`agent.max_concurrent_agents` in `WORKFLOW.md` to cap active workers (default 10);
`agent.max_turns` sets the turn limit (default 20). Reloading the workflow changes
the dispatch limit for subsequent work; reducing it leaves existing workers running.
The older top-level `max_turns` remains accepted, with `agent.max_turns` taking precedence.

Agent turns use the turn and silence limits above. Hub synchronization, formula
evaluation, and gate evaluation have separate control deadlines (half the silence
timeout, with a one-minute minimum). Tracker subprocesses also have a one-minute
per-command bound. A gate timeout retries the gate on the same settled output;
it does not consume another agent attempt.

## OS-level restart (production)

The recommended production setup is to let the operating system keep `lambe
run` alive, so the documented dominant failure — supervisor death — is covered
by the platform, not by lambe itself. Example service units ship in
[examples/](examples/):

- **macOS (launchd):** `KeepAlive` restarts the daemon on exit. Copy the
  plist, adjust paths, then `launchctl load`.
- **Linux (systemd user):** `Restart=on-failure`. Copy the unit, adjust paths,
  then `systemctl --user enable --now lambe`.

Lambe keeps private session checkpoints under its state directory. Restarting
with the same state directory, tracker, and actor restores the pinned workflow,
attempt, turn, thread, and retry deadline. Interrupted turns replay within the
same attempt; pending gate checks reuse the settled output. Workspace files
remain available. Keep the state directory across upgrades.

Before replacing a surviving agent, recovery verifies the tracker claim and
the process's kernel identity on macOS or Linux. A foreign claim or unverifiable
process is left alone. Recovery errors are logged and retain the workspace;
transient tracker failures are retried on subsequent ticks. Claims from older
versions without checkpoints require inspection before releasing them for a
fresh run. Do not release one while its old agent is still working.

Checkpoints preserve Lambe's continuation state. Backend conversation resumption
also depends on the harness: Pi reopens its workspace session; the current ACP
driver creates a new backend conversation. A replay can repeat an interrupted
tool operation, so tasks with external side effects still need idempotent steps.

## Symphony conformance and divergence

lambe implements the Symphony specification as pinned to `8001b52` and stays
compatible with other Symphony orchestrators. Where lambe goes further, it does
so as a durable superset: state survives restarts, and agent ownership is
fenced through Beads claims so two supervisors can share a tracker without
double-assigning work.

## Status

Early development. Expect rough edges and changing interfaces. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Roadmap

Done:

- [x] Go module scaffold, config, logging, toolchain pins, GoReleaser
- [x] Beads tracker adapter (hub, repo, in-repo `.beads`, embedded local store)
- [x] Process-supervision primitives (spawn ownership, kill ladder, watchdog)
- [x] Symphony core loop (WORKFLOW.md render, workspaces, retry/reconcile)
- [x] Pi harness driver (RPC client, settle-only completion, limit classifier)
- [x] Docs and CI

In flight:

- [x] Additional harness driver (Pi)
- [x] Verified-completion gates (`shell`, `gh:run`, `gh:pr`)
- [x] Daemon assembly (`lambe run`, `lambe status`, single-instance lock)
- [x] Fault-injection suite as the definition of done
- [x] MCP server surface
- [x] ACP harness driver
- [x] Rex surface
- [x] HTTP state API

Next:

- [ ] Multi-machine affinity and failover
- [ ] Shared Dolt-server mode
- [ ] More harness drivers (Codex, Claude Code, OpenCode)
- [ ] `bd`-as-library embedding
- [ ] Limit-classifier tuning on real traffic

Later / exploring:

- [ ] Gas-City interop
- [ ] `ghx` caching proxy at multi-agent scale

## License

MIT. The OpenAI Symphony specification (Apache-2.0) is attributed in a
NOTICE-style comment in source; lambe is not affiliated with OpenAI.
