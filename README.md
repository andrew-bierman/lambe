# lambe

<img src="docs/assets/lambe-mascot.png" alt="Lambe, a colorful friendly lambeosaurus" width="180">

[![CI](https://github.com/andrew-bierman/lambe/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/andrew-bierman/lambe/actions/workflows/ci.yml)
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

Install a checksummed release on macOS or Linux. Intel and ARM64 builds are
available. This command uses `curl`, `tar`, and either `shasum` or `sha256sum`.

```sh
(
set -eu
version=0.8.0
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'Unsupported OS: %s\n' "$(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac
archive="lambe_${version}_${os}_${arch}.tar.gz"
base="https://github.com/andrew-bierman/lambe/releases/download/v$version"
install_dir=$(mktemp -d)
trap 'rm -rf "$install_dir"' EXIT
if [ "${LAMBE_PRIVATE_RELEASE:-0}" = 1 ]; then
  gh release download "v$version" --repo andrew-bierman/lambe \
    --dir "$install_dir" --pattern "$archive" --pattern checksums.txt
else
  curl -fsSL "$base/$archive" -o "$install_dir/$archive"
  curl -fsSL "$base/checksums.txt" -o "$install_dir/checksums.txt"
fi
(
  cd "$install_dir"
  awk -v name="$archive" '$2 == name {print; found=1} END {if (!found) exit 1}' checksums.txt > selected-checksum.txt
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c selected-checksum.txt
  else
    sha256sum -c selected-checksum.txt
  fi
  tar -xzf "$archive"
)
mkdir -p "$HOME/.local/bin"
install -m 755 "$install_dir/lambe" "$HOME/.local/bin/lambe"
"$HOME/.local/bin/lambe" version
)
```

While the repository is private, sign in with `gh auth login` and export
`LAMBE_PRIVATE_RELEASE=1` before running the command. You can also download
an archive and its checksum from the [releases page](https://github.com/andrew-bierman/lambe/releases).

Release binaries report their version, commit and build date. Upgrading a binary
does not upgrade a running daemon: stop it in its owning terminal, then restart.
Add `~/.local/bin` to your `PATH` to use the `lambe` command by name.

### Explore the dashboard

Run `lambe tui -demo` to try the dashboard before setting up a tracker or agent.
Press `h` for history or `:` for commands. The demo runs entirely in memory.

### Run a disposable task

Install [Beads](https://github.com/gastownhall/beads) (`bd`) and authenticate
[Pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) with
your model provider. Lambe starts Pi when eligible work appears. For Codex or
another ACP agent, see [Agent harnesses](#agent-harnesses).

Run this in a fresh terminal. It creates a local tracker with one task, a
separate state directory, and a workspace hook that supplies the task's input.

```sh
mkdir -p "$HOME/Code"
demo=$(mktemp -d "$HOME/Code/lambe-demo.XXXXXX")
cd "$demo" || exit
git init -q
bd init --non-interactive --prefix demo --skip-agents --skip-hooks
lambe init -non-interactive -config "$PWD/lambe.yaml"

cat > WORKFLOW.md <<'EOF'
---
hooks:
  after_create: "printf 'hello, lambe!\\n' > input.txt"
---

Complete this task in the current workspace: {{ issue }}
EOF

cat >> .beads/formulas/keep-tests-green.formula.toml <<'EOF'

[gate]
type = "shell"
command = "test \"$(cat greeting.txt)\" = 'HELLO, LAMBE!'"
EOF

bd create 'Write an uppercase copy of input.txt to greeting.txt' --type task
export LAMBE_STATE_DIR="$PWD/state" LAMBE_WORKSPACE_ROOT="$PWD/workspaces"
lambe run -config "$PWD/lambe.yaml"
```

Lambe runs the gate in the agent's workspace and closes the task when the check
passes. Watch the daemon output, then press Ctrl-C and confirm the closure with
`bd list --status closed` in the demo directory.

### Set up a project

Run `lambe init` from the project containing your Beads tracker. It preserves
existing workflow files and records the absolute workflow path in the config.
Set `after_create` to populate each workspace with your project and add a
`[gate]` in `.beads/formulas/` that verifies completion. Every configured gate
applies to every task in the tracker. The starter trigger is disabled, and
existing eligible tasks can dispatch at startup. Set `labels` before running
against a shared tracker if Lambe should only claim a dedicated set of tasks.
For task-specific checks, shell gates receive the issue ID in `LAMBE_ISSUE_ID`.
Quote the variable as `"$LAMBE_ISSUE_ID"` when using it in a command.

To use a tracker elsewhere, point the config at its workflow and tracker.
Put its completion gate in the tracker's `.beads/formulas/` directory.

```yaml
# lambe.yaml
workflow_path: /absolute/path/to/project/WORKFLOW.md
trackers:
  - name: main
    type: hub
    path: /absolute/path/to/beads-hub
```

```sh
lambe run -config lambe.yaml
lambe status -config lambe.yaml
```

Only one daemon runs per configured tracker. `lambe tui` and `lambe logs -follow`
attach as viewers; quitting a viewer leaves the daemon running. To stop it,
press Ctrl-C in the terminal running `lambe run`.

The morning read:

```sh
lambe status
```

`lambe status` shows active and deferred work and items needing human attention.
Gate-rejection history is not yet available there. `lambe version` prints the
build version.

### Development installs

Keep development builds out of the production PATH. From this checkout:

```sh
go build -ldflags='-X main.version=dev-local' -o .dev/bin/lambe-dev ./cmd/lambe
LAMBE_STATE_DIR="$PWD/.dev/state" LAMBE_WORKSPACE_ROOT="$PWD/.dev/workspaces" \
  ./.dev/bin/lambe-dev run -config /absolute/path/to/dev-config.yaml
```

Use a disposable Beads tracker and an explicit development configuration. A
separate state directory still allows mutations to the configured tracker.

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

To keep coding workspaces separate from application state, configure a root
before starting work:

```yaml
workspace:
  root: /absolute/path/to/your/code/lambe-workspaces
```

Relative roots resolve from the configuration file’s directory.
`LAMBE_WORKSPACE_ROOT` overrides the file setting. Configure it before starting
work; if saved checkpoints use another root, startup names both paths and
refuses to resume until the previous root is restored or the workspace and
checkpoint are deliberately migrated.

## Watching agent work

Run `lambe tui -demo` to explore the dashboard with sample active, retrying,
passed, and failed runs. The demo runs in memory and needs no daemon or tracker.

Keep `lambe run` running, then open another terminal:

```sh
lambe tui                       # supervisor phase, attempts, tools, assistant output
lambe logs -follow              # follow recent agent output
lambe logs -issue ISSUE_ID      # inspect one issue
lambe logs -json                # shared read-only snapshot for scripts and other clients
```

Pass the same `-config FILE` to these commands when using a custom configuration.
In the command palette, **Queue an agent task** creates a Beads task in the
primary tracker with the workflow's configured labels. Enter a task title and
press Enter to queue it for the running supervisor's next poll. The demo adds
the task to its in-memory queue. Pick **Show activity chart** for a breakdown
of current runs and recent attempt outcomes.

The TUI exits with Ctrl-C or `q`. Keybindings:

- `j`/`k` or `↑`/`↓` — move through the run list
- `:` or `Ctrl-P` — search commands, inspect a run, or open a shell in its workspace or the Beads tracker
- `a` — show retries, deferred or interrupted runs, and failed gates
- `h` — switch between the run queue and recent attempt history
- `Tab` — switch focus between the run list and the detail pane
- `/` — filter runs. While filtering, `Esc` clears the filter.
- `u`/`d` or `Ctrl-U`/`Ctrl-D` — half-page up/down in the detail pane
- `g`/`G` — jump to top/bottom of the detail pane
- mouse wheel — scroll the pane under the pointer

On narrow terminals and Rex splits, the TUI shows one pane at a time and fits more runs per page. Press `Enter` to open
the selected run, `Esc` from its details to return to the run list, or `Tab`
to switch between panes.

In Rex the daemon reports an OSC status badge in its own pane. Rex is optional;
agent execution stays headless and the views work in a normal terminal. A
stopped daemon's output is labeled historical, using its native process identity
rather than a reusable PID alone.

For optional Rex command-palette shortcuts, add the actions in
[contrib/rex/lambe.lua](contrib/rex/lambe.lua) to your Rex `init.lua`. Run
`rex config check` and `rex config reload`, then search for **Lambe** in the
palette. **Open Dashboard** uses your default Lambe config. **Open Demo**
needs no tracker. On Rex servers and clients with custom prompts (control API
revision 17 or later), **Choose Dashboard…** offers both in a native picker.
The actions use `lambe` from the Rex terminal's PATH.

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
| `labels` | (none) | Opt-in work lane: claim only issues with every listed label |

On a shared tracker, set `labels` so lambe claims only issues explicitly
marked for its workflow (for example `lambe:babysit`). Other hub tasks stay
available to their own agents, and `lambe status` shows only lambe's own work.

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
