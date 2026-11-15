---
title: Unit-green seams shipped dead — wiring layers need assembled-path tests
date: 2026-11-15
category: best-practices
module: internal/daemon wiring (DefaultWire, harnessRunner, cmd/lambe)
problem_type: best_practice
component: testing_framework
severity: high
applies_when:
  - "Adding a seam (interface, hook, option, subcommand) whose production caller is assembled in a different package than the seam's implementation"
  - "A unit test exercises the seam through a fixture but no test drives the assembled binary or wiring function end-to-end"
tags: [wiring, integration-tests, seams, dead-code, review-findings]
---

# Unit-green seams shipped dead — wiring layers need assembled-path tests

## Context

The slice-one code review found five P1 defects in one category: production seams that were fully implemented, thoroughly unit-tested, and completely unreachable in the assembled product. `harness.Options.Persist` existed with tests — no production caller set it, so orphan recovery always found nothing. `gate.Runner.Pin` was implemented and unit-tested — never called, so revision pinning never engaged. `mcp.ServeStdio` existed with a full tool suite — no `lambe mcp` subcommand existed to serve it. `LAMBE_DEPTH` was read by a depth guard — no spawn path ever exported it. `ShouldDispatch`/tombstone checks were wired into tests only.

Every one of these passed the full CI suite: race-enabled, bd-integrated, lint-clean. The fault-injection suite itself — the definition of done — was green, because it too drove the seams through fixtures rather than through the assembled wiring.

## Guidance

A seam's unit tests prove the seam works. They cannot prove the seam is *reachable*. When the production caller lives in a wiring layer (`DefaultWire`, `cmd/lambe`, an assembly function), only a test that drives **through the wiring** — ideally the real binary or the real `Wire` function — detects the dead-seam class.

For lambe specifically:

- One end-to-end test per surface that constructs the daemon the way `run` does (`daemon.New` + `DefaultWire`) and exercises the loop with a fake agent — this would have caught dead Persist, undrained events, unclosed drivers, and the depth export in one test.
- A `cmd/lambe` invocation test per subcommand (spawn the built binary, assert behavior) — this would have caught the missing `mcp` subcommand.
- When a seam has exactly one production caller, that caller's absence is a compile-time question — prefer exporting the seam *from* the wiring package so an unused import fails the build, over optional hooks assembled elsewhere.

## Why This Matters

The dead-seam class is invisible to every check that inspects the seam's own package: coverage says the seam's code runs (in tests), lint says the code is clean, and the fixture-based suite stays green. Only two things catch it: assembled-path tests, or a reviewer who traces the wiring. Five of the twenty-eight findings in the slice-one review — including the discovery that crash recovery was inoperable — were this exact shape, and all five scored P1.

## When to Apply

- Adding an `Options.X` hook whose setter lives in another package.
- Adding a subcommand-adjacent server/surface (`ServeStdio`, HTTP handlers) whose entrypoint is a CLI wiring decision.
- Adding a guard that depends on an environment variable something else must set.
- Whenever a plan says "unit U later wires this" — the wiring unit's tests must drive the assembled path, not re-fixture the seam.

## Examples

The fix wave added exactly these tests: `test/faultsuite` gained `TestFault_SpawnDepthGuardHoldsInTheDaemon` (drives `ControlAdapter` directly), `cmd/lambe` tests invoke the real subcommands, and the persist wiring is asserted through `newAttemptPersister` with an injectable tracker opener. `TestDaemonCycleSerializesTickAndPoke` drives `Daemon.cycle` — the wiring-level mutex — not a re-implemented tick.

## Related

- docs/plans/2026-11-15-1256-feat-lambe-slice-one-plan.md (U7's "Wire is the single assembly point" design; the review findings #4, #6, #9, #11, #28)
- bd 1.3.4 facts recorded in docs/research/deep-beads.md (metadata large-int precision, "ready" pseudo-status) — related dead-assumption class caught the same way.
