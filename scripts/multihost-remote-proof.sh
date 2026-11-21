#!/usr/bin/env bash
# Host-A side of the cross-machine proof. Starts an ISOLATED dolt sql-server
# bound to BIND (a Tailscale/LAN address; never 0.0.0.0), seeds work, runs the
# same checks as scripts/multihost-proof.sh against a peer machine running
# scripts/multihost-peer.sh with SERVER=$BIND PORT=$PORT. The orphan check
# waits out bd's REAL 5m lease (no backdating). Cleans up on exit.
set -euo pipefail
BIND=${BIND:?set BIND to the tailnet or LAN IP of this host}
PORT=${PORT:-33992}
ROUNDS=${ROUNDS:-30}
PEER=${PEER:-host-mini}
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/lambe-multihost-remote.XXXXXX")
export DOLT_ROOT_PATH=$ROOT/doltroot
mkdir -p "$DOLT_ROOT_PATH" "$ROOT/dolt" "$ROOT/mac"
trap '[ -n "${DPID:-}" ] && kill $DPID 2>/dev/null; rm -rf "$ROOT"' EXIT
dolt config --global --add user.name multihost >/dev/null
dolt config --global --add user.email multihost@example.invalid >/dev/null
(cd "$ROOT/dolt" && dolt init >/dev/null 2>&1)
# root is loopback-only in Dolt; remote hosts need a real user. Throwaway
# credentials for a throwaway database bound to one tailnet/LAN address.
PW=$(openssl rand -hex 12)
(cd "$ROOT/dolt" && dolt sql -q "create user 'bd'@'%' identified by '$PW'; grant all on *.* to 'bd'@'%' with grant option;" >/dev/null)
export BEADS_DOLT_PASSWORD=$PW
(cd "$ROOT/dolt" && exec dolt sql-server -H "$BIND" -P "$PORT" >"$ROOT/dolt.log" 2>&1) & DPID=$!
for _ in $(seq 50); do (exec 3<>/dev/tcp/"$BIND"/"$PORT") 2>/dev/null && break; sleep 0.2; done
(cd "$ROOT/mac" && BEADS_ACTOR=host-mac bd init --non-interactive --server --external \
   --server-host "$BIND" --server-port "$PORT" --server-user bd -p mh --database mh >/dev/null 2>&1)
mac() { (cd "$ROOT/mac" && BEADS_ACTOR=host-mac bd "$@" 2>/dev/null); }
field() { mac show "$1" --json | grep -m1 "\"$2\"" | sed 's/.*: "\(.*\)",*/\1/'; }
fail() { echo "FAIL: $*"; exit 1; }

echo "server ready on $BIND:$PORT; start the peer now (SERVER=$BIND PORT=$PORT DBPW=<password in $ROOT/pw> sh scripts/multihost-peer.sh)"
echo "$PW" >"$ROOT/pw"
ids=()
for i in $(seq "$ROUNDS"); do ids+=("$(mac create "race-$i" --silent -p 2)"); done
H=$(mac create healthy --silent -p 2); O=$(mac create orphan --silent -p 2)
echo "seeded; waiting for the peer to join"
until mac list --all --json --limit 0 | grep -q '"peer-ready"'; do sleep 1; done
mac create go --silent -p 2 >/dev/null  # starting gun: both sides race from here

echo "== check 1: contested claim vs $PEER, $ROUNDS issues =="
mine=0
for id in "${ids[@]}"; do mac update "$id" --claim --json >/dev/null && mine=$((mine+1)); done
sleep 5
a=0; m=0
for id in "${ids[@]}"; do case "$(field "$id" assignee)" in host-mac) a=$((a+1));; "$PEER") m=$((m+1));; *) fail "$id unowned or foreign owner";; esac; done
[ "$a" = "$mine" ] && [ $((a+m)) = "$ROUNDS" ] || fail "mac won $mine but owns $a; mac=$a peer=$m of $ROUNDS"
echo "PASS: every issue has exactly one owner (mac=$a peer=$m of $ROUNDS); no double wins"

echo "== check 2: healthy lease (mac heartbeats 40s while peer reclaims) =="
mac update "$H" --claim --json >/dev/null
for i in 1 2 3 4 5 6 7 8; do mac heartbeat "$H" --json >/dev/null || fail "mac lost its healthy lease"; sleep 5; done
[ "$(field "$H" assignee)" = host-mac ] || fail "healthy lease stolen"
echo "PASS: mac kept its claim under peer reclaim attempts (peer output confirms heartbeat refusal)"

echo "== check 3: mac 'dies' (stops heartbeating); peer must recover after real 5m expiry =="
mac update "$O" --claim --json >/dev/null; t0=$(date +%s)
for _ in $(seq 120); do [ "$(field "$O" assignee)" = "$PEER" ] && break; sleep 5; done
[ "$(field "$O" assignee)" = "$PEER" ] || fail "peer never recovered the orphan"
echo "PASS: peer took over after $(( $(date +%s) - t0 ))s"
if mac heartbeat "$O" --json >/dev/null; then fail "dead mac heartbeat still accepted"; fi
echo "PASS: mac's late heartbeat refused (stop signal)"
