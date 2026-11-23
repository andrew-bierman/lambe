# Operating lambe

How to run the lambe supervisor in production: setup, a disposable first task,
the sync model, liveness invariants, and what to do when lambe asks for a human.

## Setup

1. Install `bd` ([Beads](https://github.com/gastownhall/beads)) and an agent
   harness (Pi by default; see [Agent harnesses](#agent-harnesses)).
2. From the project that holds your Beads tracker, run `lambe init`. It writes
   the config, a starter `WORKFLOW.md`, and a starter formula. Add a `[gate]`
   completion check to the formula: the daemon refuses to start without one.
3. Run `lambe doctor` and fix anything it marks `fail`.
4. Install the OS service so the daemon survives crashes and reboots:
   `lambe service install`. This is the recommended setup, not an optional
   one: an unsupervised supervisor is the dominant documented failure mode.
5. Check the morning read: `lambe status`.

The binary enforces single-instance per machine with flock, so a stray second
`lambe run` exits instead of double-driving work.

## Doctor: finding stall risks

`lambe doctor [-config FILE] [-json]` is read-only and exits 1 when any check
fails. It answers "could my agents be silently stalled right now?":

| Check | What it looks at | Typical fix |
| --- | --- | --- |
| `config` | the config loads and validates | `lambe init` |
| `workflow` | `WORKFLOW.md` exists and parses (front matter, env vars) | fix the file |
| `tracker`, `gates` | `.beads` exists; at least one completion gate is defined | `bd init`; add a `[gate]` |
| `bd` | `bd` on PATH and answers `--version` | install Beads |
| `harness` | the configured agent executable and runtime are present | install it, or set `harness` |
| `daemon` | state lock held and live snapshot belongs to a live process; an unfinished run with a dead daemon is a `fail` | `lambe service install` / `lambe run` |
| `identity` | the resolved machine and actor; warns on generic hostnames (`localhost`, `macbook`, ...) with a derived actor, and on a bd `node_id` set against a shared server | set `machine:` / `actor:`; unset `node_id` |
| `progress` | each active run's quiet time: warns past `supervisor.stall_after`, fails past `supervisor.silence_timeout` | read `lambe logs -issue ID`; restart a wedged daemon |
| `loop` | age of the daemon's loop heartbeat (`tick_at`) while its PID is alive; warns when a cycle is slow, fails when the loop is wedged | check the daemon log; restart the daemon |
| `service` | launchd/systemd unit installed, running, and pointing at this config | `lambe service install [--force]` |
| `stateapi` | with `stateapi.enable`, the descriptor's loopback API answers with its token | restart the daemon |
| `rex` | Rex detected (optional) | none |

`daemon`, `progress` and `loop` read the daemon's live snapshot. `progress`
measures the latest agent activity or phase change; `loop` measures the loop
heartbeat the daemon stamps at the start and end of every cycle, so a live PID
with a stuck loop is caught. A snapshot from an older daemon has no heartbeat
and `loop` skips. The tracker lease heartbeat itself is not checked.

## Service management

`lambe service install | status | uninstall` generates and manages a launchd
user agent (macOS, `~/Library/LaunchAgents/com.lambe.daemon.plist`) or a
systemd user unit (Linux, `~/.config/systemd/user/lambe.service`). The
generated unit differs from a copy-pasted example in ways that matter for
stalls: it bakes in a PATH that finds `bd` and the agent (service managers
start with a minimal one), gives the daemon 45s to run its kill ladder before
the OS kills it, and on macOS creates the log directory
(`~/Library/Logs/lambe/daemon.log`), without which launchd fails the job
silently.

```sh
lambe service install --dry-run      # print the unit and where it would go
lambe service install                # write it and load it
lambe service install --force        # replace a unit that differs (reload)
lambe service status                 # unit file + launchd/systemd state
lambe service uninstall
```

`-label NAME` installs under another launchd label / systemd unit name
(`dev.me.lambe-test`, `lambe-test.service`) with its own log file, for a second
or throwaway daemon; pass the same flag to `status` and `uninstall`, and
`-service-label NAME` to `lambe doctor`. If `LAMBE_STATE_DIR` is set when you
install, the unit carries it. `uninstall` waits for launchd to finish stopping
the daemon before it returns.

`--root DIR` writes the unit under DIR instead of `$HOME` and never calls
launchctl/systemctl, for inspection and tests; `--no-start` writes without
loading; `--manager launchd|systemd` overrides OS detection. Restarting the
daemon after a config change: `lambe service install --force`. On Linux, also
run `loginctl enable-linger "$USER"` to keep the unit running after logout.

## Run a disposable task

Try the full loop before pointing lambe at a tracker you care about. Install
[Beads](https://github.com/gastownhall/beads) (`bd`) and authenticate
[Pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) with
your model provider. Lambe starts Pi when eligible work appears. For Codex or
another ACP agent, see [Agent harnesses](REFERENCE.md#agent-harnesses).

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

## The sync model

lambe is a single writer to the tracker. Each tick it:

1. pulls the machine's local clone,
2. writes claims, status transitions, heartbeats, and gate results,
3. pushes.

Consequences for humans:

- Owner edits (changing priorities, adding items) should flow through the same
  clone the daemon uses — push to the shared remote and let the daemon pull —
  or stop the daemon first (`systemctl --user stop lambe.service` / `launchctl
  bootout gui/$(id -u)/com.lambe.daemon`), edit, push, then start it again. Never commit directly into the
  daemon's working clone while it is running.
- Everything the daemon writes is fenced by Beads claims, so even if a second
  supervisor points at the same tracker, work is not double-assigned.

## Heartbeat, TTL, and reclaim

The liveness machinery rests on three invariants. Keep them true in any config
change:

- TTL must be greater than the sync interval — the heartbeat must be able to
  reach the remote tracker before the old one expires.
- Grace must be greater than the sync interval — a slow push must not read as
  a dead agent.
- Reclaim fires at roughly 2× TTL — an unclaimed item waits one full TTL to
  expire plus headroom before another agent picks it up.

Reclaim is OFF unless `supervisor.auto_reclaim: true` is set; it is meant for
several hosts sharing one Dolt server. See [MULTI-MACHINE.md](MULTI-MACHINE.md)
for when to enable it and the proof of the ownership fences.

Default pacing: tick 10s, heartbeat 150s. If you lengthen the tick, lengthen
the heartbeat proportionally.

The daemon heartbeats every claim it holds each `heartbeat_interval`. Crashed-host claims are reclaimed by an external `bd reclaim --older-than 10m`; the daemon does not run a reaper.

## Orphaned claims

A claim this actor holds that recovery cannot resume (no durable continuation
state, typically left by an older daemon) is kept and heartbeated, because its
agent might still be alive. Left alone, a large pile of them used to flood the
tracker with `bd heartbeat` calls. Now:

- The heartbeat loop beats live work first, orphans second, and never runs
  more than 4 beats at once. A round that exhausts its 30s budget means the
  tracker is saturated, so the next rounds are skipped with backoff, and each
  round starts after a small random delay.
- After `supervisor.orphan_claim_ttl` (default 1h, counted from when the claim
  first blocked and persisted across restarts) the daemon stops heartbeating
  the claim, provided its recorded agent is provably gone. The lease then
  expires on its own (or `auto_reclaim` / `bd reclaim` picks it up). A claim
  whose agent may still be alive keeps heartbeating.
- Startup recovery inspects claims 4 at a time, each time-boxed, and a pass
  is capped at 15s per tick, so new issues dispatch while old claims are still
  being checked. A blocked claim is re-checked every 5 minutes.
- Checkpoints of closed issues are dropped.

`lambe status` shows an "orphaned claims" section (issue, age, owner,
workspace present, reason), and `lambe doctor` warns when any exist.

```sh
lambe claims list                    # the same table, from checkpoints
lambe claims release ISSUE           # release one orphan back to the open pool
lambe claims release --force ISSUE   # skip the workspace and agent checks
```

`release` is the only way lambe releases such a claim. Without `--force` it
refuses when the claim is not a recorded orphan, its workspace still exists, or
its recorded agent is not provably dead. The release is guarded by this
actor's identity, so another host's claim is never touched.

## Configuration

Config file keys, each overridable by `LAMBE_<SECTION>_<KEY>` (first underscore
is the section separator):

| Key | Default | Meaning |
| --- | --- | --- |
| `machine`, `actor` | hostname first label, `lambe@<machine>` | Dispatch affinity name and the tracker identity on claims. Must be distinct per host sharing a tracker; `lambe doctor` prints the resolved actor and warns on generic hostnames. |
| `supervisor.tick_seconds` | 10 | Loop cadence. |
| `supervisor.heartbeat_interval` | 150s | Lease extension cadence. |
| `supervisor.orphan_claim_ttl` | 1h | How long a recovered claim with no continuation state may stay blocked before the daemon stops heartbeating it so its lease can expire. `0` heartbeats such claims forever. The claim is never released automatically; see [Orphaned claims](#orphaned-claims). |
| `supervisor.bd_timeout` | 60s | Bound on one `bd` read or heartbeat subprocess run. The clock starts when the process starts, never while it waits in the queue. Raise it (180s) on a slow embedded hub; see [slow embedded Dolt](solutions/slow-embedded-dolt.md). |
| `supervisor.bd_write_timeout` | 0s (= `bd_timeout`) | Bound on one `bd` write run (claim, attempt record, close). |
| `supervisor.bd_max_concurrency` | 3 | Most `bd` subprocesses running at once. Writes jump the queue ahead of reads, and reads ahead of heartbeats. |
| `supervisor.bd_queue_wait` | 10m | How long a call may wait for a free slot before it fails. |
| `supervisor.silence_timeout` | 10m | No agent output for this long: the watchdog probes, then kills. |
| `supervisor.turn_timeout` | 1h | Hard cap on one agent turn. |
| `supervisor.stall_after` | 5m | **The one stall threshold.** A running run with no agent activity or supervisor progress for this long is `stalled` in the state API (`stalled`, `stall_after_seconds`) and flagged by `lambe doctor`. Observation only; the watchdog still acts at `silence_timeout`. |
| `supervisor.probe_window` | driver default (30s) | Pi harness only: how long the watchdog waits for any response after its steer probe before killing. |
| `gate.check_timeout` | `silence_timeout / 2`, min 1m | Bound on each external gate check (tests, CI polls), independent of the silence timeout. Raise it for slow suites. Per-gate overrides do not exist. |

## Stalls and quiet tool calls

`supervisor.silence_timeout` (default 10m) is how long an agent may emit
nothing. A silent agent is probed (Pi) or killed (ACP) and the attempt is
retried after the failure backoff (10s, doubling). A tool call that is still
running does not count as silence, so a long build or test run is never
mistaken for a stall; `supervisor.turn_timeout` (default 1h) still bounds the
whole turn, tool calls included.

## Needs-human

A work item lands in needs-human when:

- it hits the attempt cap (default 8) without passing its gates, or
- its gates fail in a way lambe knows is not transient (e.g. a `gh:pr` gate
  reporting a review block).

Resolve it by reading `lambe status`, fixing the underlying cause (often in
the repo or the tracker item itself), then resetting the item's attempts in
the tracker. lambe picks it up on the next tick; there is no resume command to
run.

## Dashboard (read-only)

The native macOS dashboard (`apps/dashboard/`) is another read path: it polls
the daemon's loopback HTTP state API and shows the same Queue, Attention,
History, and run detail views, changing nothing. It only sees daemons running
with `stateapi.enable: true`; daemon start, stop, and restart stay with
`lambe run` or your launchd/systemd unit, and needs-human items are still
resolved in the tracker with `bd`. Setup, demo mode, and the API contract:
[apps/dashboard/README.md](../apps/dashboard/README.md).

## Log lines to watch

Structured logs go to stdout; the service manager captures them
(`journalctl --user -u lambe -f` / `log stream --predicate
'process == "lambe"'`).

- `claim` / `release` — work items entering and leaving an agent's ownership
- `gate` with result — every verified-completion check and its outcome
- `reclaim` — a dead agent's work being picked up; frequent occurrences mean
  agents are dying, not that lambe is flaky
- `needs_human` — an item exhausted its attempts or hit a hard gate failure
- `sync error` — pull→write→push failed; persistent failures usually mean a
  remote conflict (see the sync model) or lost credentials

## ghx (optional)

At multi-agent scale, many concurrent agents hammering the GitHub API will
rate-limit. ghx is an optional `gh` caching proxy: install it, point the
`gh`-family gates at the proxy via config, and agents share a warm cache. This
is a config-level adoption — the gate seam does not change.

---

# Reference

Reference material for operators: harnesses, workflows, the dashboards, tuning, and restart semantics.

### Agent harnesses

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

### Named workflows

Configure multiple workflows in one daemon to use a shared Beads tracker.
Give each workflow a distinct label and a `WORKFLOW.md` file. Use `gate_dir`
to select a separate directory of TOML completion gates for each workflow.

```yaml
workflows:
  - name: Repo tasks
    label: lambe:repo
    path: workflows/REPO.md
    gate_dir: workflows/repo-gates
  - name: Fleet
    label: lambe:babysit
    path: workflows/FLEET.md
    gate_dir: workflows/fleet-gates
```

Paths are relative to the config file. With named workflows, put routing labels
on the workflows instead of configuring top-level `labels`. An issue with zero
or multiple matching routes is skipped. The command palette has a
**Queue task** action for each workflow, and applies the correct label when
you submit. For an existing ready issue, preview the route and then queue it:

```sh
lambe enqueue -workflow 'Repo tasks' ISSUE_ID
lambe enqueue -workflow 'Repo tasks' -apply ISSUE_ID
```

You can also apply exactly one workflow label in Beads or a companion
such as B9S.

Named workflows share the daemon's configured harness and workspace root.
Their instructions, hooks, and completion gates are saved in the run
checkpoint at claim time. Edits apply to new runs. When `gate_dir` is omitted,
the workflow uses the primary tracker's `.beads/formulas` directory.

Use B9S or another Beads board to plan work and manage dependencies. Use Lambe
to verify execution and inspect run attempts. The inspector shows the workflow,
workspace, and native agent thread ID so you can find the same session in a
session browser such as Show Agent.

### Watching agent work

Run `lambe tui -demo` to explore the dashboard with sample active, retrying,
passed, and failed runs. The demo runs in memory and needs no daemon or tracker.

Queue includes ready and blocked Beads tasks for the configured labels.
Attention surfaces blockers alongside failed or retrying runs. Open a
blocked task to see the reason and dependency IDs. If Show Agent is
installed, use `:` → **Show agent session details** on a run with a
recorded thread to look up that exact session on this machine.

[See the History screenshot](assets/history.png). Refresh both
screenshots from the repository root with
`go build -o .dev/bin/lambe-dev ./cmd/lambe` and
`vhs scripts/dashboard.tape`.

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
- `1` / `2` / `3` — open Queue, Attention, or History directly
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

#### Local state API (opt-in)

A daemon can serve its supervised state as JSON on loopback for apps that
render it — the bundled SwiftUI dashboard (`apps/dashboard`) is the first
client. The surface is off until you ask for it, and binding is loopback-only:

```yaml
# lambe.yaml
stateapi:
  enable: true       # off by default
  addr: 127.0.0.1:0  # ephemeral loopback port; loopback IP literals only
```

When enabled, every route — the refresh POST included — requires a per-start
bearer token, and the daemon publishes a per-instance descriptor file
(`<state-dir>/live/stateapi-<instance>.json`, mode 0600) carrying the bound
address, the token, and the daemon identity. Clients enumerate those
descriptors; nothing needs to be parsed from configuration or logs.

- Routes: `GET /api/v1/state`, `GET /api/v1/queue`, `GET /api/v1/history`,
  `GET /api/v1/{issue}`, `POST /api/v1/refresh`.
- `POST /api/v1/refresh` is an operator action, not an advisory refresh: it
  triggers a full supervision cycle and can write to the hub. Dashboards and
  other read-only clients must stay GET-only.
- The versioned wire contract — routes, field semantics, freshness,
  evolution policy, error envelopes, discovery — is documented in
  [docs/api/state-api.md](api/state-api.md) and pinned by the committed
  fixtures in [testdata/contract/v1/](../testdata/contract/v1/).

#### Rex integration

Open the live dashboard or interactive demo from the Rex command palette.
On Rex clients with custom UI, **Lambe: Open Run…** searches live and
archived runs and opens the selected output in its own tab.
A terminal-hosted daemon can report its state through a Rex program status
badge. [Install the actions and check compatibility](../contrib/rex/README.md).

The private live snapshot keeps owned work visible and trims completed history
when it reaches 100 issues,
up to 4 KiB per issue. It contains tool names and completed assistant messages;
it is a progress view, not a full transcript archive. The tracker and restart
checkpoints remain the scheduling authority. Future clients can use the same
projection without starting another scheduler.

#### Native dashboard (macOS)

`apps/dashboard/` holds a native macOS SwiftUI app that attaches to a running
daemon over the daemon's authenticated local HTTP state API and renders the
same views the TUI shows — Queue, Attention, History, and run detail. The app
is read-only: needs-human items are resolved in Beads or your tracker, not in
the app.

Open [apps/dashboard/LambeDash.xcodeproj](../apps/dashboard/LambeDash.xcodeproj)
and press Cmd+R, or run `mise dashboard:build` and `mise dashboard:test`.
The toolbar's Live/Demo picker shows the four views with sample data before
any daemon is set up.

The app reads the daemon's optional local state API, which is off by default:
set `stateapi.enable: true` (or `LAMBE_STATEAPI_ENABLE=true`) to turn it on.
Discovery, instance selection, and demo mode are covered in
[apps/dashboard/README.md](../apps/dashboard/README.md), and the wire contract
is specified in [docs/api/state-api.md](api/state-api.md). Attaching
never restarts a daemon — production start, stop, and restart stay with
`lambe run` and the service units in
[OS-level restart](#os-level-restart-production).

### Adapter seams

lambe is built around a small set of seams. What exists today:

| Seam | Default | Alternatives |
| --- | --- | --- |
| Tracker | Beads (hub, repo, or in-repo `.beads`) | Linear / GitHub / Jira via the Symphony adapter surface |
| Harness | Pi | Codex through ACP, configurable ACP agents |
| Store | Embedded Beads local store | — |
| Gates | `shell`, `gh:run`, `gh:pr` | — |
| Surfaces | terminal logs/TUI and JSON progress | MCP, optional Rex badges, opt-in local state API with the SwiftUI dashboard |

### Configuration

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

### Restart and recovery semantics

The OS keeps `lambe run` alive (see [Service management](#service-management));
supervisor death is covered by the platform, not by lambe itself. The example
units in [examples/services/](../examples/services/) show what
`lambe service install` generates.

Lambe keeps private session checkpoints under its state directory. Restarting
with the same state directory, tracker, and actor restores the pinned workflow,
attempt, turn, thread, and retry deadline. Interrupted turns replay within the
same attempt; pending gate checks reuse the settled output. Workspace files
remain available. Keep the state directory across upgrades.

Before replacing a surviving agent, recovery verifies the tracker claim and
the process's kernel identity on macOS or Linux. Lambe sets no parent-death
signal, so an agent that outlives a SIGKILLed daemon keeps running until the
next daemon life reaps it; an agent that exits when its stdin closes leaves
nothing to reap. A foreign claim or unverifiable process is left alone. Recovery errors are logged and retain the workspace;
transient tracker failures are retried on subsequent ticks. Claims from older
versions without checkpoints require inspection before releasing them for a
fresh run. Do not release one while its old agent is still working.

Checkpoints preserve Lambe's continuation state. Backend conversation resumption
also depends on the harness: Pi reopens its workspace session; ACP reopens the
recorded backend session when the agent advertises resume or load support.
Switching harnesses preserves workspace files and starts a new conversation
at a full-task boundary; conversation content is not translated between agents. A replay can repeat an interrupted
tool operation, so tasks with external side effects still need idempotent steps.

### Recurring responsibilities

Recurring responsibilities are checked each tick. The materialization ledger
keeps one recorded root active per formula and permits another cycle after
that root closes and the refire interval expires. A corrupt ledger or a pour
whose result is unknown stops that formula from issuing duplicates. Inspect
the tracker before repairing `.beads/lambe-materialized.json`: stop the daemon,
then either set that formula’s `root_id` to the root that was actually created
and clear `pending` (keeping its valid `poured_at`), or delete only that formula’s
entry after confirming no root was created. Preserve other entries, then restart.
Restarting alone does not discard that safety record.

### Symphony conformance and divergence

lambe implements the Symphony specification as pinned to `8001b52` and stays
compatible with other Symphony orchestrators. Where lambe goes further, it does
so as a durable superset: state survives restarts, and agent ownership is
fenced through Beads claims so two supervisors can share a tracker without
double-assigning work.

### Roadmap

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
- [x] HTTP state API — opt-in, bearer-guarded, contract documented and pinned with fixtures (`docs/api/state-api.md`)

Next:

- [ ] Multi-machine affinity and failover
- [ ] Shared Dolt-server mode
- [ ] More harness drivers (Codex, Claude Code, OpenCode)
- [ ] `bd`-as-library embedding
- [ ] Limit-classifier tuning on real traffic

Later / exploring:

- [ ] Gas-City interop
- [ ] `ghx` caching proxy at multi-agent scale
