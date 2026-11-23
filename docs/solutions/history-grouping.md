---
title: History lists one row per issue; closed-externally stops; gate rejections in status
date: 2026-11-22
category: design
module: internal/daemon (history, status), internal/surface/tui, internal/httpstate, internal/symphony
problem_type: ux
severity: medium
tags: [history, tui, state-api, gate, status]
---

# History grouping, closed-externally stops, gate rejections

## Observed

On the production daemon: History listed an issue once per attempt (a demo
issue showed three `interrupted` rows above its final `passed / stopped: gate
green` row), and the Mac app's list headlined `interrupted` while the detail
said `gate green`; 47 issues closed outside lambe all showed as `stopped:
recovered closed issue`, drawn red; `lambe status` always printed `last-night
gate rejections: (history unavailable)`.

## Causes

- The history builders (`StateSourceAdapter.History`, the TUI's history view)
  listed raw attempt records. A restart archives the in-flight attempt as
  `interrupted` with a fresh `UpdatedAt`, so it sorted above the real outcome,
  and the API's first-wins dedupe per `issue/attempt` could let the
  `interrupted` copy hide the final one.
- Every `stopped:` phase other than `gate green` renders as a failure.
- `Report.GateRejections` was never filled. Gate output is persisted only when
  an issue escalates to needs-human, and a red verdict just schedules a retry.

## Design

**Grouping, once.** `daemon.GroupHistory` partitions entries per issue. Within
a group: one entry per attempt number, newest attempt first; at the same attempt
number a real outcome beats `interrupted` or a phase-less registry row; ties go
to the later `UpdatedAt`. The first entry is the headline, so an interrupted
attempt headlines only when nothing later exists. Groups sort by headline time.
The TUI (which reads the live projection directly) and the state API history
builder both call it, so the two surfaces cannot disagree.

**Wire contract (still v1, additive only).** `HistoryRunDTO` keeps every field
and now describes the latest attempt; it gains `attempt_count` (always) and
`attempts` (`HistoryAttemptDTO` list, newest first, headline included). Old
clients see the same fields with fewer rows. Fixtures and
`docs/api/state-api.md` are updated; `schema_version` is not bumped.

**TUI.** One history row per issue, `×N` after the title when N > 1; the detail
pane opens with an "N attempts" list (glyph, attempt, outcome, age). Active
queue and attention views are unchanged.

**Closed externally.** The loop's recovery stop reason becomes `closed
externally`; `ReadLive` maps the legacy `stopped: recovered closed issue` to
`stopped: closed externally` so already-persisted history reads the same. The
TUI draws it `◌`, neutral, word `closed`. It stays a `stopped:` phase (still
visible in history, `canceled` coarse state); clients treat unknown phases as
neutral text.

**Gate rejections.** `Loop.OnGateRed` is called on every red verdict; the
daemon appends a record to `<state-dir>/gates/<instance>.jsonl` (0600): issue,
attempt, time, output. Output has ANSI and control characters stripped,
whitespace collapsed, and is truncated to 512 bytes. The file compacts to the
newest 200 records past 256 KiB. `lambe status` reads the last 24 hours. It
lives in the state dir rather than tracker metadata on purpose: a rejection
write must not add load to a hub that is already the bottleneck (see
`orphan-claim-heartbeats.md`), and `lambe status` already reads local state for
orphaned claims. The gate kind is not recorded: the loop receives a verdict and
output, not which gate produced it. Secrets that a gate prints are not detected;
the bound and the 0600 mode are the mitigation.
