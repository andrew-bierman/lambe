---
title: Silent stalls the watchdog should have caught
date: 2026-11-20
category: runtime-errors
module: internal/harness/pi, internal/daemon (hub sync), internal/gate (writeback)
problem_type: runtime_error
severity: high
tags: [watchdog, stalls, max-run, wait-delay, tick-loop, pi]
---

# Silent stalls the watchdog should have caught

Two defects let work stop without any watchdog, log line, or status change
noticing. Both are fixed; each fix has a test that fails on the old code.

## 1. Pi lost its per-turn deadline after the steer probe

**Symptom.** An agent that kept emitting records but never settled ran far
past `supervisor.turn_timeout` and was never killed.

**Cause.** `Driver.onExpiry` handles the first silence expiry by sending a
steer probe and re-arming the watchdog for one probe window. The re-arm was
`proc.NewWatchdog(0, ProbeWindow, ...)`: a max-run of zero, which means "no
deadline". Any record after the probe (the probe's own acknowledgement counts)
resets the silence timer, so a chatty agent could keep the run alive forever.

**Fix.** The driver records when the run was armed (`runStart`) and re-arms
with `remainingRun()`, the unspent part of `MaxRun`, so the original deadline
survives the probe (`internal/harness/pi/driver.go`).

**Evidence.** `TestMaxRunSurvivesSteerProbe` in
`internal/harness/pi/pi_test.go` (uses the fake-agent addition in
`fake_test.go`): the agent answers the probe with endless chatter and must be
killed at `MaxRun`. With the fix reverted it fails (`outcome = failed after 6s, want
watchdog_killed at MaxRun`); with it, the run ends with a retryable
watchdog-kill outcome at `MaxRun`.

**Left as is.** After a probe the silence window stays at `ProbeWindow`
(30s by default; `supervisor.probe_window` tunes it) even when the agent
answers with real events, so a genuinely slow agent can be killed 30s after
its probe response instead of getting a full `silence_timeout` again. Fixing
it needs the watchdog liveness hook to tell event records from the probe's own
acknowledgement, and `ClientOptions.OnRecord` is a bare `func()` that sees
neither. The follow-up is to pass the record type to the hook and restore
`SilenceAfter` only on non-response records. Until then the effect is a
slightly early kill that retries, never a hang.

## 2. A descendant holding stdout froze the whole tick cycle

**Symptom.** The daemon was alive, its lock held, its PID healthy, and
nothing moved: no dispatch, no heartbeats from the loop, no flush. Status
showed the last good state indefinitely.

**Cause.** `bd dolt pull` / `push` and the gate's `bd` write-backs ran under
`exec.CommandContext` with a deadline but no `cmd.WaitDelay`. When `bd` spawns
a descendant that inherits its stdout (a `dolt sql-server` does), the context
kill reaches `bd`, but `Wait` keeps blocking on the pipe the descendant still
holds. The call never returned, so the cycle (which holds `cycleMu`) never
finished, and every later tick queued behind it. `ShellTransport.run` already
had `WaitDelay`; `BdSyncer.run` and `BdHub.run` did not.

**Fix.** `cmd.WaitDelay = time.Second` on both (`internal/daemon/sync.go`,
`internal/gate/writeback.go`), matching the transport.

**Evidence.**
- `TestBdSyncerReturnsWhenADescendantHoldsThePipe`
  (`internal/daemon/sync_hang_test.go`): a fake `bd` backgrounds a `sleep`
  that inherits stdout; the pull must return once its context expires.
- `TestBdHubReturnsWhenADescendantHoldsThePipe`
  (`internal/gate/hub_hang_test.go`): same for a gate write-back.
  With the fix reverted the hub test fails (`BdHub.Comment blocked 5s past its
  context`) and the syncer test fails the same way.

## Detection that no longer depends on the fix

The daemon now stamps a loop heartbeat (`tick_at`) at the start and end of
every cycle. `lambe doctor` (`loop` check) and `GET /api/v1/state` (`tick_at`)
expose it, so a wedged loop behind a live PID is visible even when the cause
is a third bug of the same class.
