# lambe local state API — wire contract v1

This document specifies version 1 of lambe's local HTTP state API: the
loopback surface a running daemon serves so operator apps — the SwiftUI
dashboard, scripts, future clients — can read supervised state without
touching the tracker, the live projection file, or any Go code. A client can
be implemented from this document plus the committed fixtures in
[`testdata/contract/v1/`](../../testdata/contract/v1/) alone.

The fixtures are generated and pinned by
[`internal/httpstate/contract_test.go`](../../internal/httpstate/contract_test.go):
any DTO drift fails CI, and the Swift tests decode the same files.

## Overview

- The surface is **opt-in**: with `stateapi.enable` unset or false the daemon
  serves nothing — no listener, no discovery file. Enabling it is an operator
  decision (see [Configuration](#configuration)).
- The listener binds a **loopback address only**. `stateapi.addr` defaults to
  `127.0.0.1:0` (ephemeral port); configuration validation accepts only the
  exact literal `127.0.0.1` in `host:port` form — `localhost`, `[::1]`, other
  `127.x.x.x` addresses, and a missing port are all rejected. There is no
  TLS: the surface is for same-machine, same-user clients.
- Every response — success and error — is a single JSON document with
  `Content-Type: application/json`, UTF-8, HTML characters unescaped, and a
  trailing newline. The wire encoding is not indented; the committed fixtures
  are indented for review.
- Every response carries a `daemon` identity block and a `schema_version`.
- Every list-valued field is decoded defensively by clients: JSON `null` and
  `[]` are equivalent for every list in this contract. (`/api/v1/state`
  serializes empty sections as `null`; `/api/v1/queue` and `/api/v1/history`
  serialize empty lists as `[]`. Clients must accept both everywhere.)
- Timestamps are RFC 3339 in UTC (`2026-11-19T12:00:00Z`). Human age strings
  (`last_event_age`) always have an RFC 3339 machine counterpart
  (`last_event_at`).

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `stateapi.enable` | `false` | Serve the API on loopback. Off means nothing listens. |
| `stateapi.addr` | `127.0.0.1:0` | Host:port to bind; port `0` selects an ephemeral port. The host must be the exact literal `127.0.0.1` — `localhost`, `[::1]`, and other 127.x.x.x addresses are rejected. |

Environment overrides follow the documented flattening rule:
`LAMBE_STATEAPI_ENABLE` and `LAMBE_STATEAPI_ADDR`.

## Discovery

An enabled daemon publishes a per-instance descriptor after it acquires its
instance lock, and removes it during shutdown before the lock is released.

### Descriptor file

- Path: `<state-dir>/live/stateapi-<instance>.json`
- `<instance>` is the daemon's instance name — the instance lock name without
  its `.lock` suffix (form `lambe-<hex>`). The name embeds a hash of the
  configured tracker paths. **Clients must never reconstruct this name**;
  they enumerate the directory instead (below).
- Mode `0600` inside the `0700` `live` directory: the file carries the bearer
  token, so it is readable only by the daemon's user.
- Written atomically (temp file + rename): a reader never sees partial
  content. A descriptor may be briefly absent during daemon startup, and a
  file left by a crashed daemon may name a dead process — both are normal
  discovery outcomes, not errors.
- Clients never delete descriptors; the owning daemon removes its own file.

### Descriptor schema (version 1)

The descriptor schema version is independent of the HTTP wire schema.
Example (committed as [`descriptor.json`](../../testdata/contract/v1/descriptor.json)):

```json
{
  "schema_version": 1,
  "instance": "lambe-1a2b3c4d5e6f",
  "addr": "127.0.0.1:53412",
  "bearer_token": "descriptor-fixture-token-not-a-secret",
  "pid": 4242,
  "identity": "darwin:boot:1:2",
  "version": "v0.9.0",
  "config_path": "/Users/operator/lambe/lambe.yaml",
  "started_at": "2026-11-19T11:30:00Z"
}
```

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Descriptor schema version. Skip entries with an unknown value. |
| `instance` | string | always | Instance name, as in the file name. |
| `addr` | string | always | Bound `host:port` the API answers on. |
| `bearer_token` | string | always | Bearer token for every route (below). Secret: never log or embed it. |
| `pid` | number | always | Daemon process id. |
| `identity` | string | optional | Process-incarnation identity; absent when the platform cannot provide one. |
| `version` | string | always | Daemon build version. |
| `config_path` | string | optional | Configuration file the daemon loaded; absent when it ran without one. |
| `started_at` | string | always | Daemon start time, RFC 3339 UTC. |

### Discovery algorithm

1. Enumerate `stateapi-*.json` in `<state-dir>/live/`. Never rebuild the
   instance name or any hash from configuration.
2. Read each candidate with a bounded timeout and a small retry (a descriptor
   can appear while you are enumerating, and a daemon may be restarting).
   Skip unreadable or malformed files.
3. Skip stale entries: a process whose `pid` no longer exists, or whose
   process identity does not match the descriptor's `identity`. Pids are
   recyclable; the identity check is what distinguishes a restart from an
   impostor.
4. Skip entries whose `addr` host is not literally `127.0.0.1` before ever
   reading the token out of the file (see [Authentication](#authentication)).
5. Select the entry whose `instance` (or `config_path`) matches the daemon
   you mean to talk to. Multiple daemons under one state directory publish
   multiple files — a merged document never exists.
6. On repeated connection failure, re-enumerate: a launchd/systemd restart
   produces a new pid, a new ephemeral port, and a new per-start token.

## Authentication

- **Every route requires the bearer token — including `POST /api/v1/refresh`.**
  Send `Authorization: Bearer <token>` on all requests.
- The token is a fresh random value per daemon start (32 bytes from
  `crypto/rand`, base64url-encoded). It is delivered only in the 0600
  descriptor — never in logs, configuration summaries, or error strings.
- A missing or wrong token is answered with the same 401 envelope on every
  route; the two cases are indistinguishable by design. 401 responses carry
  `WWW-Authenticate: Bearer realm="lambe-stateapi"`.
- A request whose `Host` header does not name the bound loopback host is
  rejected with 403 `bad_host` before any handler runs, even with a valid
  token. (The port on the Host header is ignored; the host must match.)
- No `Access-Control-*` header is ever emitted, on any response, including
  OPTIONS. The surface serves same-origin loopback clients only.
- Token comparison is constant time (`crypto/subtle`) server-side.

### Client duties (non-negotiable hardening)

1. Validate that the descriptor's `addr` host is literally `127.0.0.1`
   **before** attaching the token to any request.
2. Never follow redirects: a 3xx response is an error, not a hop.
3. Never disclose the token — not in logs, not in error values, not in
   user-facing diagnostics.
4. Use bounded request timeouts; fail fast rather than hanging.

## Routes

| Method | Path | Response DTO | Success status |
| --- | --- | --- | --- |
| GET | `/api/v1/state` | `SnapshotResponse` | 200 |
| GET | `/api/v1/queue` | `QueueResponse` | 200 |
| GET | `/api/v1/history` | `HistoryResponse` | 200 |
| GET | `/api/v1/{issue}` | `IssueResponse` | 200 |
| POST | `/api/v1/refresh` | `RefreshResponse` | 202 |

### Route precedence

The literal path names `state`, `queue`, `history`, and `refresh` under
`/api/v1/` are **reserved**: they match their own routes before the
`/api/v1/{issue}` wildcard. An issue whose identifier collides with a
reserved name — `state`, for example — is shadowed: `GET /api/v1/state`
always returns the snapshot, never that issue's detail. This is pinned by
test.

Wrong methods on known endpoints answer 405 with the error envelope (`GET
/api/v1/refresh` and `POST` on any GET route included). Every unmatched
request answers 404 with the error envelope — never the net/http plaintext
page. The two 404 shapes differ by source: a single-segment identifier under
`/api/v1/` that names no issue reaches the wildcard and answers
`unknown issue "<id>"`; paths that match no route at all — an empty
identifier (`/api/v1/`), or a multi-segment path such as `/api/v1/a/b` —
answer the catch-all `unknown route`.

### `POST /api/v1/refresh` is a mutating operator action

The route is **not** an advisory refresh hint. An accepted request (202) asks
the daemon to run one full supervision cycle in the background: hub pull →
supervisor tick → flush queued writes → **hub push** → report refresh. That
cycle can write to the hub — heartbeats, status writes, gate bookkeeping.

Guidance:

- Dashboards and other read-only clients must never call it. Their refresh is
  GET-only (manual or on a poll cadence); the daemon refreshes its own
  projections on its tick.
- Overlapping refresh requests collapse into one in-flight cycle, and the
  response returns immediately (202 `{"accepted":true}`); it does not wait
  for the cycle.
- Any future client that treats refresh as an operator action needs an
  explicit human decision to ship it — it is a write path, not a read
  optimization.

## DTO reference

Rules that apply to every DTO:

- **Absent means unknown.** Optional fields use JSON `omitempty`: the server
  omits a field when its value is genuinely unknown, never as an empty-string
  placeholder. Clients must treat *absent* as "unknown" — not as `""`, `0`,
  or any other meaningful value — and *present* as known, even when the value
  is empty text.
- Unknown fields must be ignored by clients (see
  [Evolution](#versioning-and-evolution)).
- The `daemon` block is identical across concurrent responses from one daemon
  life; a change in `pid` or `identity` means the daemon restarted.

### `DaemonDTO`

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `pid` | number | always | Serving process id. |
| `identity` | string | optional | Process incarnation; absent when the platform cannot provide one. |
| `machine` | string | optional | Configured dispatch identity. |
| `version` | string | optional | Daemon build version. |
| `config_path` | string | optional | Configuration file loaded; absent when the daemon ran without one. |

### `SnapshotResponse` — `GET /api/v1/state`

The full supervised state. Fixture:
[`snapshot.json`](../../testdata/contract/v1/snapshot.json).

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `daemon` | object | always | `DaemonDTO`. |
| `runs` | list | always, `null` when empty | Supervised runs in report order. |
| `deferred_until` | string | optional | When the supervisor deferred its next action, RFC 3339; absent when not deferred. |
| `wake_at` | string | optional | When the supervisor will wake to re-evaluate; absent when unset. |
| `needs_human` | list | always, `null` when empty | Items awaiting human attention. |
| `gate_rejections` | list | always, `null` when empty | Recent gate rejections, oldest first. |
| `runs_as_of` | string | optional | When the runs section was read (serve time on a live daemon). |
| `needs_human_as_of` | string | optional | When the needs-human section was last read from the tracker; absent before the first read. |
| `gate_rejections_as_of` | string | optional | When the gate-rejection section was last read; absent before the first read. |

`RunDTO`:

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `issue` | string | always | Issue identifier the run works on. |
| `session` | string | optional | Harness session; absent when none. |
| `attempt` | number | always | Attempt number, starting at 1. |
| `state` | string | always | Coarse state (see [Vocabularies](#state-and-phase-vocabularies)). |
| `last_event_age` | string | always | Human age of the last observed event (`"30s"`, `"n/a"` when none observed). |
| `last_event_at` | string | optional | RFC 3339 counterpart of `last_event_age`; absent when no event observed. |
| `phase` | string | optional | Operator-facing phase; absent when not yet observed. |

`NeedsHumanDTO`:

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `issue` | string | always | Issue identifier. |
| `reason` | string | always | Why a human is needed: the escalation's recorded reason, or the last gate output summary when no recorded reason exists. Never a placeholder. |
| `detail` | string | optional | Human-readable explanation. |
| `since` | string | optional | When the item was raised, RFC 3339; absent when the record carries no time. |

`GateRejectionDTO`:

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `issue` | string | always | Issue identifier. |
| `gate` | string | optional | Gate kind (`"gh:pr"`, `"shell"`, …) — present only when the source record carries it. Absent means unknown; it is never guessed. |
| `reason` | string | always | Rejection reason. |
| `at` | string | optional | When the rejection was observed, RFC 3339; absent when the record carries no time. |

### `IssueResponse` — `GET /api/v1/{issue}`

One issue's live detail. Fixture:
[`issue.json`](../../testdata/contract/v1/issue.json). Unknown identifiers
answer 404 `not_found`.

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `daemon` | object | always | `DaemonDTO`. |
| `issue` | string | always | The identifier requested. |
| `state` | string | always | Coarse state. |
| `attempt` | number | always | Current attempt number. |
| `session` | string | optional | Active harness session. |
| `last_event_age` | string | always | As in `RunDTO`. |
| `last_event_at` | string | optional | RFC 3339 counterpart. |
| `summary` | string | optional | Short human-readable status. |
| `phase` | string | optional | Operator phase; absent when not observed. |
| `turn` | number | optional | Current conversation turn within the attempt; absent when 0. |
| `thread` | string | optional | Harness conversation the run continues. |
| `workspace` | string | optional | Run's working directory. |
| `workflow` | string | optional | Named workflow the run executes. |
| `wake_at` | string | optional | When a deferred run wakes; absent unless deferred. |
| `last_gate` | string | optional | Bounded preview of the last gate output. |
| `tail` | string | optional | At most 4 KiB of the run's most recent output, cut on a UTF-8 boundary. Full output lives in artifacts, never here. |
| `tail_truncated` | boolean | optional | `true` when the tail was cut at the bound; absent (false) otherwise. |

### `QueueResponse` — `GET /api/v1/queue`

Readiness rows for every configured label. Fixture:
[`queue.json`](../../testdata/contract/v1/queue.json).

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `daemon` | object | always | `DaemonDTO`. |
| `as_of` | string | optional | When the readiness projection was read from the tracker; absent before the first read. Rows are served from that cached read, never re-read per request. |
| `rows` | list | always, `[]` when empty | Ready rows first; blocked rows carry reason and blockers. |

`QueueRowDTO`:

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `id` | string | always | Issue identifier. |
| `title` | string | optional | Issue title. |
| `reason` | string | optional | Why the task is ready or blocked. |
| `blocked` | boolean | always | `true` when the task cannot be dispatched now. |
| `blockers` | list | optional | Blocking issues; absent when none. |

`QueueBlockerDTO`: `id` (string, always) and `title` (string, optional).

### `HistoryResponse` — `GET /api/v1/history`

Finished and archived attempts. Fixture:
[`history.json`](../../testdata/contract/v1/history.json).

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `daemon` | object | always | `DaemonDTO`. |
| `as_of` | string | optional | When the merge was built. |
| `runs` | list | always, `[]` when empty | Newest first, bounded at **100** entries. |

Semantics:

- **Ordering:** newest first by `updated_at`.
- **Bound:** at most 100 entries; older history lives in the tracker, not
  here.
- **Merge:** terminal rows still in the live projection (phase `stopped: …`),
  attempts archived across daemon restarts (phase `interrupted`), and
  terminal registry rows that never got a projection record (coarse `state`
  and `outcome`, no phase) are merged and deduplicated per `issue`+`attempt`.
  A daemon restart preserves history; it does not reset it.

`HistoryRunDTO`:

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `issue` | string | always | Issue identifier. |
| `title` | string | optional | Issue title, when the projection carried it. |
| `phase` | string | optional | Operator phase the attempt ended in (`"stopped: gate red"`, `"interrupted"`, …); absent for registry-terminal rows. |
| `state` | string | optional | Coarse state, when known. |
| `attempt` | number | always | Attempt number. |
| `outcome` | string | optional | How the run ended, when the record carries one. |
| `updated_at` | string | optional | When the attempt was last observed; the ordering key. |

### `RefreshResponse` — `POST /api/v1/refresh`

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `daemon` | object | always | `DaemonDTO`. |
| `accepted` | boolean | always | `true` — the request was accepted for processing. |

### `ErrorResponse` — every non-2xx response

Fixture: [`error.json`](../../testdata/contract/v1/error.json).

| Field | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `schema_version` | number | always | Wire schema version (1). |
| `error` | string | always | Machine-stable code (below). |
| `message` | string | always | Human-readable explanation; never quotes credentials. |

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | Malformed request reaching a handler (defensive; the mux routes empty identifiers to the 404 catch-all). |
| 401 | `unauthorized` | Missing or invalid bearer token — every route, refresh included. |
| 403 | `bad_host` | `Host` header does not name the bound loopback host. |
| 404 | `not_found` | Unknown issue under `/api/v1/{issue}` (`unknown issue "<id>"`), or any path matching no route (`unknown route`). |
| 405 | `method_not_allowed` | Wrong method on a known endpoint. |

Exact envelopes:

```json
{"schema_version":1,"error":"unauthorized","message":"missing or invalid bearer token"}
{"schema_version":1,"error":"bad_host","message":"request host does not match the bound loopback address"}
{"schema_version":1,"error":"not_found","message":"unknown issue \"T-404\""}
{"schema_version":1,"error":"not_found","message":"unknown route"}
{"schema_version":1,"error":"method_not_allowed","message":"wrong method for this endpoint"}
{"schema_version":1,"error":"bad_request","message":"missing issue identifier"}
```

## State and phase vocabularies

Two vocabularies are served side by side; they answer different questions and
must not be collapsed:

- **`state` is the machine contract**: the coarse enum aligned with the
  Symphony §13.7 / MCP vocabulary. Values: `pending`, `running`, `deferred`,
  `settled`, `failed`, `canceled`. This set is stable; machines branch on it.
- **`phase` is the operator display**: richer live-projection strings.
  Values served today: `claiming`, `preparing`, `running`, `retry`, `gating`,
  `deferred`, `interrupted`, and terminal `stopped: <reason>` strings
  (`stopped: gate green`, `stopped: gate red`, `stopped: gate escalation`,
  and other stop reasons). The set is open —
  new phases may appear without a schema bump.

Typical pairings (illustrative, not a normative transition table):

| state | phases typically seen |
| --- | --- |
| `pending` | `claiming`, `preparing` |
| `running` | `running`, `gating`, `retry` |
| `deferred` | `deferred` |
| `settled` | `stopped: gate green` |
| `failed` | `stopped: gate red`, `stopped: gate escalation` |
| (archived) | `interrupted` — a run cut off by a daemon restart |

Client rules: render `phase` when present and fall back to `state`; treat
unknown `phase` values as neutral display text; never branch logic on
`phase`.

## Freshness

Every tracker-derived section carries an `as_of` stamp so a client can show
how fresh what it renders is:

| Section | Stamp | Freshness behavior |
| --- | --- | --- |
| snapshot runs | `runs_as_of` | Read per request from the in-memory registry — fresh on every poll. |
| snapshot needs-human | `needs_human_as_of` | Cached tracker read, refreshed every few supervisor ticks (about a minute at the default 10s tick) and on an accepted refresh; absent before the first read. |
| snapshot gate rejections | `gate_rejections_as_of` | Read per request from the live projection. |
| queue | `as_of` | Cached canonical tracker read shared with the status projection; stale by the report cadence is normal. Absent before the first read. |
| history | `as_of` | Merge built per request from the live projection and registry. |
| issue detail | (none) | Live in-memory read; `last_event_at` is the freshness anchor. |

An absent `as_of` means "not read yet", not "no data" — serve the section as
unknown rather than empty. Clients should label stale sections (a degraded
banner with the `as_of` value) and never silently mix sections whose
freshness differs sharply.

## Versioning and evolution

- `schema_version` is `1` on every response envelope in this contract.
- **Evolution is additive-only at a given version**: new optional
  (`omitempty`) fields, new `phase` values, new list entries. Existing field
  names and the meanings documented here do not change within a version.
  Correcting a wrong *value* (a placeholder reason, an unset timestamp) is
  not a shape break.
- **Bump policy:** `schema_version` is bumped only for a breaking change — a
  field removed or renamed, a type or semantic reversed, a route removed.
- **Higher-version behavior (clients):** when a response's `schema_version`
  is greater than the client supports, the client shows a "daemon newer than
  app" state. It must not fail to decode: ignore unknown fields, take the
  fields it knows, and degrade visibly rather than crash.
- **Fixtures:** `testdata/contract/v1/*.json` are canonical examples pinned
  by the Go contract test (byte-stable, decode-checked) and decoded by the
  Swift tests. Regenerate with
  `go test ./internal/httpstate -run TestContract -update` after an
  additive change; a committed-fixture mismatch fails CI.

## A minimal client, in pseudocode

```
descs = enumerate(<state-dir>/live, "stateapi-*.json")   // bounded read, small retry
for d in descs:
    if parse(d) fails or d.schema_version != 1: continue
    if host(d.addr) != "127.0.0.1": continue             // before touching the token
    if not alive(d.pid) or identity(d.pid) != d.identity: continue
    selected = d                                          // match by instance
request(base = "http://" + selected.addr):
    headers  = {Authorization: "Bearer " + selected.bearer_token}
    redirects = refused
    timeout   = bounded
GET /api/v1/state   -> SnapshotResponse
GET /api/v1/queue   -> QueueResponse
GET /api/v1/history -> HistoryResponse
GET /api/v1/<id>    -> IssueResponse | 404
# POST /api/v1/refresh is an operator action; read-only clients never call it.
```
