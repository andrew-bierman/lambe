# Operating lambe

How to run the lambe supervisor in production: setup, a disposable first task,
the sync model, liveness invariants, and what to do when lambe asks for a human.

## Setup

1. Install `bd` ([Beads](https://github.com/gastownhall/beads)) and an agent
   harness (Pi by default; see [Agent harnesses](REFERENCE.md#agent-harnesses)).
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

`lambe doctor [--config FILE] [--json]` is read-only and exits 1 when any check
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

`--label NAME` installs under another launchd label / systemd unit name
(`dev.me.lambe-test`, `lambe-test.service`) with its own log file, for a second
or throwaway daemon; pass the same flag to `status` and `uninstall`, and
`--label NAME` to `lambe doctor`. If `LAMBE_STATE_DIR` is set when you
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

Every config key, its type, default, environment variable and meaning is in the
generated [configuration reference](reference/config.md). Unknown keys and
values of the wrong type are rejected at load time, so a typo fails loudly
instead of silently using a default. Two knobs deserve a note here:

- `supervisor.bd_timeout`: raise it (180s) on a slow embedded hub; see
  [slow embedded Dolt](solutions/slow-embedded-dolt.md).
- `supervisor.orphan_claim_ttl`: see [Orphaned claims](#orphaned-claims).

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
