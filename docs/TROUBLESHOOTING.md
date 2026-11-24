# Troubleshooting

Start with `lambe doctor`. It is read-only, checks everything below that it can
see, and prints a `fix:` line for each problem. Add `--json` for scripts.

## Quick index

| Symptom | Go to |
| --- | --- |
| `lambe: unknown command` / `usage` errors, exit 2 | [Usage errors](#usage-errors) |
| `no lambe configuration at ...` | [No config](#no-config-or-config-will-not-load) |
| `unknown key ... (did you mean ...)` | [No config](#no-config-or-config-will-not-load) |
| `read tracker ...: no such file or directory` | [Tracker not found](#tracker-not-found) |
| `exec: "bd": executable file not found` | [bd missing](#bd-missing) |
| `no completion gate configured` | [No gate](#no-completion-gate) |
| Daemon not running / nothing is happening | [Nothing happens](#nothing-happens) |
| A run shows `stalled` | [Stalls](#stalled-runs) |
| An issue says needs-human | [Needs-human](#needs-human-issues) |
| `orphaned claims` in status | [Orphaned claims](#orphaned-claims) |
| `lambe init` refuses to use a tracker | [init](#lambe-init-refuses-an-ancestor-tracker) |
| `lambe run` exits 3 | [Lock held](#lambe-run-exits-3) |
| Service will not start | [Service](#service-will-not-start) |

## Usage errors

Exit code 2 means the command line was wrong: an unknown command or flag, or a
missing argument. The message is one line and ends with `run 'lambe help CMD'`.
`lambe help CMD` shows every flag with examples.

## No config, or config will not load

`lambe init` writes the config to your user config directory (`lambe state-dir`
shows state; config lives at `~/Library/Application Support/lambe/config.yaml` on
macOS and `~/.config/lambe/config.yaml` on Linux). Use `--config FILE` or
`LAMBE_CONFIG` to point elsewhere.

Unknown keys and wrong types are errors with the key, file and a suggestion.
Valid keys are in the [configuration reference](reference/config.md).

## Tracker not found

Commands read the Beads tracker at the config's `trackers[0].path`. Run
`bd init` in that project root, or fix the path (relative paths resolve from
where you run the command). `lambe doctor` prints the path it tried.

## bd missing

Lambe drives Beads through the `bd` binary. Install Beads
([github.com/gastownhall/beads](https://github.com/gastownhall/beads); the
Homebrew formula depends on it). Services start with a minimal PATH; the unit
`lambe service install` writes bakes in a PATH that finds `bd`, so reinstall it
(`--force`) if you installed `bd` afterward.

## No completion gate

`lambe run` refuses to start without at least one gate, because without one
nothing proves an issue is done. Add a `[gate]` table to a formula in
`.beads/formulas/` (see the starter `keep-tests-green.formula.toml` and
[Completion gates](../README.md#completion-gates)).

## Nothing happens

1. `lambe doctor`: check `daemon`, `loop`, `service`.
2. `lambe status`: is there ready work? Issues need to be open, unassigned and
   unblocked, and carry your configured `labels` if you set any.
3. `lambe logs --follow` or the service log (`journalctl --user -u lambe -f`,
   `~/Library/Logs/lambe/daemon.log`).
4. A gate-less setup, a missing `WORKFLOW.md`, or a missing agent executable
   stops `lambe run` at startup; the error names which.

## Stalled runs

A run with no agent activity for `supervisor.stall_after` (5m) is `stalled`.
Quiet tool calls (a long test suite) are normal; read `lambe logs --issue ID`.
At `supervisor.silence_timeout` (10m) the watchdog probes, then kills the run
and a new attempt starts. Raise the timeouts if your tools are legitimately slow.

## Needs-human issues

An issue that used all its attempts (`supervisor.attempt_cap`) or hit a hard
gate failure waits for a person. `lambe status` lists them with the reason.
Fix the cause, then reopen or update the issue in Beads.

## Orphaned claims

A claim whose agent is gone and cannot be resumed. `lambe claims list` shows
them; `lambe claims release ISSUE` frees one after verifying the agent and
workspace are gone (`--force` skips the checks, so be sure). Details:
[OPERATIONS.md](OPERATIONS.md#orphaned-claims).

## `lambe init` refuses an ancestor tracker

By design `lambe init` only writes inside the directory you ran it in. To adopt
a tracker in a parent directory pass `--tracker PATH`; to create a separate one
run `bd init` here first.

## `lambe run` exits 3

Another daemon holds the instance lock for this tracker (service managers treat
3 as "do not restart-loop"). `lambe state-dir` lists live daemons. Stop the
other one, or use a distinct `LAMBE_STATE_DIR` for an isolated instance.

## Service will not start

`lambe service status` and `lambe doctor` (`service` check) show the unit and
state. On macOS the log directory must exist (`lambe service install` creates
it); on Linux run `loginctl enable-linger "$USER"` to survive logout.
`lambe service install --dry-run` prints exactly what would be written.

## Still stuck

Run `lambe version --json` and `lambe doctor --json`, then open an issue on
the repository with both (they contain no secrets; review before pasting).
