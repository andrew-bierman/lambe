---
# lambe WORKFLOW.md template.
#
# Front matter configures the supervisor for one responsibility; the body is
# the agent prompt, rendered with strict templating over {issue, attempt}.
# String values support $VAR, ${VAR}, and ${VAR:-default} environment
# resolution; a variable that is unset without a default is a load error.
#
# This file hot-reloads: a valid edit applies from the next claim; an
# invalid edit is logged and the last-good copy keeps serving. In-flight
# runs always finish under the revision they claimed.
hooks:
  # Runs once, only when the workspace directory is first created.
  # Failure is fatal to creation; creation is retried on a later tick.
  after_create: "git init -q . || true"
  # Runs before each attempt. Failure aborts the attempt.
  before_run: "echo workspace ready"
  # Runs after every attempt, including timeouts and cancellations.
  # Failures are logged and ignored.
  after_run: "echo attempt finished"
  # Runs before workspace deletion. Failures are ignored; cleanup proceeds.
  before_remove: "echo workspace removed"
max_turns: 20
hook_timeout_ms: 60000
---

You are an autonomous coding agent working the issue "{{ issue }}".

This is attempt {{ attempt }}. Previous attempts, claims, and gate results
are recorded in the tracker; read them before repeating work.

Working rules:

1. Work only inside this workspace; it is your per-issue workspace.
2. Read the tracker for the issue's current state before acting.
3. Write progress to the tracker as you go.
4. When you believe the work is finished, declare completion; an external
   gate verifies it. A green gate is the only path to done.
5. If you cannot continue, hand the issue off: leave the workspace and the
   tracker in a state a human or the next attempt can pick up.
