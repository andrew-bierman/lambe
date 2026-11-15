# Project grounding: andrew-bierman/pigeon (v0.5.1, @task-pigeon)

## What Pigeon is today

- `package.json:4` — `"description": "Beads-compatible Cloudflare hosting with retained Pigeon migration and recovery tools."`
- `README.md:3` — "**Task coordination uses native Beads (`bd`).** Pigeon no longer provides task commands."
- `README.md:5` — "Cloudflare hosting and administration for the **official Beads HTTP client**. Use Beads for tasks, dependencies, claims and agent context; use Pigeon to host that contract on a Worker and SQLite Durable Objects."
- `README.md:15-21` — Architecture: "Official bd on each machine | authenticated HTTPS | Elysia Worker | SQLite Durable Object per tracker"; "It requires no Beads fork, custom task client, VM, Go container, D1, R2, KV or queue service."
- `README.md:9` — "**Current status: development trial.**"
- `CHANGELOG.md:5` (0.5.1) — "Retire the old task CLI; use native Beads for task coordination." `CHANGELOG.md:7-8` (0.5.0) — "legacy CLI operations require `PIGEON_LEGACY=1` for recovery and export."
- Legacy CLI survives at `bin: pigeon/task-pigeon -> dist/cli.js` (`package.json:31-34`); remaining commands include `whoami` (`src/cli.ts:1515`), `monitor` (Ink TUI, `src/monitor-tui.tsx`, 1,580 LOC), `wait` — "Block until visible tasks or messages change" (`src/cli.ts:2213-2214`).
- Legacy data model (drizzle/SQLite): tasks table with `uid, title, url, status, priority, claimed_by, blocked_by, labels, note, created_at…done_at` (`drizzle/0006_solid_task_pigeon.sql:2-17`); statuses `pending|claimed|blocked|done` (`packages/core/src/schema.ts:7-12`). 8 migrations in `drizzle/`.
- Active product `hosting/beads-worker/src/` (~20 modules): `coordination.ts`, `lifecycle.ts`, `journal.ts`, `stream.ts`, `recovery.ts`, `migration-authority.ts`, `admin-api.ts`, `bulk.ts`, `native-json.ts`, `gateway.ts`, `graph.ts`, `memories.ts`, etc.
- Skills: `skills/pigeon/SKILL.md:3` — "Coordinate agent work with the official Beads CLI…; use for task discovery, claiming, progress, handoffs, and explicit legacy recovery." `skills/pigeon-babysit/SKILL.md:3` — "Keep a named task or PR moving through the configured official Beads tracker, with ownership checks, durable progress and a clear completion or handoff condition."
- `packages/`: `core` (legacy task core), `driver-bun-sqlite`, `driver-turso`, `web` (dashboard w/ session auth + cached GitHub PR summaries), `worker-d1` (legacy sync worker, 208 LOC).

## Reusability for agent supervision

- Lease/heartbeat protocol exists: `hosting/beads-worker/src/coordination.ts:59` — `lease_expires_at = new Date(Date.parse(now) + 300000)` (5-minute leases); `heartbeat(id,…)` (:215), `release` (:263), and a reclaim scan over expired leases (:403-410) executed inside a request transaction — lazy reclaim, no DO alarms/cron.
- Event journal: `Journal` class with per-replica pins and pruned-payload collection (`journal.ts:15-17`); routes `/v0/beads/events` and `/v0/beads/events:watch` (`native-routes.ts:225,230`); SSE response `"Content-Type": "text/event-stream"` (`stream.ts:248`).
- Portable recovery: `BackupPayload { format: "pigeon-beads-backup/v1", schemaVersion: "2", identity, schema, rows }` (`recovery.ts:12-19`); operator migration path is "dry-run-first migration planning, guarded resumable application, rollback" (`CHANGELOG.md:10-11`).
- **No process/session launching anywhere**: only spawn in repo is `Bun.spawnSync` in `scripts/publish-package.ts:10`. No Codex/Claude/OpenCode/Pi adapters, no worktree management, no schedulers/crons.
- Supervision exists as *prompt conventions*, not code: `skills/pigeon-babysit/SKILL.md` — "On resume, read the issue and verify ownership/heartbeat rather than trusting the earlier claim"; "A shared `codex` actor defeats distinct-worker claim protection"; "End the loop when the requested work is verified complete, a genuine blocker prevents further authorized progress, a human takes over, or the user stops it."

## Compatibility

- Runtime: Bun `>=1.4.4` (`package.json:engines`), `packageManager: bun@1.4.4`; Node 24 LTS alignment commit 2026-11-05. TypeScript `^7.0.2`, strict tsconfig variant.
- Key deps: elysia 1.4 + @elysia/eden (Worker HTTP), drizzle-orm 0.45 + drizzle-kit (legacy), zod 4, ink 4/React 18 (monitor TUI), citty, cli-table3, @libsql/client (Turso driver). Pigeon itself `"license": "UNLICENSED"` (`package.json:5`); `hosting/beads-worker/compatibility/LICENSE.upstream` covers upstream-compat tooling.
- Split runtime: "Bun handles development, tests and operator commands; production executes in Cloudflare Workers" (`README.md:23`). Worker binding: `durable_objects: TRACKERS -> BeadsTracker (new_sqlite_classes)` (`hosting/beads-worker/wrangler.jsonc`); **no cron triggers**. Legacy CLI side is locally runnable (bun-sqlite/libsql); `worker-d1` builds browser-target.

## Scale and activity

- Non-test LOC: `hosting/beads-worker/src` ~10,020; `src` (legacy CLI) ~5,529; `packages/core` ~3,630; `scripts` ~1,360; `web` 469; `worker-d1` 208; drivers 315+68. The active product is now the majority of the code.
- 50 `*.test.*` files. `docs/beads-evaluation.md:29` — "**227 hosting tests** … Root checks also passed **484 tests** with legacy coverage unchanged. The direct matrix passes **182 cases per backend** and **90 exact refusal comparisons**."
- 154 commits since 2026-08-15; last commit 2026-11-05. Bursts: Oct 14–17 (GitHub PR-status links, agent guidance), Nov 2–5 (retire CLI, native Beads on Cloudflare, Bun/Node alignment).
- Release gates open: "Complete upstream compatibility and authenticated hosted Workers Free acceptance remain release gates." (`README.md:9-11`)

## Convention fit

- `AGENTS.md` — "Keep changes small, typed, and documented. Prefer the repo's existing Zod-first and command-first patterns."; publishing only via GitHub Actions manual `workflow_dispatch` with `confirm=yes`; never publish locally.
- `AGENTS.md` — dev discipline: `PIGEON_LOCAL_ONLY=1` + shared ignored `PIGEON_DB`; "Use disposable tracker/project identities for development."
- CI gates: `check` = lint (biome) + policy + intent:validate (@tanstack/intent) + typecheck + typecheck:strict + policy:code + test:coverage; plus smoke and pack:check (`package.json:scripts`).

## Pain signals

- No TODO/FIXME/HACK markers found in src/packages/hosting/skills/docs.
- Gap evidence is in gates, not TODOs: "a fresh live D1 capture was later denied with 403, so a future delta review remains necessary." (`docs/beads-evaluation.md:5`); "A claim also does not fence arbitrary external work after a lease expires." (`docs/beads-evaluation.md:16`); "Independent replicas can both claim work locally before merging" (`README.md:26-29`).

## Prior-decision scan

- `docs/beads-evaluation.md:3` — "**Retire Pigeon's competing task CLI; retain Pigeon as a Cloudflare hosting and administration layer for the unmodified official Beads HTTP client.** … Maintaining another task language and CLI is not justified by the legacy differentiation."
- `docs/beads-evaluation.md:18` — "Is that worth a separate project? **The old task engine/CLI is not.** A bounded hosting/admin layer is worth trialing… **Defer new orchestration, messaging and dashboard features until usage demonstrates a need.**"
- `docs/beads-evaluation.md:199` — "Do not add an orchestration system merely to make that mode imitate the selected HTTP authority."
- `docs/beads-evaluation.md:254` — "Pigeon's Cloudflare host/admin layer remains a separately gated hosting option; additional UI or orchestration work is a later decision."
- `README.md:103` — "An MCP service or extra orchestration daemon is not required." `skills/pigeon/SKILL.md:74` — "Use the project's native event tooling rather than building a new orchestration system."
- `docs/beads-cloudflare-reuse-audit.md:3-5` — "the user superseded the extraction recommendation below. Build Pigeon-owned Elysia/Bun code; use beads-cloudflare as reference only." Also notes the community dependency is "a young dependency with limited public adoption evidence" (:29-31).
- `docs/plans/2026-11-01-beads-cloudflare-compatibility-plan.md` — objective: "coordinate work across projects and machines through the official Beads client, using a self-hostable Cloudflare authority"; R4: "Match the native heartbeat contract, including refresh, re-arm, refusals and five-minute leases."
