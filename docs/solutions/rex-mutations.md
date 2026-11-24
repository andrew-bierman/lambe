# Proposal: mutating Rex palette actions

Status: proposal for the owner's decision. Nothing here is built. It depends on
the open question in issue #59 (should the state API offer any run-control or
queue-mutation route?). If the answer there is no, the Rex palette stays
read-only and this document records why.

## Today

The Rex actions only read. `lambe logs -json -brief` feeds the pickers, and the
only things they start are `lambe tui` and `lambe logs -issue ID -follow` in a
tab. The state API (`docs/api/state-api.md`) has four GET routes and one
mutating route, `POST /api/v1/refresh`, which the spec says needs an explicit
human decision before any client ships it.

Two facts shape the design:

- **Rex Lua cannot talk to the daemon directly.** Lua in Rex has no sockets and
  no file reads; it can only start programs with `rex.exec`. The bearer token
  lives in a 0600 descriptor file and must never leave `lambe`. So every
  mutation goes through a `lambe` subcommand that reads the descriptor, applies
  the client hardening rules, and calls the daemon. The Lua sees only exit
  codes and printed text.
- **One mutation already exists without a route.** `lambe enqueue --workflow W
  --apply ISSUE` labels a task through the tracker (`bd`), previewing by
  default. A palette "Queue issue" could shell that today. The routes below
  are for actions that need the daemon (it owns the run) rather than the
  tracker.

## Candidate actions

| Action | What it does | Needs a daemon route | Risk if misused |
| --- | --- | --- | --- |
| Queue issue | Pick a workflow, enter an issue id (`rex.ui.input`), label it | No: `lambe enqueue --apply` exists | Queues work that spends agent budget |
| Retry run | Re-dispatch a run that stopped (red gate, escalation, interrupted) | Yes | Repeats a costly attempt |
| Stop run | Terminate the run's process group and park the issue | Yes (the MCP `CancelRun` path already does this inside the daemon) | Loses in-flight work |
| Acknowledge | Mark an escalation as seen so it leaves Needs You | Yes, plus a place to store it | Hides a real problem |
| Pause / resume | Stop or start taking new work | Yes | Silent stall |

Approving a gate is deliberately absent: it is a verification decision, not a
convenience, and belongs in the tracker with a reviewer.

## Route design (if #59 says yes)

All routes sit under `/api/v1/`, use the existing bearer token and loopback
host checks, and answer in the existing error envelope.

| Method and path | Body | Effect |
| --- | --- | --- |
| `POST /api/v1/{issue}/retry` | `{"dry_run": bool}` | Re-dispatch a stopped run |
| `POST /api/v1/{issue}/stop` | `{"dry_run": bool}` | Terminate and park |
| `POST /api/v1/{issue}/acknowledge` | `{}` | Record that a human saw the escalation |

Shared rules, taken from the #59 sketch:

- **`dry_run: true`** returns what would happen (state now, state after) and
  changes nothing. `lambe` subcommands print the preview by default and take
  `--apply`, like `enqueue` and `setup rex`.
- **Idempotency key.** A required `Idempotency-Key` header; a repeated key
  returns the first result, so a retried request from a flaky link cannot
  start a second attempt.
- **Audit event.** Every accepted mutation writes one structured log line with
  the actor, issue, action, key and outcome. The token never appears in it.
- **State preconditions.** Retry only applies to a stopped run, stop only to a
  live one. Otherwise 409 with the current phase, so a stale picker cannot act
  on a run that has moved on.
- **Reserved names.** `retry`, `stop`, `acknowledge` are issue sub-paths, so
  they do not collide with the reserved top-level names (`state`, `queue`,
  `history`, `refresh`).

CLI surface that the Lua would call: `lambe retry ISSUE [--apply]`,
`lambe stop ISSUE [--apply]`, `lambe ack ISSUE`, each reading the descriptor
for the instance the existing `--config` selects.

## Rex side

- Every mutation action first shows a preview (the dry run's text) through a
  two-item `rex.ui.pick` (Apply / Cancel), because Rex has no confirm widget.
  Cancel and any prompt failure do nothing. Picking is only possible with an
  app connected, which is also the safeguard against `rex do` running a
  mutation unattended: the action takes no `apply` argument.
- The row for a run in **Open Run** and **Needs You** gets the follow-up
  action by choosing it (a second pick: Open output / Retry / Stop), so the
  palette stays one list and no key is bound to a destructive action.
- Results come back as the action's return value (and an error on failure, per
  the existing error policy), plus an OSC badge change from the daemon.
- A host without the new lambe simply lacks the subcommands; the plugin checks
  `LambeVersion` from `-json -brief` and hides the mutation rows.

## Verification plan (when built)

- Go: route tests for auth, dry run, idempotency, preconditions and audit.
- Lua: stub specs for the confirm pick, cancel, `no_client`, and the version
  gate.
- Headless on a scratch daemon: `rex -S macmini do` cannot answer a pick, so
  the Apply path needs a short walkthrough on a screen with an app connected.

## Decisions needed

1. Does the state API get any mutating route (#59)? If no, ship only "Queue
   issue" through `lambe enqueue`, or nothing.
2. If yes: is the set above right (retry, stop, acknowledge), and is
   acknowledge worth a stored state?
3. Idempotency keys required or optional (required is safer, costs one header)?
4. Should pause/resume exist at all, given it can stall the fleet silently?
