# Changelog

All notable changes to lambe will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `lambe version --json` and a `state API` line in `lambe version`; development builds report `vX.Y.Z-dev+<shortsha>`.
- Opt-in `supervisor.auto_reclaim` / `reclaim_grace` (default off) for multi-host deployments sharing one Dolt server, with `docs/MULTI-MACHINE.md` and isolated multi-host tests.
- Configurable machine/actor identity, `stall_after`, `probe_window`, and `gate.check_timeout`.
- `lambe doctor` preflight checks and `lambe service install|status|uninstall` for launchd/systemd units.

### Changed

- Expand `docs/OPERATIONS.md` with the harness, workflow, dashboard, tuning, and restart-semantics reference.
- Redesign the Mac dashboard as a tabbed table with a preview pane.
- Releases now use a single `vX.Y.Z` tag for the CLI and the Mac app; see `docs/RELEASING.md`.
- `scripts/install.sh` installs the latest release by default (checksum still verified) and prints next steps.

## [0.9.2] - 2026-11-20

### Added

- Package the Mac dashboard as a universal (arm64 + x86_64) app ZIP with a checksum.
- Explain Lambe and Rex host support in the docs.

### Changed

- Surface stalled runs in the dashboard and polish narrow layouts, tabs, and run rows.

### Fixed

- Bound stalled Pi runs and Beads subprocesses so a hung child no longer blocks the loop.
- Bundle the mascot icon in the macOS app.

## [0.9.1] - 2026-11-20

### Added

- Open Lambe run output from the Rex palette.

## [0.9.0] - 2026-11-20

### Added

- Add the read-only native macOS dashboard under `apps/dashboard`. It attaches to a daemon through the opt-in (`stateapi.enable`, default off), bearer-authenticated local state API and shows queue, attention, history, and run detail.
- Serve the local HTTP state API on loopback with per-instance endpoint descriptors, queue and history reads, and daemon identity plus per-section timestamps in every response.
- Document the versioned wire contract in `docs/api/state-api.md` and pin it with Go-generated fixtures in `testdata/contract/v1/`.

### Changed

- Mark `POST /api/v1/refresh` as a mutating operator action, so read-only clients stay GET-only.
- Simplify installation docs and point them at the dashboard release.

## [0.8.4] - 2026-11-19

### Added

- Show Beads readiness and agent session details in the dashboard.

## [0.8.3] - 2026-11-19

### Changed

- Show compact run rows, stack the inspector, and give run titles more space in the dashboard.

## [0.8.2] - 2026-11-19

### Added

- Route Beads tasks to named workflows.

### Changed

- Make dashboard views visible in the header.

## [0.8.1] - 2026-11-18

### Changed

- Polish release installation and CLI help; the install and first-run walkthrough now work from a fresh setup.

### Fixed

- Batch Beads attempt lookups during hub recovery.

## [0.8.0] - 2026-11-18

### Added

- Add an Attention view for retries and failed gates, with the latest rejection visible in the inspector.
- Add optional Rex actions for opening the dashboard and demo.
- Expose the current issue ID to shell gates.

### Changed

- Make agent output easier to scan with tool-result markers and fewer repeated tool-start lines.

### Fixed

- Keep dashboard controls visible on narrow terminals and keep filtered selection and follow mode stable across refreshes and resizes.
- Bound optional Rex detection and retry timed-out process-binding reads and writes once.

## [0.1.0] - 2026-11-16

- Publish tagged release archives and checksums; keep local development binaries and state separate from production.
- Explain how to view an existing daemon and that quitting the TUI does not stop it.

- Resolve mise-managed Codex adapters before selecting and checking their Node runtime.

- Complete gate bookkeeping when Beads omits closed records from default reads, and reject changed workspace roots before restoring saved work.

- Allow a separate `workspace.root` so trial and production instances can keep their coding directories isolated.

- Bare CLI commands load the configuration written by init and share the configured tracker’s live-view namespace.

- Show live supervisor phases and agent output with `lambe tui` and `lambe logs`, and use the current Rex status protocol and CLI.
- Select and validate the npm Codex adapter’s Node runtime before claiming work.
- Pin generated workflow paths, provide a native Beads starter with automatic triggering disabled during setup, and show upstream module versions in `lambe version`.
- Re-arm recurring responsibilities after their recorded root closes, decode native Beads status arrays, and persist pour intent to prevent duplicate work after ambiguous failures.


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

[Unreleased]: https://github.com/andrew-bierman/lambe/compare/v0.9.2...HEAD
[0.9.2]: https://github.com/andrew-bierman/lambe/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/andrew-bierman/lambe/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/andrew-bierman/lambe/compare/v0.8.4...v0.9.0
[0.8.4]: https://github.com/andrew-bierman/lambe/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/andrew-bierman/lambe/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/andrew-bierman/lambe/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/andrew-bierman/lambe/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/andrew-bierman/lambe/compare/v0.7.0...v0.8.0
[0.1.0]: https://github.com/andrew-bierman/lambe/releases/tag/v0.1.0
