#!/usr/bin/env bash
# Multi-machine Beads proof against an ISOLATED dolt sql-server + database.
# Never touches production Beads: everything lives under a fresh temp dir and
# a private port. See docs/MULTI-MACHINE.md. Not load-bearing (a proof, not
# infrastructure).
#
# Env: ROUNDS (default 20), PORT (default 33991), BIND (default 127.0.0.1),
#      REAL_EXPIRY=1 waits out the real 5m lease instead of backdating it.
set -euo pipefail

ROUNDS=${ROUNDS:-20}
PORT=${PORT:-33991}
BIND=${BIND:-127.0.0.1}
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/lambe-multihost.XXXXXX")
export DOLT_ROOT_PATH=$ROOT/doltroot
mkdir -p "$DOLT_ROOT_PATH" "$ROOT/dolt" "$ROOT/hostA" "$ROOT/hostB"
cleanup() { [ -n "${DPID:-}" ] && kill "$DPID" 2>/dev/null || true; rm -rf "$ROOT"; }
trap cleanup EXIT

dolt config --global --add user.name multihost >/dev/null
dolt config --global --add user.email multihost@example.invalid >/dev/null
(cd "$ROOT/dolt" && dolt init >/dev/null 2>&1)
(cd "$ROOT/dolt" && exec dolt sql-server -H "$BIND" -P "$PORT" >"$ROOT/dolt.log" 2>&1) &
DPID=$!
for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/"$PORT") 2>/dev/null && break; sleep 0.2; done

# Each host is its own workspace (own .beads) pointing at the SAME server and
# database: one replica, one atomic claim authority.
for h in A B; do
  (cd "$ROOT/host$h" && BEADS_ACTOR=host-$h bd init --non-interactive --server --external \
     --server-host 127.0.0.1 --server-port "$PORT" -p mh --database mh >/dev/null 2>&1)
done
hostA() { (cd "$ROOT/hostA" && BEADS_ACTOR=host-a bd "$@" 2>/dev/null); }
hostB() { (cd "$ROOT/hostB" && BEADS_ACTOR=host-b bd "$@" 2>/dev/null); }
newid() { hostA create "$1" --silent -p 2; }
assignee() { hostB show "$1" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0].get("assignee",""))'; }
status() { hostB show "$1" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["status"])'; }
fail() { echo "FAIL: $*"; exit 1; }

echo "== check 1: contested claim, $ROUNDS rounds =="
a_wins=0; b_wins=0
for i in $(seq "$ROUNDS"); do
  id=$(newid "race-$i")
  ra=$(mktemp); rb=$(mktemp)
  (hostA update "$id" --claim --json >/dev/null && echo won >"$ra" || echo lost >"$ra") &
  (hostB update "$id" --claim --json >/dev/null && echo won >"$rb" || echo lost >"$rb") &
  wait %2 %3 2>/dev/null || wait
  wa=$(cat "$ra"); wb=$(cat "$rb"); rm -f "$ra" "$rb"
  [ "$wa$wb" = wonlost ] && a_wins=$((a_wins+1))
  [ "$wa$wb" = lostwon ] && b_wins=$((b_wins+1))
  [ "$wa$wb" = wonlost ] || [ "$wa$wb" = lostwon ] || fail "round $i: A=$wa B=$wb"
  owner=$(assignee "$id"); case "$owner" in host-a) [ "$wa" = won ] || fail "owner/winner mismatch";; host-b) [ "$wb" = won ] || fail "owner/winner mismatch";; *) fail "owner=$owner";; esac
done
echo "PASS: exactly one winner in all $ROUNDS rounds (A=$a_wins B=$b_wins)"

echo "== check 2: healthy lease survives =="
id=$(newid healthy)
hostA update "$id" --claim --json >/dev/null
hostA heartbeat "$id" --json >/dev/null
hostB reclaim --older-than 0s --json >/dev/null
[ "$(assignee "$id")" = host-a ] && [ "$(status "$id")" = in_progress ] || fail "healthy lease was reclaimed"
if hostB heartbeat "$id" --json >/dev/null; then fail "host B heartbeat on A's claim succeeded"; fi
echo "PASS: B reclaim --older-than 0s left A's live claim alone; B heartbeat refused"

echo "== check 3: host B recovers after host A disappears =="
id=$(newid orphan)
hostA update "$id" --claim --json >/dev/null          # A claims, then 'dies' (never heartbeats again)
if [ "${REAL_EXPIRY:-0}" = 1 ]; then
  echo "waiting out the real 5m lease..."; sleep 305
else
  hostB sql "update leases set lease_expires_at = now() - interval 1 hour where issue_id = '$id'" >/dev/null
fi
hostB reclaim --older-than 0s --json >/dev/null
[ "$(status "$id")" = open ] && [ -z "$(assignee "$id")" ] || fail "orphan not reclaimed: $(status "$id")/$(assignee "$id")"
hostB update "$id" --claim --json >/dev/null
[ "$(assignee "$id")" = host-b ] || fail "B could not take over"
if hostA heartbeat "$id" --json >/dev/null; then fail "dead host A heartbeat still accepted"; fi
echo "PASS: lease expiry -> reclaim -> host-b claimed; host-a heartbeat now refused"
