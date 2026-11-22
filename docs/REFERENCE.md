# Reference

Configuration, harnesses, workflows, restart behavior, and the adapter seams.
For a first run, start with the [README](../README.md).

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

## Named workflows

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

Named workflows share the daemon's workspace root and use its top-level
`harness` unless a workflow sets its own. This sends repo tasks to Pi and
fleet babysitting to Codex from one daemon:

```yaml
harness: pi            # default for workflows that do not name one
workflows:
  - name: Repo tasks
    label: lambe:repo
    path: workflows/REPO.md
  - name: Fleet
    label: lambe:babysit
    path: workflows/FLEET.md
    harness: codex     # pi, codex, or acp (uses the top-level acp: block)
```

Startup checks every harness a workflow names before claiming work. A saved
conversation stays with the harness that created it.
Their instructions, hooks, and completion gates are saved in the run
checkpoint at claim time. Edits apply to new runs. When `gate_dir` is omitted,
the workflow uses the primary tracker's `.beads/formulas` directory.

## Watching agent work

Run `lambe tui -demo` to explore the dashboard with sample active, retrying,
passed, and failed runs. The demo runs in memory and needs no daemon or tracker.

Queue includes ready and blocked Beads tasks for the configured labels.
Attention surfaces blockers alongside failed or retrying runs. Open a
blocked task to see the reason and dependency IDs.

Keep `lambe run` running, then open another terminal:

```sh
lambe tui                       # supervisor phase, attempts, tools, assistant output
lambe logs -follow              # follow recent agent output
lambe logs -issue ISSUE_ID      # inspect one issue
lambe logs -json                # shared read-only snapshot for scripts and other clients
```

Pass the same `-config FILE` to these commands when using a custom configuration.

For scripts and tools that compose Lambe, two read-only commands print JSON
and share the exit codes of `lambe doctor` (0 ok, 1 a problem such as an
unreadable config or tracker, 2 usage):

```sh
lambe status -json [-config FILE]   # state API snapshot, read from the tracker
lambe state-dir [-json]             # resolved state dir and live descriptors
```

`lambe status -json` emits the `GET /api/v1/state` document
([`SnapshotResponse`](api/state-api.md#snapshotresponse--get-apiv1state)),
built by the same conversion, but it only reads the tracker and never contacts
a daemon, so it works with the state API off or no daemon running. It is a
documented subset: `runs` are the tracker's in-progress claims (`state`
`running`, no phase or event times) and `needs_human` comes from the tracker.
`gate_rejections`, `tick_at`, and per-run phases exist only in a running
daemon's projection, so they are empty or absent. The `daemon` block has no
`pid` (0) or `identity`. Read the API for those.

`lambe state-dir` prints the state directory (`LAMBE_STATE_DIR` or the
`~/.local/state/lambe` default, and which one applied) and the live state API
descriptors under `<state-dir>/live/`: path, instance, address, pid, start
time. It never prints the bearer token; read the descriptor file itself for
that, as the state API's client rules describe. See
[Discovery](api/state-api.md#the-state-dir-command).
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

### Local state API (opt-in)

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

### Rex integration

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

## Adapter seams

lambe is built around a small set of seams. What exists today:

| Seam | Default | Alternatives |
| --- | --- | --- |
| Tracker | Beads (hub, repo, or in-repo `.beads`) | Linear / GitHub / Jira via the Symphony adapter surface |
| Harness | Pi | Codex through ACP, configurable ACP agents |
| Store | Embedded Beads local store | — |
| Gates | `shell`, `gh:run`, `gh:pr` | — |
| Surfaces | terminal logs/TUI and JSON progress | MCP, optional Rex badges, opt-in local state API with the SwiftUI dashboard |

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

## Keeping the daemon running

The recommended production setup is to let the operating system keep `lambe
run` alive so a supervisor crash is handled by the platform, not by Lambe. Example service units ship in
[examples/](../examples/):

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
the process's kernel identity on macOS or Linux. Lambe sets no parent-death
signal, so an agent that outlives a SIGKILLed daemon keeps running until the
next daemon life reaps it; an agent that exits when its stdin closes leaves
nothing to reap. A foreign claim or unverifiable
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

## Development builds

Keep development builds out of the production PATH. From this checkout:

```sh
go build -ldflags='-X main.version=dev-local' -o .dev/bin/lambe-dev ./cmd/lambe
LAMBE_STATE_DIR="$PWD/.dev/state" LAMBE_WORKSPACE_ROOT="$PWD/.dev/workspaces" \
  ./.dev/bin/lambe-dev run -config /absolute/path/to/dev-config.yaml
```

Use a disposable Beads tracker and an explicit development configuration. A
separate state directory still allows mutations to the configured tracker.

