# Changelog

All notable changes to lambe will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Agent turns now use their configured turn and silence limits instead of inheriting the daemon polling deadline. Formula and gate timeouts remain bounded; timed-out triggers do not create work and timed-out gates retry without consuming an agent attempt.

### Added

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
