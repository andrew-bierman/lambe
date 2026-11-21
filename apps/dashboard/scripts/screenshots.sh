#!/usr/bin/env bash
# Repeatable window screenshots of LambeDash fixture scenarios.
#
# Builds the app into a temp DerivedData dir, launches the built binary once
# per scenario with `-LambeScenario <name>` (network-free fixtures, nothing
# polls), captures that window with screencapture, then quits it. Each window
# is on screen for about two seconds. Needs a logged-in GUI session and, for
# screencapture, Screen Recording permission for the terminal running this.
#
# usage: apps/dashboard/scripts/screenshots.sh [OUT_DIR] [scenario...]
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
out="${1:-$root/apps/dashboard/.build/screenshots}"
shift || true
scenarios=("$@")
[ ${#scenarios[@]} -gt 0 ] || scenarios=(live stalled archive empty disconnected neverConnected longText multiMachine)

work="$(mktemp -d "${TMPDIR:-/tmp}/lambedash-shots.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"

xcodebuild -project "$root/apps/dashboard/LambeDash.xcodeproj" -scheme LambeDash \
  -destination 'platform=macOS' -derivedDataPath "$work/dd" build -quiet
bin="$work/dd/Build/Products/Debug/LambeDash.app/Contents/MacOS/LambeDash"

cat > "$work/wid.swift" <<'SWIFT'
import CoreGraphics
let pid = Int32(CommandLine.arguments[1])!
let list = CGWindowListCopyWindowInfo([.optionOnScreenOnly], kCGNullWindowID) as! [[String: Any]]
for w in list where (w[kCGWindowOwnerPID as String] as? Int32) == pid && (w[kCGWindowLayer as String] as? Int) == 0 {
    print(w[kCGWindowNumber as String] as! Int); break
}
SWIFT
swiftc -o "$work/wid" "$work/wid.swift"

shoot() { # name, extra launch args...
  local name="$1"; shift
  "$bin" -LambeScenario "$name" "$@" >/dev/null 2>&1 &
  local pid=$!
  local wid=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    sleep 0.5
    wid="$("$work/wid" "$pid" || true)"
    [ -n "$wid" ] && break
  done
  if [ -n "$wid" ]; then
    sleep 1
    screencapture -x -o -l "$wid" "$out/${OUT_NAME:-$name}.png"
    echo "wrote $out/${OUT_NAME:-$name}.png"
  else
    echo "no window for scenario $name" >&2
  fi
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

for s in "${scenarios[@]}"; do
  case "$s" in
    stalled) OUT_NAME=stalled shoot stalled -LambeIssue lam-103 ;;
    archive) OUT_NAME=archive shoot archive -LambeSection history -LambeIssue lam-090 ;;
    longText) OUT_NAME=longText shoot longText -LambeIssue lambe-with-an-extraordinarily-long-issue-identifier-0042 ;;
    multiMachine) OUT_NAME=multiMachine shoot multiMachine -LambeIssue lam-103 ;;
    *) OUT_NAME="$s" shoot "$s" ;;
  esac
done
