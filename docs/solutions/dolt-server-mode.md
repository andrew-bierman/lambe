---
title: Running the Beads hub on a Dolt sql-server (runbook)
date: 2026-11-22
category: operations
module: Beads hub (agent-coordination), bd, internal/tracker
problem_type: performance
severity: high
tags: [dolt, beads, server-mode, launchd, embedded, concurrency, migration, runbook]
---

# Running the Beads hub on a Dolt sql-server

Status: evaluated on APFS clones of the production hub, **not migrated**. Every
number below comes from clones in `~/Code/_scratch/dolt-server`; production
(`~/Code/agent-coordination`, `~/.local/state/lambe`, the running daemon,
LaunchAgents) was only read.

## Problem

The hub is an embedded Dolt database (`.beads/embeddeddolt/ac`, about 760 MB,
2.5k issues, about 400 attempt records, about 80 claims). Every `bd` call opens
the engine in-process, so each call costs 1-2 s of CPU solo, and concurrent
callers serialize behind `.beads/*.gate.lock`. Under fleet load the Lambe
daemon's `bd` calls hit their 60 s timeout (see
[orphan-claim-heartbeats](orphan-claim-heartbeats.md)). Server mode keeps one
engine resident, so each `bd` call is a cheap SQL round trip.

## Recommendation

**Go**, in the shape "one loopback-only `dolt sql-server` on this Mac under
launchd; the other machines change nothing". Preconditions (all in the
checklist below): a real `dolt` binary (not the mise shim), a Dolt identity for
CLI-routed sync, `dolt.auto-start: false`, and a scheduled `bd backup sync`
(auto-backup is off in server mode). Moving the other machines from replicas to
clients of this server is a separate, optional decision
([MULTI-MACHINE.md](../MULTI-MACHINE.md)); this migration does not require it.

## Measurements

Same data, same host (12 cores, shared with other agents; `uptime` load average
was 6.9 at the start of the server runs and 14-16 during the embedded runs, part
of that self-inflicted by 16 parallel embedded opens). Seconds, `p50 / p95`, per
`bd` process wall time, 0 failures or timeouts in every cell. Runs per cell: 8 to
32, so treat p95 as indicative. Harness: `bench.py` in the scratch directory
(threads spawning `bd`, `BEADS_ACTOR` per worker for heartbeats).

| op (parallel) | embedded 1 | server 1 | embedded 4 | server 4 | embedded 16 | server 16 |
|---|---|---|---|---|---|---|
| `list --json --limit 1` | 1.23 / 1.39 | 0.11 / 0.12 | 1.86 / 11.5 | 0.13 / 0.15 | 14.4 / 34.0 | 0.24 / 0.29 |
| `list --json --ready` | 1.66 / 1.77 | 0.11 / 0.12 | 3.05 / 16.1 | 0.12 / 0.14 | 19.7 / 39.7 | 0.26 / 0.30 |
| `show <id> --json` | 1.40 / 1.68 | 0.09 / 0.10 | 2.89 / 18.3 | 0.11 / 0.12 | 11.7 / 32.6 | 0.20 / 0.26 |
| `list --metadata-field` x2 | 1.34 / 1.48 | 0.11 / 0.11 | 2.11 / 14.8 | 0.12 / 0.13 | 13.0 / 29.0 | 0.26 / 0.36 |
| `heartbeat <id>` | 0.95 / 1.01 | 0.09 / 0.09 | 1.90 / 8.3 | 0.12 / 0.14 | 11.6 / 26.4 | 0.23 / 0.33 |
| `create --id` | 2.08 / 3.06 | 0.15 / 0.16 | 6.04 / 15.9 | 0.31 / 0.34 | 23.5 / 44.6 | 1.01 / 1.23 |
| `comments add` | 1.24 / 1.30 | 0.17 / 0.20 | 1.88 / 11.8 | 0.23 / 0.24 | 15.7 / 31.0 | 0.52 / 0.76 |

Max at 16 parallel: embedded 31-49 s, server 0.3-1.3 s. Parallel 8 sits between
the two columns (embedded p50 4.6-10.9 s, server 0.14-0.50 s).

With four busy-loop processes added (parallel 4; load average 14-17, shared
host): server list 0.17 / 0.20, metadata query 0.19 / 0.21, heartbeat 0.21 /
0.22, create 0.42 / 0.43; embedded list 3.2 / 16.1, metadata 3.7 / 10.7,
heartbeat 4.0 / 10.2, create 8.6 / 26.5.

The 13 s metadata-field query seen on production was contention, not the query:
solo on the embedded clone it took 1.3 s. Server mode removes the contention.
Idle `dolt sql-server`: about 370 MB RSS, 0 % CPU.

## Correctness (server-mode clone)

- **Data integrity.** Moving the embedded repo directory under a server gave a
  byte-identical issue set: 2525 issues, per-status counts, 6965 labels, 26
  dependencies, 399 attempts, and the same row count in all 29 tables; the
  JSON fingerprint
  `bd list --all --json --limit 0 | jq -S -c 'sort_by(.id)' | shasum -a 256`
  matched exactly, and also matches an embedded replica that holds the same
  content, so it is a valid cross-mode check.
- **Contested claim.** 16 processes ran `bd update ID --claim` as 16 actors on
  one open issue: exactly one rc 0, 15 refused, assignee equals the winner.
- **Leases.** Owner heartbeat rc 0; non-owner heartbeat rc 1; `bd reclaim
  --older-than 0s` left a live lease alone and reverted an expired one to open,
  after which the old owner's heartbeat is refused. Leases live in the ignored
  `leases` table of the one server (default TTL 5 min) and are **not** synced by
  `bd dolt push/pull`.
- **Concurrent writers.** 12 processes from two working directories using
  `BEADS_DIR` and 12 actors each created an issue and a comment: 12/12 present,
  12 comments, no errors.
- **Crash safety.** `kill -9` of the server: launchd `KeepAlive` brought a new
  one up in 2 s; fingerprint unchanged.
- **Server down.** With `dolt.auto-start: false`, `bd` fails in under 0.1 s and
  starts nothing. With auto-start left at its default `bd` tries to spawn its
  own server on the same data directory (it failed in the test only because the
  mise shim is broken, see below). That is a data-corruption hazard, so
  `auto-start: false` is mandatory.
- **Mixed clients.** The pinned `~/Code/agent-coordination/.local/beads-1.3.2/bd`
  (used by `bin/coord`) works against the server alongside bd 1.3.4.

## Sync (Dolt remotes)

All tested against local remotes (a `file://` Dolt remote and a `git+file://`
bare git repo seeded with one commit, the same shape as the GitHub remote).

- `bd dolt push`, `bd dolt pull` and `bd sync` work from a server-mode clone.
  `file://` remotes run through the server (0.6-4 s). `git+...` remotes are
  routed through the **`dolt` CLI on PATH** and need a Dolt identity
  (`dolt config --global --add user.name/user.email`, in the `HOME` or
  `DOLT_ROOT_PATH` that the caller uses; `bin/coord` sets
  `DOLT_ROOT_PATH=.local/dolt-home`). Without a binary or identity, sync fails
  with `empty ident name not allowed` or the mise-shim error. Embedded mode needs
  neither.
- `git+file` sync of the 760 MB database took 25-60 s wall (4 s CPU) on the
  loaded host, similar for embedded (25 s).
- **Interop.** An embedded clone pulled the server's push, merged a diverged
  history cleanly, pushed back, and the server pulled it: identical issue sets
  afterwards (same fingerprint on the server clone after `bd sync` and on an
  embedded replica after `bd dolt pull` from the same git remote).
- **Conflicts.** Same issue edited on both sides: the pull auto-merged with a
  notice `title, updated_at settled last-write-wins (the older side's edit was
  superseded)` and exit 0. Sync never force-pushes; `bd sync` halts on conflicts
  it cannot resolve.
- **Leases do not travel.** After a pull, the pulled-in claimed issues have
  assignee and status but no `lease_expires_at` on the receiving side. This is
  already true between embedded replicas today.
- Remote name: bd always pushes to the Dolt remote named `origin`. Server mode
  needs the remote registered through `bd dolt remote add` (the production
  database already has `origin = git+https://github.com/andrew-bierman/agent-coordination.git`
  inside its repo state, and it moves with the data).

## Lambe compatibility

Built `~/Code/lambe` master (v0.10.2-dev+4c2a2aa) with a private `GOCACHE`; ran
an isolated daemon (own state dir, config, workspace root, actor
`lambe@dolt-server-test`, label-scoped workflow) against the server-mode clone.

- `lambe doctor`: 12 ok, 0 fail, 0.06 s (the one warning was the production
  launchd unit pointing at a different config, expected).
- Dispatch to completion with the repo's scriptable `fakeagent e2e` as the ACP
  agent and a `test -f done.txt` gate: issue claimed 10 s after daemon start (one
  10 s tick), agent spawned 2 s later, gate green and issue closed 3 s after
  that, attempt record `ac-att-ac-lfake1-1` written with pgid and process
  identity metadata. No `ERROR` log lines.
- Restart recovery in server mode: the earlier stalled run (`ac-ldemo1`) was
  adopted by the next daemon life (`session recovered`) and completed.
- The shipped demo workflow (opencode agent) could not be completed: the
  opencode account hit its weekly usage limit (`OpenCode Go weekly usage limit
  reached`). Dispatch, claim, attempt record, spawn and the retry loop all
  worked in server mode; the agent turn itself is not a hub-mode question.

## Prerequisites

1. `dolt` 2.6.0 or newer as a real binary on the PATH that launchd gives
   Lambe (`~/.local/bin:/opt/homebrew/bin:...`). Tested with 2.7.5. The mise shim
   `~/.local/share/mise/shims/dolt` fails outside a mise project
   (`No version is set for shim: dolt`), which breaks CLI-routed sync and
   bd's server start.
   ```sh
   install -m 755 "$(mise where aqua:dolthub/dolt)/dolt-darwin-arm64/bin/dolt" ~/.local/bin/dolt
   dolt version                       # >= 2.6.0
   dolt config --global --add user.name  "Andrew Bierman"
   dolt config --global --add user.email "<your git email>"
   # bin/coord uses its own DOLT_ROOT_PATH; give it the same identity:
   DOLT_ROOT_PATH=~/Code/agent-coordination/.local/dolt-home \
     dolt config --global --add user.name "Andrew Bierman"
   DOLT_ROOT_PATH=~/Code/agent-coordination/.local/dolt-home \
     dolt config --global --add user.email "<your git email>"
   ```
2. A free loopback port. This runbook uses 3418 (not Dolt's 3306 or bd's 3307).
3. A maintenance window of about 10 minutes with the fleet told to pause.

## Migration: production hub on this Mac

All paths assume `HUB=~/Code/agent-coordination`. The steps were written from
clone tests and then run once against the real hub; see the production record
below, and prefer [`migrate-hub.sh`](dolt-server-mode/migrate-hub.sh), which adds
a writer fence and rollback to this sequence.

```sh
HUB=~/Code/agent-coordination; cd "$HUB"; export BEADS_DIR="$HUB/.beads"

# 1. Quiesce. Stop every local writer (Lambe first), then confirm nothing holds the engine.
launchctl bootout gui/$(id -u)/com.lambe.daemon
pgrep -fl 'bd ' ; lsof .beads/embeddeddolt.gate.lock .beads/dolt.gate.lock   # expect nothing

# 2. Baseline fingerprint and a final embedded backup.
bd list --all --json --limit 0 | jq -S -c 'sort_by(.id)' | shasum -a 256 | tee /tmp/ac-before.sha
bd backup sync

# 3. Snapshot (APFS clone, instant; keep outside .beads). Safe only while no bd runs.
mkdir -p ~/Code/_backups && cp -cR "$HUB/.beads" ~/Code/_backups/ac-beads-pre-server-$(date +%Y%m%d-%H%M)

# 4. Copy (not move) the repo under the server's data dir. The embedded copy stays as the rollback base.
mkdir -p .beads/dolt && cp -cR .beads/embeddeddolt/ac .beads/dolt/ac

# 5. Install and start the server (plist below), then set the root password IMMEDIATELY
#    (loopback only; a fresh data dir starts with a passwordless root until this runs).
mkdir -p ~/.local/state/dolt-hub
# write ~/Library/LaunchAgents/local.agent-coordination.dolt.plist from the XML in "launchd" below, then:
plutil -lint ~/Library/LaunchAgents/local.agent-coordination.dolt.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/local.agent-coordination.dolt.plist
PW=$(openssl rand -hex 16)
dolt --host 127.0.0.1 --port 3418 --no-tls -u root -p "" sql -q "ALTER USER 'root'@'localhost' IDENTIFIED BY '$PW'"
mkdir -p ~/.config/beads && umask 077 && printf '[127.0.0.1:3418]\npassword = %s\n' "$PW" > ~/.config/beads/credentials
chmod 600 ~/.config/beads/credentials
# Offline `dolt sql ALTER USER` in the data dir did NOT take effect on this dolt version; use the live server.

# 6. Flip the hub to server mode. metadata.json decides the mode (config.yaml `dolt.mode` is ignored).
python3 - <<'PY'
import json
p='.beads/metadata.json'; m=json.load(open(p))
m['dolt_mode']='server'; m['dolt_server_host']='127.0.0.1'
json.dump(m,open(p,'w'),indent=2)
PY
# config.yaml: under the existing `dolt:` key add (4-space indent, like the file):
#     port: 3418
#     auto-start: false       # MANDATORY: otherwise bd may spawn a second server on the same data
$EDITOR .beads/config.yaml

# 7. Verify, then start the daemon.
bd dolt show                                   # Host 127.0.0.1, Port 3418, "Server connection OK"
bd list --all --json --limit 0 | jq -S -c 'sort_by(.id)' | shasum -a 256 | diff - /tmp/ac-before.sha && echo IDENTICAL
time bd list --json --limit 1                  # expect ~0.1-0.3 s
mv .beads/embeddeddolt .beads/embeddeddolt.pre-server   # (rollback renames it again) so nothing can open the stale copy by accident
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.lambe.daemon.plist
lambe doctor -config ~/Code/agent-coordination/lambe.yaml
```

Every client on the Mac (agents, `bin/coord`, Lambe) finds the mode from
`.beads/metadata.json` and the password from `~/.config/beads/credentials`; no
plist or `runtime.local.sh` edits are needed. Do not set `BEADS_DOLT_SERVER_PORT`
in agent environments; it is unnecessary here.

Do not `cp -cR` a `.beads` directory whose server is running; the clone can be
torn. Use `bd backup sync` or stop the server first.

### launchd: `dolt sql-server`

Save as `~/Library/LaunchAgents/local.agent-coordination.dolt.plist`. Verified
under a scratch label: starts at load, restarts in about 2 s after `kill -9`, the
password survives restarts, `bootout` stops it cleanly in a few seconds.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>local.agent-coordination.dolt</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/andrewbierman/.local/bin/dolt</string>
    <string>sql-server</string>
    <string>--host</string><string>127.0.0.1</string>
    <string>--port</string><string>3418</string>
    <string>--data-dir</string><string>/Users/andrewbierman/Code/agent-coordination/.beads/dolt</string>
    <string>--max-connections</string><string>200</string>
    <string>--loglevel</string><string>info</string>
  </array>
  <key>WorkingDirectory</key><string>/Users/andrewbierman/Code/agent-coordination/.beads/dolt</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ExitTimeOut</key><integer>60</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/andrewbierman/.local/state/dolt-hub/dolt.out.log</string>
  <key>StandardErrorPath</key><string>/Users/andrewbierman/.local/state/dolt-hub/dolt.err.log</string>
</dict>
</plist>
```

The server binds to `127.0.0.1` only; `lsof -nP -iTCP:3418`
should show exactly one `127.0.0.1:3418` listener. Dolt does not read
`user`/`password` from its YAML config any more; passwords live in the data
dir's `.doltcfg/privileges.db`, so **back up `.beads/dolt/.doltcfg` with the
data** (it is inside `.beads/dolt`, so a clone of `.beads` includes it).

Order matters at boot: Lambe's `KeepAlive` will retry failing `bd` calls until the
server answers, so there is no hard dependency, but start the server first when
doing this by hand. The hub dir inside `~/Code` stays the single home, per the
workspace policy.

## Production migration record (2026-11-22)

The runbook was written from clone tests. It was then run once against the real
hub on the owner's Mac, and these are the differences from the clone runs.

- **Fencing writers.** Other agents kept writing to the hub during the window, so
  stopping Lambe alone was not enough. [`migrate-hub.sh`](dolt-server-mode/migrate-hub.sh)
  holds `bd`'s own lock (`.beads/embeddeddolt.gate.lock`, via
  [`holdlock.py`](dolt-server-mode/holdlock.py)) for the whole critical section.
  While it is held, `bd` fails fast instead of writing, so no late write can land in
  the old embedded copy; callers see a short error and retry. Verified before use
  that `bd list` exits non-zero under the held lock rather than blocking.
- **Critical section:** snapshot clone, copy, start the server, set the password,
  compare the server's head commit hash and issue count against the snapshot (no
  `bd` needed, since it is fenced), flip `metadata.json` and `config.yaml`, release.
  It took about 8 seconds. Any failed check rolls back to embedded.
- **Result:** head hash and issue count (2540) identical to the snapshot. `bd`
  calls dropped from 1 to 13 s (or the daemon's 60 s timeout) to 0.24 to 0.42 s.
  Lambe came up in about 40 s instead of 6+ minutes and dispatched the next issue
  on its first tick.
- **Backup destination.** After the flip `bd backup status` reported no
  destination, although the server's copy still had the `backup_export` remote.
  Run `bd backup init <the same path>` once; `bd backup sync` then works.
  `bd backup status` kept showing the pre-migration "Last backup" time after
  successful syncs, so check freshness from the destination's `manifest` mtime or
  the job log instead.
- **Scheduled backup.** [`hub-backup.ts`](dolt-server-mode/hub-backup.ts) runs
  `bd backup sync` under a non-overlap lock with a 3 minute timeout and logs to
  `~/.local/state/dolt-hub/backup.log`; [`register-hub-backup.ts`](dolt-server-mode/register-hub-backup.ts)
  registers it every 15 minutes with `Bun.cron` (launchd job `bun.cron.hub-backup`,
  real `bun` binary, not a version-manager shim). Remove it with
  `bun -e 'await Bun.cron.remove("hub-backup")'`. Do not use `backup.enabled=true`
  instead: with many clients on one server each registers its own backup and
  full-syncs, which `bd` documents as a self-amplifying storm.
- **Restore check.** A backup restored into a scratch directory
  (`dolt backup restore file://<dest> <dir>`) had the same head commit hash as the
  live database and all 2540 issues. This covers the Dolt layer only; a restore
  through `bd backup restore` into a fresh server was not exercised.
- **Before it:** `dolt` installed as a real binary in `~/.local/bin` (the mise shim
  fails outside a mise project), with a Dolt identity set globally and for
  `bin/coord`'s separate `DOLT_ROOT_PATH`.

## Other machines

- **Default, no change required:** omarchy boxes and the Mac mini keep their own
  embedded replicas and keep syncing through the existing
  `git+https://github.com/andrew-bierman/agent-coordination.git` remote
  (`bin/coord sync`). From the Mac, `bd sync` replaces the embedded push/pull and
  needs the dolt binary and identity from Prerequisites. This keeps today's
  properties: claims can race between replicas before sync (see the hub
  AGENTS.md), and leases are per replica (`bd reclaim` only reaps leases its own
  store granted when `BEADS_NODE_ID` is set; leave it unset).
- **Optional later, eliminates the replica race:** make the other machines
  clients of this server, as in [MULTI-MACHINE.md](../MULTI-MACHINE.md): bind
  the server to this Mac's Tailscale address, create a dedicated SQL user (Dolt's
  `root` is loopback-only), and point each machine's `.beads/metadata.json`
  at it with `bd init --server --external --server-host H --server-port P`. They
  then share one lease table and one replica id. Not verified here.
- Each machine should check `bd --version` (1.3.2 and 1.3.4 both work against the
  server) and `dolt version >= 2.6.0` before it becomes a client.

## Backups

- Auto-backup is **off in server mode** (`bd backup status` says `auto: off in
  sql-server mode`). Schedule `bd backup sync` with the owner's existing
  scheduler (the opencode scheduler plugin for agent jobs, or `Bun.cron` for a
  script), for example every 15 minutes, with `BEADS_DIR` set. Verified: sync of
  the 760 MB database to a new `file://` destination took 14 s wall and produced
  a 174 MB backup.
- The existing destination `file://.../agent-coordination/.beads/backup` is
  registered inside the repo state (`backup_export`) and moves with the data
  directory, so `bd backup sync` keeps using it.

### Off-machine backup

`file://` destinations on the same disk protect against a corrupted database, not
against losing the machine. Dolt's `dolt backup` accepts these destinations (see
Dolt's backup docs and `dolt backup --help`):

- `file://<absolute path>`: local disk (what the hub uses today).
- `gs://<bucket>/<path>`: Google Cloud Storage, using Application Default
  Credentials; no Dolt-specific credential setup.
- `aws://[<dynamo table>:<bucket>]/<path>`: S3; Dolt's AWS guide also needs a
  DynamoDB table, so it takes more setup.
- DoltHub: the cloud option `bd backup --help` recommends
  (`bd backup init https://doltremoteapi.dolthub.com/<user>/<repo>` with
  `DOLT_REMOTE_USER` / `DOLT_REMOTE_PASSWORD`).
- `dolt backup sync-url <url>` pushes to any of these without registering a named
  backup.

A git remote used for replica sync is not a substitute: with `auto-push` off it is
only as fresh as the last manual sync and has no point-in-time history. `bd backup
init` configures a single destination; a second one means `dolt backup add` against
the server's database directly. None of the off-machine options above were tested
here. A hub that holds private work data is better backed up to a bucket the owner
controls than to a third-party service, unless that service's terms are acceptable.

- Also keep the step 3 snapshot until the server has run cleanly for a week.
- Not verified: restoring through `bd backup restore` into a fresh server (the Dolt-level restore was checked, see the production record), and a periodic
  `dolt gc` to keep the data dir from growing (an externally managed server gets
  no bd auto-GC; try `bd admin compact --dolt` on a clone before relying on it).

## Rollback to embedded (verified on a clone)

The server's data directory is a plain Dolt repo; the embedded engine reads it
unchanged. Fingerprints before and after were identical, and a write after
rollback succeeded.

```sh
cd ~/Code/agent-coordination
launchctl bootout gui/$(id -u)/com.lambe.daemon           # stop writers first
bd list --all --json --limit 0 | jq -S -c 'sort_by(.id)' | shasum -a 256 > /tmp/ac-server.sha   # needs the server, so before it stops
launchctl bootout gui/$(id -u)/local.agent-coordination.dolt
pgrep -fl 'dolt sql-server'                                # wait until gone (takes a few seconds)
[ -d .beads/embeddeddolt ] && mv .beads/embeddeddolt .beads/embeddeddolt.stale   # the pre-migration copy, or the one renamed in step 7
mkdir -p .beads/embeddeddolt
mv .beads/dolt/ac .beads/embeddeddolt/ac                   # carries every write made in server mode
python3 - <<'PY'
import json
p='.beads/metadata.json'; m=json.load(open(p))
m['dolt_mode']='embedded'; m.pop('dolt_server_host',None); m.pop('dolt_server_port',None)
json.dump(m,open(p,'w'),indent=2)
PY
rm -f .beads/dolt-server.port; mv .beads/dolt .beads/dolt.stopped   # holds .doltcfg (password hash); delete when done
# config.yaml: remove `port:` and `auto-start:` under `dolt:` (harmless if left).
bd list --all --json --limit 0 | jq -S -c 'sort_by(.id)' | shasum -a 256 | diff - /tmp/ac-server.sha && echo IDENTICAL
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.lambe.daemon.plist
```

If the server is already damaged, restore instead from the step 3 snapshot
(`rm -rf .beads && cp -cR ~/Code/_backups/ac-beads-pre-server-... .beads`) or from
`.beads/backup` with `bd backup restore --force`; either loses writes since the
snapshot or last sync.

## Risks

- **Single point of failure.** Server down means every `bd` call fails fast (under
  0.1 s) until launchd restarts it (about 2 s after a crash, `ThrottleInterval` 10 s
  for crash loops). Watch `~/.local/state/dolt-hub/dolt.err.log`.
- **auto-start must stay false.** Otherwise a stopped server lets bd spawn a second
  one on the same data directory.
- **Sync needs new pieces on the Mac** (real dolt, identity). Without them a git
  remote push or pull fails, whereas embedded mode needed neither. Sync is also
  slow (tens of seconds) on a loaded host; unchanged from embedded.
- **Password handling.** Loopback only; the password is in
  `~/.config/beads/credentials` (mode 600) and `.beads/dolt/.doltcfg`. Any process
  running as this user can read both. Keep the bind at `127.0.0.1`.
- **No automatic backup, no automatic GC** in server mode (see Backups).
- **Stale embedded copy.** Rename `.beads/embeddeddolt` (step 7) so nothing opens
  it. A writer running an old `bd` with an explicit `--db` could still diverge it.
- **Version coupling.** bd refuses a Dolt server older than 2.6.0; upgrade dolt
  and restart the launchd job together. bd 1.3.2 and 1.3.4 clients both worked.
- **Memory.** About 370 MB resident when idle after a migration test.
- **Torn clones.** Never `cp -cR` the data dir while the server runs.

## Go / no-go checklist

Go only when every line is true.

- [ ] `dolt version` is 2.6.0 or newer, the binary is real (not a mise shim) and
      is first on the launchd PATH; identity set for both `HOME` and the `coord`
      `DOLT_ROOT_PATH`.
- [ ] The launchd job runs a clone of the hub in a rehearsal (`bd dolt show`
      says `Server connection OK`, fingerprint identical) the same day.
- [ ] Step 3 snapshot exists and `bd backup sync` just succeeded.
- [ ] All local writers are stopped for the window; the fleet knows.
- [ ] Root password set and `-p ""` login is refused; credentials file is mode 600.
- [ ] `dolt.auto-start: false` is in `.beads/config.yaml`.
- [ ] A scheduled `bd backup sync` is ready to enable right after the flip.
- [ ] Rollback steps read through; owner accepts that writes made in server mode
      come back with the rollback only if the server was stopped cleanly.
- [ ] After the flip: `lambe doctor` has 0 fail, one dispatch to completion
      works, `bd sync` against the real remote succeeds once.

## Not verified

- Anything against the real GitHub remote or the real hub: `git+https` sync from
  server mode, other machines' replicas pulling the Mac's push, conflicts with
  real concurrent remote writers. Only local `file://` and `git+file://` remotes
  were exercised.
- The production daemon under server mode was exercised with one real opencode run
  of a harmless demo workflow (gate green); the clone tests used a fake ACP agent.
  No long soak was done.
- Other machines as network clients of this server (see MULTI-MACHINE.md for what
  was proven earlier), and a Tailscale-bound server.
- A real 5-minute lease expiry in server mode (an already-expired lease was
  reclaimed); soak tests longer than a few minutes; `dolt gc`/compaction; restoring
  from backup into a fresh server.
- Behavior of production `bd` consumers that bypass `.beads/metadata.json`
  (explicit `--db`, other repos with `BEADS_DIR` copies).
- Absolute timings depend on the host: the box was shared and loaded
  (load average 7-29) throughout, which affects embedded numbers most.
