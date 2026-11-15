# lambe

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

Prebuilt static binaries for macOS and Linux are published via GoReleaser on
the releases page; a Homebrew tap is planned.

Zero-config, single machine — lambe defaults to the current directory's Beads
layout:

```sh
cd ~/code/my-beads-hub
lambe run
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

Key knobs and defaults:

| Setting | Default | Meaning |
| --- | --- | --- |
| `tick` | 10s | Supervisor loop interval |
| Attempt cap | 8 | Max attempts per work item before needs-human |
| Silence timeout | 10m | Agent produces no output for this long → reclaim |
| Turn timeout | 1h | Hard cap on a single agent turn |
| Heartbeat | 150s | Liveness interval written to the tracker |

## OS-level restart (production)

The recommended production setup is to let the operating system keep `lambe
run` alive, so the documented dominant failure — supervisor death — is covered
by the platform, not by lambe itself. Example service units ship in
[examples/](examples/):

- **macOS (launchd):** `KeepAlive` restarts the daemon on exit. Copy the
  plist, adjust paths, then `launchctl load`.
- **Linux (systemd user):** `Restart=on-failure`. Copy the unit, adjust paths,
  then `systemctl --user enable --now lambe`.

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

## License

MIT. The OpenAI Symphony specification (Apache-2.0) is attributed in a
NOTICE-style comment in source; lambe is not affiliated with OpenAI.
