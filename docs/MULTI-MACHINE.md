# Multi-machine operation

How to run Lambe on more than one host against one Beads tracker, what is
guaranteed, and how to reproduce the proof. Everything here was proven against
an isolated throwaway Dolt server, never a production tracker.

## Design

- One Lambe daemon per host, each with a distinct actor. `lambe run` writes as
  `lambe@<machine>`, where machine is the short hostname unless the `machine`
  (`LAMBE_MACHINE`) or `actor` (`LAMBE_ACTOR`) config key overrides it; set one
  if two hosts could share a hostname.
- One live `dolt sql-server` owns the database. Every host's `.beads/` points
  at it (`bd init --server --external --server-host H --server-port P`). Claims
  are atomic server-side, so only one host wins a contested claim.
- All hosts that are clients of the same server are ONE bd replica. Leave
  `BEADS_NODE_ID` unset (or give every client of the store the same value), or
  bd's replica guard will skip peers' leases.
- bd's lease TTL is fixed at 5 minutes. A healthy daemon renews it every
  `supervisor.heartbeat_interval` (default 150s).
- Leases live in an ephemeral table on the server (`leases`); they are only
  meaningful on that one server. Do not federate leases across independent
  Dolt remotes; use the shared-server shape instead.

Ownership fences in Lambe (all four are covered by tests below):

1. `bd update --claim` is atomic; the loser gets `tracker.ErrLostRace`.
2. `bd heartbeat` by a non-owner fails; Lambe maps it to `tracker.ErrNotOwner`.
3. A worker whose heartbeat reports `ErrNotOwner` is cancelled
   (`reconcileWorkers`, `internal/symphony/workers.go`).
4. The gate closes with `bd update --if-assignee <actor>`; a stale owner's
   green gate is refused (`gate.ErrCloseRace` -> `GateErr`), never published.

## Automatic reclaim (default OFF)

```yaml
supervisor:
  auto_reclaim: false    # default
  reclaim_grace: 10m     # only used when auto_reclaim is true
```

With `auto_reclaim: true` the daemon runs `bd reclaim --older-than <grace>`
every tick. It reverts `in_progress` issues whose lease expired more than
`reclaim_grace` ago to `open`, so a surviving host can claim them. Config
validation requires `reclaim_grace` > tick interval.

Enable it on exactly one tier of hosts: the shared-server deployment above,
where one daemon dying must not strand work. Leave it off for a single host
(restart recovery already adopts orphans), for per-host embedded trackers, and
whenever a human wants to see stranded work before it moves. The reaper is
unscoped: it reclaims any stale lease on the server, not only Lambe's. With 5m
TTL the default 10m grace means a dead host's work moves after about 15m. Never
set the grace below the TTL plus your worst pause (sleep, GC, partition); a
live-but-paused owner is protected only by fences 2-4, which stop it on wake.

## Reproduce

Prerequisites: `bd` 1.3.x, `dolt`, Go via mise. In this repo:

```sh
# Check 1-3 at CLI level (loopback, ~1 min; backdates the lease instead of
# waiting 5 minutes; REAL_EXPIRY=1 waits it out):
ROUNDS=20 scripts/multihost-proof.sh

# Automated tests for checks 1-4 (need dolt on PATH; skip without it):
LAMBE_TEST_BD=1 go test -race -run MultiHost ./internal/tracker ./internal/gate

# Cross-machine: on host A (binds the server to one tailnet/LAN address only):
BIND=<A tailnet ip> PORT=33992 scripts/multihost-remote-proof.sh
# On host B, with the password printed in A's $ROOT/pw (throwaway):
SERVER=<A tailnet ip> PORT=33992 DBPW=<pw> sh scripts/multihost-peer.sh
```

The server user is a throwaway created inside a temp dir; both scripts delete
their state on exit. Never point them at a real tracker.

## Results

Run 2026-11-20. Host A: MacBook Pro, bd 1.3.4, dolt 2.7.5. Host B: Mac mini
(`macmini` Rex host), bd 1.3.2, over Tailscale (A bound to its tailnet IP).

| # | Check | Local (A + simulated B, loopback) | Real Mac mini over Tailscale |
|---|---|---|---|
| 1 | One winner per contested claim | 20/20 rounds, exactly one winner (A=9 B=11); Go test 8 rounds | 30 issues raced concurrently: every issue has exactly one owner (mac=21, mini=9), no double wins |
| 2 | Healthy lease survives | `bd reclaim --older-than 0s` by B left A's live claim; B heartbeat refused | Mac heartbeated 40s while the mini ran reclaim + heartbeat 6 times: claim never moved, every mini heartbeat refused |
| 3 | Peer recovers after owner disappears | lease backdated, reclaim, B claims, A heartbeat/release then `ErrNotOwner` | Mac claimed and went silent; mini recovered after the REAL 5m lease expired (317s on the mini, 321s on the Mac), then Mac's late heartbeat was refused |
| 4 | Lost owner cannot publish completion | `TestMultiHostLostOwnerCannotPublishCompletion`: A's green gate after B took over (and after reclaim to unowned) returns an error verdict; issue unchanged, not tombstoned. Mutation check: with an unguarded close the test fails | not run |

Notes:

- Check 4's worker-stop half (a heartbeat reporting `ErrNotOwner` cancels the
  run) is covered by the existing symphony loop tests with a fake tracker
  (`TestRetryStandsDownAfterClaimLost`); the real-bd tests prove the signal
  (`ErrNotOwner`) and the completion fence, not a full daemon-to-daemon run.
- The mini ran bd only (as a client of A's server), not a Lambe daemon. No Lambe
  daemon pair was run end to end.
- Server binding: A's server was bound to its Tailscale address only, with a
  throwaway `bd` user and password (Dolt's `root` is loopback-only).
- bd 1.3.2 (mini) against a server initialised by bd 1.3.4 worked.
