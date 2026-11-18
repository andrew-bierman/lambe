# Precedent & Activity Dossier — Pigeon direction (agent-orchestration candidate)

Scouted 2026-11-15. Repo: ~/Code/pigeon; tracker: andrew-bierman/pigeon (gh authenticated; PR diffs not read).

## Precedent (prior decisions / abandoned attempts / stated direction)

1. **Task-CLI direction was explicitly reversed, not regretted silently.** `docs/beads-evaluation.md:3` (reviewed 2026-11-01): "Retire Pigeon's competing task CLI; retain Pigeon as a Cloudflare hosting and administration layer for the unmodified official Beads HTTP client… Maintaining another task language and CLI is not justified by the legacy differentiation." Enforced by CHANGELOG 0.5.1 ("Retire the old task CLI; use native Beads for task coordination") and merged PR #39 (2026-11-02): "Pigeon no longer exposes its former task CLI, including through the old environment-variable escape hatch."

2. **Orchestration has an explicit prior stance: deferred/rejected.** `docs/beads-evaluation.md:18`: "Defer new orchestration, messaging and dashboard features until usage demonstrates a need." And `docs/beads-evaluation.md:199` (agent automation section): "Do not add an orchestration system merely to make that mode imitate the selected HTTP authority." Same stance in the shipped supervision skill, `skills/pigeon-babysit/SKILL.md`: "The workflow does not depend on a dashboard, inbox or new orchestration daemon."

3. **Supervision already exists — as guidance, not infrastructure.** `skills/pigeon-babysit/SKILL.md` ships a named-task supervision loop: claim → ownership/heartbeat verify → progress comments → "Finish or hand off… A merged PR does not finish a task whose required checks, release or deployment remain." An earlier version was hardened via closed issue #10 (2026-08-17, "Tighten babysit so agents stay on one shared queue") and merged PRs #9/#11 ("add babysit loop skill", "tighten babysit queue contract").

4. **Hosting-vs-reuse decision also recorded and superseded once.** `docs/beads-cloudflare-reuse-audit.md:3-6` (2026-11-01): "the user superseded the extraction recommendation below. Build Pigeon-owned Elysia/Bun code; use beads-cloudflare as reference only. The extraction experiment is archived outside the shipping tree." Pattern: deliberate decisions get written down and labeled, including reversals.

5. **Original direction (2026-08-14) deliberately scoped orchestration out.** `docs/plans/2026-08-14-pigeon-cli.md`: "Pigeon is a small personal task-claiming CLI… Out of scope: sync, multi-user accounts, daemon processes, remote servers, plugin systems, TUI, notification integrations, and complex scheduling." Every out-of-scope item was later revisited (sync → D1/Turso PRs #18/#19, remote → Cloudflare, dashboard → PR #29), i.e. scope grew by decision, each documented in CHANGELOG.

6. **Latest stated trial priority narrows further.** `docs/beads-evaluation.md:7`: "The user's latest trial priority is native Beads first, without Cloudflare. The Worker is not a prerequisite for that trial." Also `docs/beads-evaluation.md:21`: "Use one personal coordination tracker across projects initially… Keep work/team trackers separate when their access or data policy differs."

7. **Explicit reassessment trigger on the incumbent.** `docs/beads-evaluation.md:18`: "Reassess that maintenance cost if upstream offers an equivalent Cloudflare host."

## Incumbent pain & exposure (open issues / in-flight PRs / second-guessing)

8. **Open issue #34** (2026-10-19): "Allow blocked tasks to resume through a supported CLI/RPC transition" — stale blocked state strands agent work; reproduced twice (tasks 781/786, again #695 on 2026-10-23). Note: filed against legacy CLI v0.4.1, since retired in 0.5.1; left open.

9. **Open issue #32** (2026-10-17): "Support safe task reassignment independently of execution claims" — "ten active engineering tasks incorrectly owned by a human and sixteen under retired agent identities"; reconfirmed against 0.4.1 on 2026-10-23+. Also legacy-era, still open.

10. **Multi-agent board-split pain already bit once.** Closed issue #10 (2026-08-17): "agents can end up reading different local `.pigeon` databases from different worktrees, which makes the board look empty or stale even while other agents are actively working. That is the failure mode we just hit." Related still-open pain: issue #12 (2026-08-17, worktree-local DB resolution) + open PR #36 (2026-10-25, "Fix local queue discovery across linked worktrees") — the only open PR, and it patches the retired legacy surface.

11. **Claim semantics limits acknowledged, not just complained about.** `docs/beads-evaluation.md:16`: "A claim also does not fence arbitrary external work after a lease expires." `:59`: legacy had "no implemented typed task DAG, replica sync, authenticated per-agent accounts, claim TTL/reaper or standalone MCP server."

12. **Known upstream (Beads) defect pinned.** `docs/beads-evaluation.md:186`: "Reviewed native stale-cache discrepancy… the spec nevertheless exposes the stored flag, so Pigeon's fresh-derived result is an observable raw difference… Do not claim 100% parity." Upstream concurrent-claim/auto-push reports (beads #5998, #5157) cited as historical risk evidence (`:152`).

13. **Adoption gates still open (self-declared).** `docs/beads-evaluation.md:247-252`: "Exact current-head remote CI, final caller-owned review and shipping" outstanding; "Actual Claude Code application/model execution was not tested"; "a fresh live D1 capture was later denied with 403, so a future delta review remains necessary" (`:5`); installed global skills "remain unchanged" (`:201`). README: "Current status: development trial."

14. **No regret language found about the Beads pivot itself.** A repo-wide grep for regret/abandon/second-guess/wrong terms surfaced only documented decision supersessions (#1, #4). Plans are explicitly marked historical where stale: `docs/beads-evaluation.md:61`: "Old plans excluded features that later shipped or named libraries later replaced. They remain historical documents, not evidence that all their content is still planned work."

## Tracker coverage note

Issues/PRs searched for orchestration, supervision, agent, cron, schedule, retry, sync, durable, session, resume, Beads. No issue or PR evaluates a coding-agent orchestration/harness-integration system; the only orchestration mentions are the deferral/rejection language in #2 above and the retired orchestrator-role references inside legacy-era issues #32/#34.
