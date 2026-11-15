# Changelog

All notable changes to lambe will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Daemon lifecycle: single-instance lock, run loop, and status snapshots,
  exposed via the `lambe` binary (`run`, `status`, `version`).
- Documentation and CI.

### In Flight

- Fault-injection test suite.
- MCP, ACP, Rex, and HTTP integration surfaces.
