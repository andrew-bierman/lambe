# lambe slice one — spec flow analysis (end-user perspective, pre-implementation)

Grounded in `docs/ARCHITECTURE.md` (read fully), `docs/research/deep-symphony-spec.md`,
`docs/research/deep-beads.md`, `docs/research/external-harnesses-failures.md`.
Users: (1) the human owner driving everything through bd/git/config; (2) supervised Pi
agents that read WORKFLOW.md, write progress to Beads, and are gated on completion;
(3) the daemon itself (the only net-new code). This analysis derives flows from what
THOSE users will actually experience, not a generic checklist.

---

## User Flows

### Flow A — happy path: responsibility trigger → pour → dispatch → run → gate → close
1. Human authors a formula in the hub repo (`.beads/formulas/*.toml`), commits, pushes.
2. Trigger fires (`bd cook` → `bd mol pour --dry-run` → pour). Molecule/steps materialize
   as hub issues with pinned ids.
3. Daemon tick: `bd ready` two-read → step is dispatchable → claim (CAS, lease TTL 5m).
4. Workspace: sanitize key, create dir, `after_create` hook (failure fatal to creation).
5. Local tracker: wisp/issue with session-binding metadata, attempt counter, pinned id
   as idempotency key. **Pgid binding written before `Start()` returns.**
6. Spawn Pi child (JSONL RPC), listeners attached before first `prompt`.
7. Agent reads WORKFLOW.md-rendered prompt, works, writes progress comments to hub,
   declares done.
8. Daemon does NOT trust the declaration. Runs gate (`bd gate check` — external tests/
   CI/typecheck defined in the git-versioned formula). Exit-gate green → close + terminal
   workspace cleanup (`before_remove` failures ignored). Gate red → result written back to
   Beads so the next attempt's prompt/record explains why it was rejected → retry with
   10s·2ⁿ backoff, 1s continuation.
9. Human wakes up to closed, externally-verified work.

### Flow B — crash/restart at each point (crash-only design)
- Kill -9 before claim: nothing happened; next tick claims.
- Kill -9 after claim, before spawn: lease expires (TTL 5m) + grace (10m) → daemon's own
  `bd reclaim --any-replica --id <id>` on its reclaim timer (~2×TTL) → re-dispatch.
- Kill -9 after spawn: local tracker still holds session id + pgid. On restart, reconcile
  derives state from Beads + local tracker: reattach or resume via `--session-id`; orphaned
  pgid killed by group before re-spawn (idempotency via pinned id).
- Kill -9 mid-gate: gate is external and re-runnable; close is CAS-guarded; no
  responsibility lost. This is the fault-injection suite's definition of done.

### Flow C — failover / reclaim
Machine-affinity default means failover is opt-in: another machine's daemon runs
`bd reclaim --any-replica` after TTL+grace, re-runs with context from the Beads record
(reconstruct, not resume — sessions are machine-local).

### Flow D — WORKFLOW.md hot reload mid-run
Spec §6.2: MUST detect, re-read, re-apply; invalid reload MUST NOT crash. In-flight runs
carry the prompt they were spawned with; next attempt uses the new version. Reconcile-on-
reload for in-flight runs is an open question (captured).

### Flow E — bd unreachable / hub remote down
`bd dolt pull`/push fails (GitHub down, network partition). Daemon must distinguish
tracker_request vs tracker_status errors, keep running agents on last-known state, and
not lose leases it can't heartbeat (heartbeat fails once reclaimed — agent must learn to
stop).

### Flow F — gate check itself failing (CI down)
`bd gate check` needs `gh` CLI / network. A gate that errors is not a gate that failed.
Conflating them either closes unverified work (CI outage → false green interpretation) or
penalizes the agent for infrastructure.

### Flow G — events-journal truncation mid-stream
`bd events tail --since <seq>` returns 410/truncated; journal is per-replica, doesn't
cover sync, raw SQL, or compaction. Consumer must re-baseline (`bd ready` full read).

### Flow H — duplicate dispatch races
Two ticks (or a tick racing a wake) see the same dispatchable step. Claim CAS makes one
win; the loser must cleanly stand down. Local pinned-id idempotency is the second fence.

### Flow I — limit lockout with no limit signal (Pi)
Pi surfaces provider failures as generic errors; no limit channel. Daemon must classify
limit-vs-failure from heuristics (error text patterns, retry cadence) and defer + wake at
reset rather than burning backoff attempts.

### Flow J — hook failures
after_create fatal (no workspace, retry later); before_run aborts attempt (counts toward
retry?); after_run logged only; before_remove ignored.

---

## Gaps

### Critical

**C1. "Done declaration" channel is unspecified — the gate's trigger is undefined.**
The spec says done = external checks green, never exit code, and fake-done is a fault
scenario. But nothing defines HOW the daemon learns the agent believes it's done. Pi's
`agent_settled` means "Pi will not continue automatically" — an agent that gives up
mid-task also settles. Candidates: agent writes a Beads comment/status (which one? what
value?), agent exits with a marker, daemon gates on every settle. This sits upstream of
the entire verified-completion story and of "results written back so next attempt learns
why rejected."
*Why it matters:* without it, the gate either runs on every settle (fine but must be
stated) or never runs; the fake-done fault-injection test can't even be written.
*Default assumption:* gate runs on every `agent_settled`; gate-red → retry with the gate
output injected as continuation guidance; N consecutive reds → surface to human, don't
loop forever.

**C2. Gate failure vs gate red vs CI-unreachable are three outcomes; spec defines one.**
Flow F. `bd gate check` evaluating a `gh:run` gate with GitHub down errors; a flaky test
is red; a genuinely failing test is red. The spec's "done = external checks green" gives
no semantics for error. Treating error as red burns retry attempts during infra outages
(and can hit terminal state → workspace cleanup while work was actually fine); treating
error as green closes unverified work.
*Why it matters:* this is the exact class of overnight-run failure lambe exists to prevent.
*Default assumption:* gate error → pause the issue (defer with wake/retry gate on timer),
never terminal, never green; alert human after sustained gate-infra failure.

**C3. Retry budget / terminal-state policy for the loop as a whole is missing.**
Spec §8.4 gives backoff math but not a max-attempts or max-wall-clock per responsibility,
nor what terminal failure looks like from the human's perspective (Beads status? comment?
notification?). An agent that fake-dones forever, or a gate that stays red forever, loops
all night burning tokens — the Gas Town token-burn failure mode the architecture doc
itself cites as a non-goal motivation.
*Default assumption:* per-responsibility attempt cap (configurable, e.g. 8) → move issue
to a needs-human status, write a Beads comment with the last gate output, stop
dispatching; wake on human intervention.

**C4. Local tracker writability by the agent vs the daemon — single-writer file lock conflict.**
The local embedded-Dolt tracker is "in-process, file-lock single-writer." If the daemon
embeds it AND supervised agents run `bd` commands against the same local tracker (progress
writes, defer), the file lock serializes/conflicts them — and every bd invocation pays
engine startup. The spec says session bindings/counters are daemon-written, but flow A
step 7 has agents "write progress to Beads" — which Beads? Hub (fine) or local (lock
contention + does the agent even see the same repo)? The captured open question
("local-tracker readability by agents") is the read side; the write/contention side is
unaddressed.
*Default assumption:* agents only ever touch the HUB tracker via adapter tools; local
tracker is daemon-private; attempt-number parity flows into the rendered prompt
(`{issue, attempt}` strict rendering — spec already requires it), which answers the
readability question by making reads unnecessary.

**C5. Kill -9 mid-close: the gate-green→close→cleanup sequence has no idempotency spec.**
The fault-injection suite covers kill mid-reconcile generically, but the specific
dangerous window is: gate passed, `bd close` succeeded, daemon died before workspace
cleanup / local-tracker wisp cleanup. On restart, the issue is terminal — reconcile
"stops runs on terminal states" and startup sweep deletes workspaces — but the sweep must
not delete a workspace whose cleanup already ran, and must not kill an agent still running
for an issue another (reclaimed) orchestrator closed. Distinguish "daemon restart" from
"this run was reclaimed by someone else" for a live child.
*Default assumption:* terminal cleanup keyed on pinned id + tombstone; kill of live child
gated on lease ownership re-verification (`bd heartbeat` fails → stand down WITHOUT
killing if assignee changed).

### Important

**I1. Heartbeat cadence vs lease TTL vs reconcile-loop period is unspecified.**
TTL 5m default; heartbeat must fire well under TTL even when the daemon is busy running
many agents; reclaim timer ~2×TTL. No numbers for heartbeat interval, max concurrent
agents per machine, or what happens to in-flight children when heartbeat fails mid-turn.
*Default:* heartbeat every TTL/3 from an independent goroutine; heartbeat failure → do not
kill child immediately, attempt CAS re-claim, if lost → SIGTERM ladder for the child.

**I2. Pi silence-watchdog thresholds and the "settled-but-silent" vs "hung" distinction.**
Pi has no hang signal; the daemon owns deadlines. Spec gives turn_timeout 1h default
(Symphony) but lambe needs: per-turn silence threshold, what the daemon sends before
killing (a `steer`? a follow_up "report status"?), and how a limit lockout (Flow I) is
distinguished from a hang so it defers instead of killing.
*Default:* escalate steer → SIGTERM → grace → SIGKILL-group; heuristic limit classifier
(error-string patterns + repeated failures with no progress writes) → defer with
wake-at-reset; log classification confidence.

**I3. `bd` invocation cost model: embedded engine startup per call × per-tick × per-agent.**
Deep-beads explicitly warns loops calling bd hundreds of times pay engine startup each
time; recommends `bd batch` or server mode. The daemon's tick does ready→show→claim→
heartbeat→gate-check→defer-write per issue. Slice one is one machine, but the loop shape
must be designed for batching (or embedding bd as a library — flagged as open evaluation
in ARCHITECTURE.md). Not decided = perf cliff baked into slice one.
*Default:* design the tracker adapter interface around batched reads from day one; shell
out per-command in slice one but measure; revisit library embedding before multi-agent.

**I4. WORKFLOW.md reload semantics for retry/next-attempt vs in-flight (sharpening the captured question).**
The spec-captured question covers authorship and reconcile-on-reload. The un-sharpened
part: an in-flight run whose WORKFLOW.md changed such that the gate formula changed —
does the running agent get judged by the old gate (the one it was prompted under) or the
new one? Gate formulas live in the hub repo, git-versioned, but nothing says which
revision a given attempt pins.
*Default:* pin formula+WORKFLOW revision at claim time; attempts are judged by the gate
revision they were dispatched under; new revision applies on next claim.

**I5. Reclaim-by-other-replica while this daemon still lives (split brain at sync-interval granularity).**
Fence visibility = assignee status, stale one sync interval — documented, not pretended
away. But the failure flow is unowned: daemon A heartbeating a local lease it still holds
while machine B reclaimed after a GC pause on A pushed the issue back to open and claimed
it. Two agents, one workspace? Different machines → different workspaces → same git
branch → conflict at push. The spec's answer is "machine-affinity default," but reclaim
--any-replica exists and the doc lists failover as a feature.
*Default:* failover is manual/opt-in for slice one; when enabled, workspace/branch
collisions resolved by pinning branch name to pinned-id + attempt, and the late heartbeat
loser must stand down before its next hub write (CAS exit-13 as the tripwire).

### Minor

**M1. Observability surfaces for the human.** Structured logs with issue_id/session_id
are spec'd; nothing says where the human looks at 8am to see "what happened overnight"
(gate rejections, defers, limit events). Default: log lines + HTTP /api/v1/state; a
`lambe status` CLI reading the local tracker.

**M2. Config/zero-config boundary.** "Batteries-included, zero-config single machine" —
but TTL, grace, backoff caps, watchdog thresholds, attempt caps are all knobs. Which are
config vs constants matters for the first-run user. Default: constants with env overrides,
documented.

**M3. Events-journal re-baseline trigger (Flow G).** Truncation handling exists as 410
semantics; the daemon just needs a defined fallback (full `bd ready` re-read). Default:
on truncated, drop cursor, full two-read, log.

**M4. Agent progress-comment format.** Agents write progress to the hub; no convention
for machine-readable progress (used by I2's non-progress heuristics and C1's done signal
if comment-based). Default: define a minimal convention in WORKFLOW.md template.

---

## Questions

1. **(Critical, C1)** What is the done signal — Beads comment, status transition,
   agent_settled, or exit marker? And is the gate evaluated on every settle or on a
   declared handoff? *Stakes:* verified-completion can't be built or tested without it.
   *Default:* gate on every agent_settled; red → retry with gate output as continuation
   guidance.

2. **(Critical, C2)** Gate-infra error semantics: defer-and-retry the gate, or fail the
   attempt? *Stakes:* overnight CI outage either falsely closes work or falsely burns
   attempts. *Default:* defer + retry gate on timer; never green, never terminal on error.

3. **(Critical, C3)** Attempt cap and needs-human terminal state: what status, who's
   notified, does the workspace survive for inspection? *Stakes:* unbounded token burn —
   the documented Gas Town failure. *Default:* cap 8, needs-human status + comment with
   last gate output, workspace retained until human resolves.

4. **(Critical, C4)** Do agents ever touch the local tracker, or hub-only with
   attempt-count parity via the rendered prompt? *Stakes:* file-lock contention design and
   the whole local-tracker trust boundary. *Default:* hub-only; prompt carries attempt
   number (spec already renders `{issue, attempt}`).

5. **(Critical, C5)** On restart with a live child whose issue was reclaimed: kill or
   stand down? *Stakes:* duplicate side effects across machines — the exact fault the
   suite tests. *Default:* re-verify lease/CAS; if lost, stand down without killing only
   if assignee changed; else resume same session.

6. **(Important, I1)** Heartbeat interval and behavior on heartbeat failure mid-turn?
   *Default:* TTL/3 independent goroutine; failure → CAS re-claim, then SIGTERM ladder.

7. **(Important, I2)** Silence-watchdog threshold and pre-kill probe (steer "status?")?
   And the limit-classification heuristic for Pi's generic errors? *Default:* 1h turn
   timeout, steer-probe at threshold, pattern-based limit classifier with defer+wake.

8. **(Important, I4)** Gate/WORKFLOW revision pinned at claim? *Default:* yes, per-attempt.

---

## Recommended Next Steps

1. Resolve Q1–Q4 (done-signal, gate-error semantics, attempt cap, agent-vs-local-tracker
   boundary) in the plan doc before any interface definitions — they shape the tracker
   adapter, harness driver, and gate interfaces respectively.
2. Write the fault-injection suite FIRST as executable specifications of C2/C3/C5 flows
   (gate-infra-down, fake-done loop, kill-mid-close); the suite is already the stated
   definition of done and will force these decisions concretely.
3. Specify the human's morning-read surface (M1) in slice one at minimal cost: structured
   log + `lambe status` from the local tracker; defer HTTP.
4. Add a "revision pinning" line to the responsibilities-as-formulas design (I4) so gate
   versioning is settled before the first formula is authored.
5. Prototype-measure the bd invocation cost per tick (I3) on the real hub before
   committing to shell-out vs embed for slice one.
