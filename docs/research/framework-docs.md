# Framework Docs Research: Pi RPC + Beads `bd` 1.4.0 (for lambe)

## Summary
Pi's RPC mode is a strict-JSONL child-process protocol with ID-correlated responses and a
session-event stream; `agent_settled` (not `agent_end`) is the completion signal a supervisor
must wait on. bd 1.4.0's CLI/HTTP surface matches the plan's needs (claims+leases, defer
`--until`, gates, cook/mol, batch, serve+SSE journal) but several planned flag names are
wrong: it's `bd update --claim` (no `bd claim`), `--until` (not `defer_until` CLI flag /
no `--overdue`), and `--dry-run` exists on all template commands. bd is importable as a Go
module but docs flag the Go SDK (`beads` package) as not covered by some write switches —
prefer the CLI/HTTP surface. No Go client for Pi exists; RPC is the intended integration.

## Version Information
- **Pi**: docs fetched from `earendil-works/pi` master (`packages/coding-agent/docs/`:
  rpc.md, rpc-commands.md, json.md, session-format.md, cli.md). Pi moves fast; docs say
  `pi --help` is authoritative for the installed version. No version numbers in docs —
  pin a Pi release and re-verify `rpc-commands.md` against it at build time.
- **bd**: 1.4.0. Notable 1.4.0 behavior vs 1.3.x: restores pre-upgrade audit events on
  first open (gastownhall/beads#7039), `bd --readonly serve` now refused outright, and
  legacy `BD_ACTOR` is a deprecated alias for `BEADS_ACTOR`. Schema era: v66 (1.3.0
  migration set); 1.4.0 has no new migration gate documented.
- Go module path is `github.com/steveyegge/beads` (repo mirrored at gastownhall/beads);
  `go install` variants: `CGO_ENABLED=0` (server-mode only) vs `CGO_ENABLED=1
  GOFLAGS=-tags=gms_pure_go` (embedded-capable). This matters if lambe imports it.

## Key Concepts — Pi RPC
- **Framing**: one JSON object per line, LF-terminated; split ONLY on LF (Go's bufio.Scanner
  is fine; Node readline is not). Read stdout continuously (backpressure stalls Pi); stdout
  is protocol, stderr is diagnostics (never parse as protocol).
- **Correlation**: optional string `id` per command; responses repeat it. Command handling
  is async — correlate by ID, not order. Malformed JSON → parse response with no ID.
- **prompt semantics**: `data.disposition` is `"started"|"queued"|"handled"`. `success:true`
  only means accepted; failures after acceptance arrive via events, never a second response.
  If agent is streaming, `prompt` REQUIRES `streamingBehavior` (`"steer"|"followUp"`) or it
  errors — lambe's driver must track `isStreaming` (from `get_state`/events) or always use
  the dedicated `steer`/`follow_up` commands.
- **Completion**: `agent_end` closes one low-level run (retries/compaction/steering may
  follow); `agent_settled {aborted}` means no automatic work remains. Subscribe listeners
  BEFORE sending prompt. `abort` waits-for-idle before responding.
- **Lifecycle/shutdown**: close stdin → orderly shutdown (give it time, then kill);
  SIGTERM/SIGHUP also orderly; second signal = immediate exit. Deadlines/hang detection are
  the client's job ("Clients must also handle … their own deadlines") — vendor guidance
  offers no built-in timeout; lambe needs its own watchdog (e.g. no events for N seconds →
  steer/abort → kill).
- **Session resume**: sessions at `~/.pi/agent/sessions/--<munged-cwd>--/<ts>_<id>.jsonl`
  (v3 tree format, `id`/`parentId` entries). Spawn with `--session-id <id>` (opens existing
  for cwd or creates), or `--session <path|id>` to resume by ID; `--session-dir` overrides
  storage. In-process: `switch_session {sessionPath}` / `fork {entryId}` / `clone` /
  `new_session {parentSession?}`; `get_entries {since}` gives a durable cursor for sync.
- **Useful extras**: `get_session_stats` (tokens/cost/contextUsage), `get_state`
  (isStreaming, sessionFile, pendingMessageCount), `bash` command with streamed
  `bash_execution_update`, `compact`, `set_auto_retry {enabled}`, `abort_retry`,
  `set_auto_compaction`. cwd is controlled by spawning Pi in the target dir (cwd drives
  config/resource discovery); env inherited from the child process.
- **Errors**: `success:false, error:<string>` per command. No typed error taxonomy —
  parse-free string handling; treat unknown commands/flags as capability probing risk.

## Key Concepts — bd 1.4.0
- **Claims/leases**: claim = `bd update <id> --claim` (atomic, idempotent, sets assignee +
  in_progress + lease) or `bd ready --claim --json` (first ready match). Release:
  `bd unclaim`. Keep-alive: `bd heartbeat <id>` (alias `hb`); only current owner; fails if
  reclaimed/closed → worker-stop signal. Leases are ephemeral, node-local, no Dolt commit.
  Reclaim: `bd reclaim [--older-than 10m] [--any-replica] [--id ...] [--label ...]`;
  default covers only leases THIS replica granted; run from supervisor at ~2× TTL.
  No documented config key for claim TTL (not in configuration.md's tables) — treat TTL as
  internal; heartbeat interval chosen empirically (< TTL). `claim.pools` config enables
  pool-alias claiming (database-only setting).
- **Guarded writes**: `bd update --if-assignee/--if-status` mismatch → exit code **13**
  (another actor won; don't retry same guard). Exit **14** = migration freeze ("retry
  later"). Branch on exit codes, not on JSON presence.
- **Defer**: `bd defer <id> [--until <date>] [--reason]` — with date = snooze (auto-wakes
  to open at next ready-front read), without = indefinite icebox. `bd create --defer` and
  `bd update --defer` also exist; JSON field is `defer_until`. There is NO `--overdue` flag.
- **Gates**: `bd gate create --type {human,timer,gh:run,gh:pr} --blocks <id> [--timeout]
  [--await-id]`; `bd gate check [--type] [--dry-run] [--escalate]` evaluates and
  auto-closes; `bd gate resolve <id> --reason`; `bd gate add-waiter <gate-id> <waiter>`;
  `bd mol ready --gated` discovers gate-resumed molecules. `bd show --json` carries derived
  `gated_by: [{id,type,reason}]` — read this instead of recomputing gate blocking.
- **Formulas/molecules**: `bd cook <formula> [--mode compile|runtime] [--var k=v] [--dry-run]
  [--persist]`; ephemeral protos are the default — `bd mol pour <proto> --var ... --dry-run`
  (persistent) and `bd mol wisp <proto>` (ephemeral); `bd mol burn/squash/bond` for
  lifecycle. All support `--dry-run`.
- **IDs/wisps**: `bd create --id bd-42` pins an explicit ID (must match DB prefix);
  `--ephemeral`/`--no-history`/`--storage-class {versioned,unversioned,ephemeral}`;
  `bd create --graph plan.json` creates an atomic graph (nodes may pin `id`, set
  `defer_until`, `metadata`, initial `status`). `types.retained` config lists types whose
  closed beads only `bd prune`/`bd purge --retain-ids` may delete — protects key-bearing
  agent/rig types from plain sweeps.
- **batch**: `bd batch [-f file] [--dry-run]` — one transaction, one DOLT_COMMIT; grammar:
  `close|update <id> k=v|create|dep add/remove`. Narrow subset (no claim/defer/gate).
  Exists to avoid per-invocation write amplification — batch loops that only
  close/update/deps.
- **serve/HTTP**: `bd serve --addr 127.0.0.1:PORT` (fixed port for mutual exclusion).
  OpenAPI at `/v0`; capability discovery `GET /v0/beads/context`. Liveness `/healthz`,
  readiness `/v0/beads/ready?limit=1`. Hooks and per-command auto-commit do NOT run;
  auth optional loopback via `--auth-token-file`. Note: `bd --readonly serve` refused.
- **Events journal**: OFF by default — must be enabled (`bd config set events-journal true`
  or `BD_EVENTS_JOURNAL=1`) at every writer, including `bd serve`. `GET
  /v0/beads/events?since=<seq>&limit=` (since required; limit 1..10000) and SSE
  `GET /v0/beads/events:watch?since=` (id = seq; heartbeats every 20s; max 48 streams →
  503 `events_watch_saturated`). Handle 410 `events_journal_truncated` (re-baseline),
  409 `events_journal_disabled`, and mid-stream `truncated` events. At-least-once; be
  idempotent on `seq`. Journal is per-replica, one branch, no sync/sql/compaction coverage.
- **JSON envelope**: `--json` is the stable flag (never `--format json`). Legacy (default
  until v2.0): object commands carry top-level `schema_version:1`, list commands emit a
  BARE ARRAY (no schema_version). Envelope mode: `BD_JSON_ENVELOPE=1` →
  `{"schema_version":1,"data":...}` (+`pagination` for truncated `bd ready`). Decide once
  and set BD_JSON_ENVELOPE=1 in lambe's env. Errors: JSON or plain text, stdout or stderr —
  parse defensively. `bd show --json` returns one array element per ID and carries
  `revision` (optimistic-concurrency token).

## Implementation Guide (decision inputs for lambe)
1. Pi driver: Go exec child, strict LF JSONL, ID-correlated map[pending]chan, event fan-out;
   completion = `agent_settled`; watchdog deadlines are ours (abort → grace → SIGKILL);
   resume via `--session-id` at spawn or `switch_session` on a live child.
2. bd adapter: prefer `bd serve` (one process, HTTP+SSE) over per-call exec for loops —
   per-command engine startup on embedded Dolt is the cost the batch/serve docs explicitly
   target. Use two-read pattern: `bd ready --json` / `bd show --json <id>` (with
   `revision` for guarded writes via `--if-status`/exit-13 semantics).
3. Supervisor loop: heartbeat on interval; `bd reclaim` on timer at ~2× TTL; events SSE for
   reactive wakes (gate close → `bd mol ready --gated`).

## Best Practices
- Set `BEADS_ACTOR` (not BD_ACTOR) to a stable lambe identity; per-worker actors for audit.
- Events journal: enable everywhere that writes; track checkpoint per server URL; handle
  409/410/truncated; poll rather than stream where latency is tolerable.
- Never give `--any-replica` to bare reclaim; scope it `--any-replica --id <id>`.
- For Pi: install event listeners before prompt; don't wait on `agent_settled` when
  disposition was `"handled"`.

## Common Issues
- `prompt` during streaming without `streamingBehavior` → error race in Pi driver.
- Reading Pi stdout with a Unicode-aware line reader corrupts the stream (U+2028/2029).
- bd list/ready JSON parsed as object in legacy mode breaks — it's a bare array.
- `bd ready --claim` skips pool-assigned issues (claim those by ID).
- Journal empty == journal caught-up; the 409 is the only discriminator.
- bd binary refuses a DB migrated past its schema — fleet upgrades are coordinated; lambe
  should run `bd doctor`/`bd version` checks at startup.

## References
- Pi RPC: github.com/earendil-works/pi → packages/coding-agent/docs/{rpc,rpc-commands,json,session-format,cli}.md; rpc-types.ts for TS protocol types.
- bd: beads.gascity.com llms.txt; cli-reference/{serve,reclaim,heartbeat,defer,gate,cook,mol,batch,create,update}.md; reference/{json-schema,events-journal,configuration}.md; getting-started/upgrading.md.
