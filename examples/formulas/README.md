# Formulas — responsibilities as code

A **formula** is one responsibility expressed as a small TOML file
(`*.formula.toml`, git-versioned). A responsibility is a standing obligation
you want held true even while you sleep — "main is green", "the backlog is
burned down", "every PR gets reviewed" — not a one-off task.

## Anatomy

Every formula has up to three parts. Lambe's parsers read exactly two of
them; the steps are for `bd`.

| Table | Who parses it | Shape |
|---|---|---|
| `[trigger]` | `internal/symphony` (`LoadFormulas`) | `command` (required, string). Runs under `sh -lc` with the formula's directory as cwd. **Non-zero exit = fired.** |
| `[gate]` | `internal/gate` (`ParseSpecFile`) | `type` ∈ `shell` \| `gh:run` \| `gh:pr`. `shell` needs `command`; `gh:run` needs `ref` (or `run_id`); `gh:pr` needs `pr`. |
| `[steps]` | `bd cook` / `bd mol pour` | The finite issues materialized when the trigger fires. |

Unknown gate types and a missing `trigger.command` are hard load errors.

## How to pour one

Rehearse first — cook and pour both take `--dry-run`:

```sh
bd cook <formula-name>            # steps -> ephemeral protos
bd mol pour --dry-run             # see what would enter the tracker
bd mol pour                       # for real; issues now dispatchable
```

Trigger-fired formulas pour themselves: on each tick the loop evaluates
every trigger, and a fired one is materialized so its issues flow into the
same two-read dispatch (status read, then claim) as any other work.

## Gates: why CI-on-PR-head

A gate is what "done" means, and the only path to done is a green gate.
The `gh:run` gate judges the GitHub Actions conclusion on the PR **head
commit** — not the agent's word, and not its local test run. This is what
prevents same-commit gaming: the proof runs on GitHub's machines against
the exact merge candidate, on a commit the agent cannot re-run locally and
relabel. Local `shell` gates are for purely local invariants (formatting,
typecheck) that run in the sanitized workspace.

## The examples

- [`keep-tests-green.formula.toml`](keep-tests-green.formula.toml) — the
  flagship. CI-watching trigger, diagnose+fix steps, `gh:run` gate on the
  PR head.
- [`backlog-worker.formula.toml`](backlog-worker.formula.toml) — finite
  work. No real trigger (manual pour), one labeled issue claimed per pour,
  `shell` gate (tests + typecheck + format).
- [`pr-reviewer.formula.toml`](pr-reviewer.formula.toml) — human-gated.
  Review-pending trigger, agent-summarizes-human-approves steps, and a
  `shell` gate polling the GitHub review decision as the human exit-gate
  (there is no native `human` gate kind — the parser accepts only `shell`,
  `gh:run`, `gh:pr`).

Pair formulas with a [`WORKFLOW.md`](../WORKFLOWS/default.md) — the front
matter configures the supervisor (hooks, turn cap) and the body is the
agent prompt rendered over `{{ issue }}` / `{{ attempt }}`.
