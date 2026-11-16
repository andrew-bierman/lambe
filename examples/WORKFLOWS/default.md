---
# Default WORKFLOW.md — a complete, copy-pasteable template.
#
# This file is the per-responsibility supervisor config plus the agent
# prompt in one. Layout: YAML front matter between --- delimiters,
# followed by the prompt body. Copy to your tracker repo root (or wherever
# the daemon's loader points), edit, and it hot-reloads: a valid edit
# applies from the NEXT claim; an invalid edit is logged and rejected
# while the last-good copy keeps serving. In-flight runs always finish
# under the revision they claimed.
#
# Front matter schema (exactly what internal/symphony/workflow.go parses —
# unknown keys in `hooks` are a load error; other keys are ignored):
#   hooks:            map of hook-name -> shell command string
#                     allowed names: after_create, before_run, after_run,
#                     before_remove (anything else: parse error)
#   max_turns:        positive integer           (default 20)
#   hook_timeout_ms:  positive integer, milliseconds (default 60000)
#
# Every STRING value in front matter (hook commands included) goes through
# $VAR / ${VAR} / ${VAR:-default} environment resolution at load time. A
# referenced variable that is unset and has no default is a load error —
# set LAMBE_GH_REPO below or always run with it in the environment.
hooks:
  # Runs once, only when the workspace directory is first created.
  # Failure is fatal to creation: the runner is never invoked and creation
  # is retried on a later tick under the failure backoff.
  after_create: "git clone --depth 1 ${LAMBE_GIT_REMOTE:?set LAMBE_GIT_REMOTE} ."
  # Runs before each attempt. Failure aborts the attempt (a failure retry
  # is scheduled; no turns run).
  before_run: "git checkout --detach && git clean -fdq"
  # Runs after every attempt, including timeouts and cancellations.
  # Failures are logged and ignored.
  after_run: "git status --short || true"
  # Runs before workspace deletion. Failures are ignored; cleanup proceeds.
  before_remove: "echo workspace removed"
max_turns: 20
hook_timeout_ms: 60000
---

# How the prompt is rendered
# --------------------------
# The body below is a STRICT template rendered per attempt:
#   {{ issue }}    the issue title (the bead being worked)
#   {{ attempt }}  the attempt number, 1-based
# The only other rendered message is the between-turns continuation —
# "Issue \"...\" is still active. This is attempt N." followed by the
# last gate output (a variable the loop injects for that message only;
# you cannot use it in this body) and "Continue or hand off." — sent on
# the SAME live thread between turns of one attempt.
#
# Rendering is fail-closed: an unknown variable, an unknown filter, or an
# empty token (empty braces) is a hard error — rendering never silently produces
# blank output. Supported filters: {{ issue | trim }}, {{ attempt | upper }},
# {{ attempt | lower }}. Use ONLY these two variables (plus filters);
# anything else fails the turn's prompt and burns a failure retry.

You are an autonomous coding agent working the issue "{{ issue }}".

This is attempt {{ attempt }}. Previous attempts, claims, and gate results
are recorded in the tracker; read them before repeating work.

Working rules:

1. Work only inside this workspace; it is your per-issue workspace.
2. Read the tracker for the issue's current state before acting.
3. Write progress to the tracker as you go.
4. When you believe the work is finished, declare completion; an external
   gate verifies it. A green gate is the only path to done. The gate judges
   CI on the PR head commit, never your local test output — so push, open
   the PR, and let the gate see the same commit reviewers will.
5. A red gate sends its output back with the next attempt: fix the cause,
   do not argue with the gate.
6. If you cannot continue, hand the issue off: leave the workspace and the
   tracker in a state a human or the next attempt can pick up.
