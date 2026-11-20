# Dashboard dev smoke

This package is the dashboard's assembled, production-safe proof: the
real `lambe` binary — bearer-authed, descriptor-publishing — serves all
four operator views' data through the shared `internal/stateapi` client,
over isolated fixture state.

## What it proves

| Step | Assertion |
|---|---|
| Build | The committed sources build into the test's own temp dir (`cmd/lambe`, the fake ACP agent) |
| Seed | A disposable bd tracker: two configured workflow lanes, a dependency-blocked row, an ambiguous two-lane row, an off-lane row that must never be served, one dispatchable work item, one issue parked via the real `gate.Hub.NeedsHuman` escalation path |
| Discovery | `stateapi.Resolve` enumerates the live descriptor like any client (no lock-name math), 0600, with pid/version/token/started-at facts matching the started process |
| Auth posture | Missing and wrong tokens are refused 401 on every route — state, queue, history, issue, and the mutating refresh POST — with the uniform envelope |
| Queue view | Rows only for the configured lanes: the blocked row carries its blocker, the ambiguous row its reason, and the off-lane seed never appears on any observation |
| Attention view | The parked item with its recorded reason, the gate-output detail, and a machine-readable `since` |
| History + run detail | The seeded work item is dispatched, runs through the fake ACP agent, passes the green shell gate, lands newest-first in the bounded history with `stopped:` phase, and its run detail answers with workflow, settled state, thread, and a workspace under the smoke state dir |
| Lifecycle | Graceful shutdown removes the descriptor; a restart publishes a fresh one with a new per-start token and the finished run's history survives it |
| Hygiene | The bearer token never appears in the daemon's output |

## Production safety

Safety is by construction, not by assertion: the smoke builds its own
binaries, seeds its own tracker in a `t.TempDir()`, points the daemon at
its own `LAMBE_STATE_DIR`, and contains no `launchctl`, `bootout`, or
process-management call against anything it did not start. The harness
owns every process; the client owns none. The smoke never touches the
operator's configured daemons, the launchd service, or
`.dev/real-hub-smoke.sh`'s fixtures.

The authenticated refresh POST is exercised once by the Go harness to
prove the route answers; view clients never call it (it mutates — a full
supervisor cycle — so the app is GET-only by contract).

## Running

```sh
LAMBE_TEST_BD=1 mise exec go -- go test ./test/dashsmoke/
```

Skip (never fail) without `LAMBE_TEST_BD=1` and a `bd` binary on PATH,
and under `-short`. The assembled daemon + `.app` pass — launching the
built `apps/dashboard` app against this same smoke instance via the
`.dev` wrapper — is recorded in the dashboard plan's PR, not automated
here.
