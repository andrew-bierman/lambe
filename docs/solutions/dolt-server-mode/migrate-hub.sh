#!/bin/zsh
# Migrates a Beads hub from embedded Dolt to a loopback dolt sql-server.
# Review every line before running; it changes a live hub. One-shot production hub migration: embedded Dolt -> loopback dolt sql-server (runbook: lambe docs/solutions/dolt-server-mode.md).
# Holds bd's embedded gate lock for the critical section so no writer can slip in; any failure rolls back.
set -u
HUB=${HUB:-$HOME/Code/agent-coordination}
cd "$HUB" || exit 2
export BEADS_DIR="$HUB/.beads"
DOLT=$HOME/.local/bin/dolt
HERE=${0:A:h}
SENT=${TMPDIR:-/tmp}/lambe-hub-fence.sentinel
TS=$(date +%Y%m%d-%H%M)
SNAP=$HOME/Code/_backups/ac-beads-pre-server-$TS
PLIST=$HOME/Library/LaunchAgents/local.agent-coordination.dolt.plist
LABEL=local.agent-coordination.dolt
LOG=$HOME/.local/state/dolt-hub/migrate-hub.log
mkdir -p "${LOG:h}"
exec > >(tee -a "$LOG") 2>&1
echo "== migrate start $(date)"

release() { rm -f "$SENT"; }
rollback() {
  echo "!! ROLLBACK: $1"
  python3 -I - <<'PY'
import json
p='.beads/metadata.json'; m=json.load(open(p))
m['dolt_mode']='embedded'; m.pop('dolt_server_host',None)
json.dump(m,open(p,'w'),indent=2)
PY
  [ -f "$HUB/.beads/config.yaml.pre-server" ] && cp "$HUB/.beads/config.yaml.pre-server" "$HUB/.beads/config.yaml"
  launchctl bootout gui/$(id -u)/$LABEL 2>/dev/null
  rm -f "$PLIST"
  rm -rf "$HUB/.beads/dolt"
  release
  echo "!! rolled back; hub is embedded again"
  exit 1
}

# preconditions
[ -e "$HOME/.config/beads/credentials" ] && { echo "credentials file already exists; refusing to overwrite"; exit 3; }
[ -d "$HUB/.beads/dolt" ] && { echo ".beads/dolt already exists; refusing"; exit 3; }
lsof -nP -iTCP:3418 >/dev/null 2>&1 && { echo "port 3418 busy"; exit 3; }
mkdir -p "$HOME/Code/_backups" "$HOME/.local/state/dolt-hub"

# acquire the fence (bd fails fast while it is held)
touch "$SENT"; rm -f "$SENT.held"
python3 -I "$HERE/holdlock.py" "$HUB/.beads/embeddeddolt.gate.lock" "$SENT" > "${TMPDIR:-/tmp}/lambe-hub-holdlock.log" 2>&1 &
for i in $(seq 1 120); do [ -f "$SENT.held" ] && break; sleep 1; done
[ -f "$SENT.held" ] || { echo "could not acquire fence"; release; exit 4; }
echo "fence held at $(date +%T)"

cp "$HUB/.beads/config.yaml" "$HUB/.beads/config.yaml.pre-server"
cp "$HUB/.beads/metadata.json" "$HUB/.beads/metadata.json.pre-server"

# snapshot + copy (APFS clones)
cp -cR "$HUB/.beads" "$SNAP" || rollback "snapshot failed"
echo "snapshot: $SNAP"
mkdir -p "$HUB/.beads/dolt" && cp -cR "$HUB/.beads/embeddeddolt/ac" "$HUB/.beads/dolt/ac" || rollback "copy failed"

# plist + start server
cat > "$PLIST" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$DOLT</string>
    <string>sql-server</string>
    <string>--host</string><string>127.0.0.1</string>
    <string>--port</string><string>3418</string>
    <string>--data-dir</string><string>$HUB/.beads/dolt</string>
    <string>--max-connections</string><string>200</string>
    <string>--loglevel</string><string>info</string>
  </array>
  <key>WorkingDirectory</key><string>$HUB/.beads/dolt</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ExitTimeOut</key><integer>60</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$HOME/.local/state/dolt-hub/dolt.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/.local/state/dolt-hub/dolt.err.log</string>
</dict>
</plist>
XML
plutil -lint "$PLIST" >/dev/null || rollback "plist invalid"
launchctl bootstrap gui/$(id -u) "$PLIST" || rollback "bootstrap failed"
for i in $(seq 1 60); do lsof -nP -iTCP:3418 -sTCP:LISTEN >/dev/null 2>&1 && break; sleep 1; done
lsof -nP -iTCP:3418 -sTCP:LISTEN >/dev/null 2>&1 || rollback "server did not listen"

PW=$(openssl rand -hex 16)
$DOLT --host 127.0.0.1 --port 3418 --no-tls -u root -p "" sql -q "ALTER USER 'root'@'localhost' IDENTIFIED BY '$PW'" || rollback "set password failed"
mkdir -p "$HOME/.config/beads"; ( umask 077; printf '[127.0.0.1:3418]\npassword = %s\n' "$PW" > "$HOME/.config/beads/credentials" ); chmod 600 "$HOME/.config/beads/credentials"

# verify server data == snapshot (commit hash + row counts), without using bd (fenced)
H_SRV=$($DOLT --host 127.0.0.1 --port 3418 --no-tls -u root -p "$PW" sql -r csv -q "use ac; select dolt_hashof('HEAD') as h" 2>/dev/null | tail -1)
H_SNAP=$(cd "$SNAP/embeddeddolt/ac" && $DOLT sql -r csv -q "select dolt_hashof('HEAD') as h" 2>/dev/null | tail -1)
echo "head hash server=$H_SRV snapshot=$H_SNAP"
[ -n "$H_SRV" ] && [ "$H_SRV" = "$H_SNAP" ] || rollback "head hash mismatch"
C_SRV=$($DOLT --host 127.0.0.1 --port 3418 --no-tls -u root -p "$PW" sql -r csv -q "use ac; select count(*) from issues" 2>/dev/null | tail -1)
C_SNAP=$(cd "$SNAP/embeddeddolt/ac" && $DOLT sql -r csv -q "select count(*) from issues" 2>/dev/null | tail -1)
echo "issue rows server=$C_SRV snapshot=$C_SNAP"
[ -n "$C_SRV" ] && [ "$C_SRV" = "$C_SNAP" ] || rollback "row count mismatch"

# flip the hub to server mode
python3 -I - <<'PY' || rollback "metadata flip failed"
import json
p='.beads/metadata.json'; m=json.load(open(p))
m['dolt_mode']='server'; m['dolt_server_host']='127.0.0.1'
json.dump(m,open(p,'w'),indent=2)
PY
python3 -I - <<'PY' || rollback "config edit failed"
import re
p='.beads/config.yaml'; s=open(p).read()
assert 'auto-start' not in s
m=re.search(r'^dolt:\s*\n',s,re.M)
assert m, 'no dolt: key'
s=s[:m.end()]+"    port: 3418\n    auto-start: false\n"+s[m.end():]
open(p,'w').write(s)
PY
echo "flipped at $(date +%T)"

# release the fence; from here bd uses the server
release
sleep 2
echo "== verify"
bd dolt show 2>&1 | head -8
( time bd list --json --limit 1 >/dev/null ) 2>&1 | grep real
bd list --json --status in_progress 2>/dev/null | python3 -I -c "import json,sys;print('in_progress via server:',len(json.load(sys.stdin)))"
echo "== migrate done $(date)"
