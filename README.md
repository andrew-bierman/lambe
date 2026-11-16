# lambe

<img src="docs/assets/lambe-mascot.png" alt="Lambe, a colorful friendly lambeosaurus" width="180">

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

## First-run setup

Run `lambe init` from the repository you want to supervise. It writes a config
that pins that repository’s `WORKFLOW.md`, so running Lambe from another folder
does not accidentally select a different prompt. Existing files are preserved.

Before dispatching real work, configure the workflow’s workspace hooks and the
completion check in a `[gate]` table in that starter or a separate formula file.
Every configured gate applies to every task in this tracker. Lambe refuses to
start without a completion gate, before claiming existing work. The starter’s automatic
trigger starts disabled; the starter uses native Beads formula and step fields.
Lambe starts the configured agent itself when an eligible task appears—you do
not start a second agent session. Queue a task with `bd create` after configuring
its completion gate, and inspect it with `lambe status`.

`lambe version` reports the installed Go module version for upstream installs,
even when no release linker flags were supplied. Bare commands read the default
configuration written by init; `-config FILE` selects another one.

## Quickstart

Install (Go 1.27+):

```sh
go install github.com/andrew-bierman/lambe/cmd/lambe@latest
```

While the repository is private, Git must have access to it (for example,
through `gh auth login` and `gh auth setup-git`). Install with:

```sh
GOPRIVATE=github.com/andrew-bierman/lambe go install github.com/andrew-bierman/lambe/cmd/lambe@latest
```

No GitHub releases are published yet. The commands above install from the
upstream repository; they do not use a local checkout.

Install `bd` and your chosen agent harness. Pi is the default; authenticate it
with your model provider before starting Lambe. For Codex or another ACP agent,
see [Agent harnesses](#agent-harnesses).

On a single machine, start from the project containing your Beads tracker:

```sh
cd ~/Code/my-beads-repo
lambe init
```

Init creates configuration and starter workflow files, preserving existing
workflows. Review the generated prompt, workspace hooks, responsibility trigger,
and completion gate before running:

```sh
lambe run
```

A gate must prove completion with your project's checks. Agents work in separate
directories; configure the workflow's `after_create` hook to clone the project
into each workspace before running those checks. The generated CI example
contains placeholders you must replace for your repository.

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

## Agent harnesses

`lambe run` starts and supervises the configured agent itself. You do not need
to start a separate Pi or Codex session. `lambe status` reads the tracker; it
does not start another agent.

Pi remains the default. To select Codex during setup:

```sh
npm install -g @agentclientprotocol/codex-acp
lambe init -harness codex
```

For an npm-installed Codex adapter, Lambe uses the Node executable beside
the adapter when available, then checks the required runtime API before
claiming work. An older shell Node cannot silently break the first prompt.
Native adapters and explicit custom commands retain their own runtime.

Authenticate Codex before running Lambe, then review the generated workflow
and completion gate. Existing configurations can set `harness: codex` instead
of running init again. Codex runs through the maintained
[Codex ACP adapter](https://github.com/agentclientprotocol/codex-acp); it uses
the bundled Codex executable unless `CODEX_PATH` selects another one.

Other ACP agents can use a known agent name or an explicit argv:

```yaml
harness: acp
acp:
  agent: opencode
  # Alternatively, command overrides agent. Arguments are never shell-split.
  # command: ["/absolute/path/to/my-agent", "--acp"]
```

Relative executable paths containing a slash resolve from the config file's
directory. Missing executables and unknown harness names fail startup before
claiming work. Backends advertising `session/resume` reopen without replaying old history;
`loadSession` is the fallback. A backend without either capability restarts
only when Lambe can supply the full task prompt, with a diagnostic. Lambe records the actual
backend thread before prompting. Pi reopens its saved workspace session.

## Watching agent work

Keep `lambe run` running, then open another terminal:

```sh
lambe tui                       # supervisor phase, attempts, tools, assistant output
lambe logs -follow              # follow recent agent output
lambe logs -issue ISSUE_ID      # inspect one issue
lambe logs -json                # shared read-only snapshot for scripts and other clients
```

Pass the same `-config FILE` to these commands when using a custom configuration.
The TUI exits with Ctrl-C or `q` followed by Enter. In Rex it reports an OSC
status badge in its own pane. Rex is optional; agent execution stays headless
and the views work in a normal terminal. A stopped daemon’s output is labeled
historical, using its native process identity rather than a reusable PID alone.

The private live snapshot keeps owned work visible and trims completed history
when it reaches 100 issues,
up to 4 KiB per issue. It contains tool names and completed assistant messages;
it is a progress view, not a full transcript archive. The tracker and restart
checkpoints remain the scheduling authority. Future clients can use the same
projection without starting another scheduler.

## Adapter seams

lambe is built around a small set of seams. What exists today:

| Seam | Default | Alternatives |
| --- | --- | --- |
| Tracker | Beads (hub, repo, or in-repo `.beads`) | Linear / GitHub / Jira via the Symphony adapter surface |
| Harness | Pi | Codex through ACP, configurable ACP agents |
| Store | Embedded Beads local store | — |
| Gates | `shell`, `gh:run`, `gh:pr` | — |
| Surfaces | terminal logs/TUI and JSON progress | MCP, optional Rex badges |

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
also depends on the harness: Pi reopens its workspace session; ACP reopens the
recorded backend session when the agent advertises resume or load support.
Switching harnesses preserves workspace files and starts a new conversation
at a full-task boundary; conversation content is not translated between agents. A replay can repeat an interrupted
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

## Recurring responsibilities

Recurring responsibilities are checked each tick. The materialization ledger
keeps one recorded root active per formula and permits another cycle after
that root closes and the refire interval expires. A corrupt ledger or a pour
whose result is unknown stops that formula from issuing duplicates. Inspect
the tracker before repairing `.beads/lambe-materialized.json`: stop the daemon,
then either set that formula’s `root_id` to the root that was actually created
and clear `pending` (keeping its valid `poured_at`), or delete only that formula’s
entry after confirming no root was created. Preserve other entries, then restart.
Restarting alone does not discard that safety record.

## License

MIT. The OpenAI Symphony specification (Apache-2.0) is attributed in a
NOTICE-style comment in source; lambe is not affiliated with OpenAI.
