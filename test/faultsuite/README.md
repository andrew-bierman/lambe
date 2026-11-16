# Fault-injection suite

This package is lambe's definition of done for surviving faults. Each
scenario is a deterministic, CI-runnable executable specification built on
the committed seams:

- `internal/tracker` — real local Beads trackers (`bd init` in throwaway
  temp directories, disposable actor identities), never fakes, for every
  scenario that touches responsibility state.
- `internal/symphony` — the real loop, workflow loader, workspaces, and
  retry queue.
- `internal/harness` + `internal/harness/pi` — the real driver supervising a
  re-exec fake agent (this test binary, selected by `LAMBE_FAULTSUITE_PI`).
- `internal/testchild` — the re-exec fake child for process-level kills
  (`LAMBE_TEST_CHILD`).

## Scenarios

| Test | Fault | Proves |
|---|---|---|
| `TestFault_DaemonKillMidReconcile` | Supervisor dies between turns | Zero responsibility loss; no double dispatch; pinned-id idempotency keeps the attempt journal single-recorded across restart |
| `TestFault_AgentKillMidTurn` | Agent process group SIGKILLed mid-turn | Persisted session binding survives; restart resumes the same session id; re-creates are no-ops |
| `TestFault_LimitLockout` | Provider limit lockout | Limit-shaped stream → `OutcomeLimitSuspected` (budget untouched) → defer-with-wake recorded on the tracker; wake fires; consecutive-defer escalation reaches needs-human |
| `TestFault_FakeDone` | Agent claims done, gate fails | RED consumes the attempt; rejection is hub-visible; retry guidance carries the gate output; cap 8 → needs-human; workspace retained |
| `TestFault_DuplicateTick` | Two supervisors race one issue | The claim is the CAS; exactly one runner invocation; the loser stands down |

After every fault, `assertContextParity` re-reads the world through the
tracker alone — the issue answers an id read with real state, the attempt
journal is readable, and the mutation journal is consumable. No fault may
leave state that only a live daemon could explain.

## Determinism

Timings are forced, never hoped for: retry delays are zeroed, the limit
wake horizon is 250 ms with a 15 s polling deadline, and both racing ticks
use agents that block mid-turn so the claim is provably held while the
loser stands down. Kill-path assertions check the actual signal
(`exitBySignal(..., SIGKILL)`) and reap (`cmd.Wait`).

## bd gating

Scenarios that need a real `bd` binary skip (never fail) when it is absent,
matching `internal/tracker`'s pattern. CI arms them with
`LAMBE_TEST_BD=1` and `bd` on `PATH`; the variable may also carry an
explicit binary path. `-short` skips them as well.

## Wiring points

**Assembled-daemon kill -9.** A true `kill -9` of the assembled supervisor
process cannot be simulated at package level, so
`TestFault_DaemonKill9_AssembledDaemon` is an env-gated integration hook:

- `LAMBE_FAULT_DAEMON_BIN` — path to the assembled `lambe` binary
  (unset → skip).
- `LAMBE_FAULT_DAEMON_DIR` — tracker directory to context-parity-check
  after the kill; also the daemon's working directory (defaults to a temp
  dir).

The hook starts the daemon (`run`) in its own process group, lets it take
on responsibility, SIGKILLs the group, and asserts the death signal and
tracker-only readability of everything it held. Once the daemon binary is
committed, wire this into CI behind the env var.

**Front filtering of operational records.** Against a real tracker, lambe's
own operational beads (attempt records, pinned rejections) are ordinary
open beads and appear on the ready dispatch front; the committed
`symphony.Loop` would dispatch them as work. The assembled daemon must
filter its own records off the front. The suite models that filter with
`quarantineRecords` (status `blocked`: off the front, still readable by
metadata queries) so scenario dispatch reads stay deterministic. This is a
finding for the daemon, not a property of the seams.

**Attempt-record metadata decode drops large pgids.** bd stores metadata
values as JSON numbers; the tracker's metadata decode stringifies with
`fmt.Sprintf("%v")`, which renders values ≥ 1e6 in scientific notation
(`2.900324e+06`), so `Attempts()`'s `Atoi` fails and the PGID reads back as
zero. Pids ≥ 1e6 are routine on long-lived Linux systems, so crash
recovery would silently lose the process-group binding. The tracker
package's own round-trip test masks this with small pgids (4242).
`TestFault_AgentKillMidTurn` documents the defect and reads the raw bead
metadata as the authoritative binding until `internal/tracker` fixes the
decode (e.g. integer-preserving stringification).

**Escalation policy.** The attempt cap (8) and the consecutive-deferral
escalation threshold (5, reset by a completed attempt) are enforced by the
daemon's gate layer. The suite carries a minimal local model of the same
contract (`deferEscalator`) plus the tracker writes the policy needs; the
daemon's own tests supersede it once wired.

## Running

```sh
LAMBE_TEST_BD=1 mise exec go -- go test -race ./test/faultsuite/
```
