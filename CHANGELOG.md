# Changelog

All notable changes to lambe will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Restart recovery restores owned attempts, turns, retry deadlines, pinned workflows,
  and gate output from private atomic checkpoints. Surviving agents are replaced
  only after verifying their boot and process identity; unprompted persistence
  failures preserve the attempt, and internal records cannot become agent tasks.

- Polling and reconciliation continue while independent agent workers run, with a workflow-configured concurrency limit and bounded worker shutdown. Standard `agent.max_turns` and `agent.max_concurrent_agents` settings are validated.

- Agent turns now use their configured turn and silence limits instead of inheriting the daemon polling deadline. Formula and gate timeouts remain bounded; timed-out triggers do not create work and timed-out gates retry without consuming an agent attempt.

### Development

- Setup tests isolate platform-specific configuration directories on macOS and
  Linux, and duplicated formula test statements no longer prevent compilation.

### Added

- Codex through the maintained ACP adapter, configurable ACP agent commands,
  harness selection during init, and startup checks for missing executables.
  ACP restores advertised saved sessions and records native backend thread IDs
  before prompting; replayed history is excluded from new turn output.

- Project scaffold: configuration loading with precedence and env flattening
  (`internal/config`), structured logging (`internal/logging`), and mise-pinned
  Go toolchain.
- Beads tracker adapter supporting hub, repo, and in-repo layouts
  (`internal/tracker`).
- Process supervision primitives with a test-only child helper
  (`internal/proc`, `internal/testchild`).
- Symphony core supervisor loop encoding the Symphony specification contract
  (`internal/symphony`, `internal/daemon`).
- Pi agent harness driver (`internal/harness`).
- Verified-completion gates: shell, `gh:run`, and `gh:pr` (`internal/gate`).
- Fault-injection test suite covering kill-mid-reconcile, agent-kill
  resume, limit lockout, and fake-done scenarios.
- MCP server surface (`internal/mcp`): `lambe_spawn`/`status`/`output`/
  `cancel`/`list` over the official go-sdk, with a recursion depth guard.
- ACP harness driver (`internal/harness`): 40+ agents via
  coder/acp-go-sdk, stop-response completion, permission auto-deny.
- Rex terminal surface (`internal/surface/rex`): OSC 7501 status badges
  and labeled sessions with graceful degradation when Rex is absent.
- HTTP state surface (`internal/httpstate`): Symphony §13.7-shaped
  `/api/v1/state`, issue, and refresh endpoints over a typed StateSource.
- Daemon lifecycle: single-instance lock, run loop, and status snapshots,
  exposed via the `lambe` binary (`run`, `status`, `version`).
- Documentation and CI.
