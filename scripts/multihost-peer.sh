#!/bin/sh
# Peer side of scripts/multihost-remote-proof.sh: runs on the SECOND machine
# as a plain bd client of the isolated Dolt server. Env: SERVER (host/IP),
# PORT, DBPW (throwaway db password), ACTOR (default host-mini). Not load-bearing; see docs/MULTI-MACHINE.md.
set -eu
: "${SERVER:?}" "${PORT:?}" "${DBPW:?}"
export BEADS_DOLT_PASSWORD=$DBPW
ACTOR=${ACTOR:-host-mini}
W=$(mktemp -d "${TMPDIR:-/tmp}/lambe-peer.XXXXXX")
trap 'rm -rf "$W"' EXIT
cd "$W"
export BEADS_ACTOR=$ACTOR
echo "peer host=$(hostname) bd=$(bd --version) actor=$ACTOR server=$SERVER:$PORT"
bd init --non-interactive --server --external --server-host "$SERVER" --server-port "$PORT" \
  --server-user bd -p mh --database mh >/dev/null 2>&1 || { echo "peer: bd init failed"; exit 1; }
q() { bd "$@" 2>/dev/null; }
field() { q show "$1" --json | grep -m1 "\"$2\"" | sed 's/.*: "\(.*\)",*/\1/'; }
idof() { q list --all --json --limit 0 | awk -v t="$1" '/"id":/{id=$2} /"title":/{if(index($0,"\"" t "\"")){gsub(/[",]/,"",id);print id;exit}}'; }

q create peer-ready --silent -p 2 >/dev/null
until [ -n "$(idof go)" ]; do sleep 1; done
echo "== peer race =="
won=""; lost=0
for id in $(q list --all --json --limit 0 | awk '/"id":/{id=$2} /"title": "race-/{gsub(/[",]/,"",id);print id}'); do
  if q update "$id" --claim --json >/dev/null; then won="$won $id"; else lost=$((lost+1)); fi
done
echo "PEER_RACE won=$(echo $won | wc -w | tr -d ' ') lost=$lost ids:$won"

echo "== peer healthy-lease check =="
until [ "$(field "$(idof healthy)" assignee)" = host-mac ]; do sleep 1; done
H=$(idof healthy); bad=0
for i in 1 2 3 4 5 6; do
  q reclaim --older-than 0s --json >/dev/null || true
  if q heartbeat "$H" --json >/dev/null; then bad=1; echo "peer heartbeat on foreign claim SUCCEEDED"; fi
  sleep 5
done
[ "$(field "$H" assignee)" = host-mac ] && [ $bad = 0 ] && echo "PEER_HEALTHY ok (reclaim spared live lease; heartbeat refused)" || echo "PEER_HEALTHY FAIL"

echo "== peer orphan recovery (real 5m lease expiry) =="
until [ "$(field "$(idof orphan)" assignee)" = host-mac ]; do sleep 1; done
O=$(idof orphan); start=$(date +%s)
while [ "$(field "$O" status)" != open ]; do q reclaim --older-than 0s --json >/dev/null || true; sleep 10; done
q update "$O" --claim --json >/dev/null && echo "PEER_ORPHAN recovered $O after $(( $(date +%s) - start ))s, now assignee=$(field "$O" assignee)"
