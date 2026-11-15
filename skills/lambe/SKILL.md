---
name: lambe
description: Operating discipline for coding agents supervised by lambe — how to claim Beads work, report progress, and handle gates without ever self-declaring done.
---

# Working under lambe

lambe is your supervisor. It assigns work from a Beads tracker, watches your
output, and judges completion with external gates — not your own opinion. This
is the discipline it expects. Your rendered WORKFLOW.md holds the specifics
(paths, commands, conventions) for this repo; when it and this file disagree,
WORKFLOW.md wins.

## Getting work

1. Read your assignment: `bd ready` in the configured tracker.
2. Claim it: `bd update <id> --claim`. Set `BEADS_ACTOR` to your agent identity
   first — claims are fenced by actor, and a claim you do not own will be
   taken from you.
3. One issue at a time. Finish, hand off, or block the current one before
   touching the next.

## Workspace

Work only in your assigned workspace directory. Do not modify other agents'
workspaces, the tracker state of other issues, or supervisor configuration.

## Reporting progress

After each meaningful step, write a progress comment to the hub on the issue.
Use these prefixes:

- `progress:` what you just did
- `verification:` how you verified it (commands run, output seen)
- `blocker:` what is stopping you and what you tried
- `next action:` what you will do next

Short and factual. The supervisor reads these to decide whether you are alive
and on track; silence looks like a hang and gets you reclaimed.

## Completion

**Never self-declare done.** Do not close, resolve, or mark the issue done.
Finishing your work means: implement, verify, commit if that is the convention,
post your final progress comment, and stop. External gates (`shell`, `gh:run`,
`gh:pr`) judge completion.

If a gate rejects your work, the reason is recorded on the issue. Read it,
address it, and continue — the rejection is a task, not a verdict.

## When blocked

If you cannot proceed, set the issue blocked with `bd update <id> --status
blocked` (or the documented equivalent), write a `blocker:` comment stating
what is blocked, what you tried, and what input you need, then stop. Do not
spin, do not guess around it, do not pick up other work.

## Heartbeats

If the tracker asks you to heartbeat, heartbeat on the requested interval.
A lost heartbeat means your claim may be reclaimed — if your work is taken
over, do not fight it; re-read the issue state before writing anything.
