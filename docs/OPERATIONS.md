# Operating lambe

How to run the lambe supervisor in production: setup, a disposable first task,
the sync model, liveness invariants, and what to do when lambe asks for a human.

## Setup

1. Clone (or locate) your Beads hub repository on the machine lambe will run
   on. lambe works with a hub clone, a standalone repo, or an in-repo `.beads`
   directory — the layout is a config setting.
2. Point config at that path (or run `lambe run` from inside the clone;
   defaults cover the common single-machine case).
3. Run the daemon under the OS service manager so it survives crashes and
   reboots — see the example units in [examples/](../examples/). This is the
   recommended setup, not an optional one: an unsupervised supervisor is the
   dominant documented failure mode.
4. Check the morning read: `lambe status`.

The binary enforces single-instance per machine with flock, so a stray second
`lambe run` exits instead of double-driving work.

## Run a disposable task

Try the full loop before pointing lambe at a tracker you care about. Install
[Beads](https://github.com/gastownhall/beads) (`bd`) and authenticate
[Pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) with
your model provider. Lambe starts Pi when eligible work appears. For Codex or
another ACP agent, see [Agent harnesses](../README.md#agent-harnesses).

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
  or stop the daemon first (`systemctl --user stop lambe` / `launchctl
  unload`), edit, push, then start it again. Never commit directly into the
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

Default pacing: tick 10s, heartbeat 150s. If you lengthen the tick, lengthen
the heartbeat proportionally.

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
