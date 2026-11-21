#!/usr/bin/env bash
# Window screenshots of LambeDash, without touching the desktop you are using.
#
# Builds the app, then launches it with `open -g` (no activation) and
# `-LambeHeadless` (accessory activation policy, a window sent behind every
# other window and never made key), captures that window with
# `screencapture -l <windowID>`, and quits only the instance it started.
# Demo or fixture data only, preferences kept in a throwaway defaults suite,
# and the default state directory pointed at an empty folder, so nothing
# real is read or shown. Needs Screen Recording permission for the terminal.
#
# usage: apps/dashboard/scripts/screenshots.sh [OUT_DIR] [name=args...]
#   default shots: demo-light, demo-dark (demo data, 1200x780)
#   a custom shot:  attention-dark="--demo -LambeSection attention -LambeIssue demo-kd8 -LambeAppearance dark"
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
out="${1:-$root/apps/dashboard/.build/screenshots}"
shift || true
mkdir -p "$out"

work="$(mktemp -d "${TMPDIR:-/tmp}/lambedash-shots.XXXXXX")"
trap 'rm -rf "$work"' EXIT
empty_state="$work/state"; mkdir -p "$empty_state"

xcodebuild -project "$root/apps/dashboard/LambeDash.xcodeproj" -scheme LambeDash \
  -destination 'platform=macOS' -derivedDataPath "$work/dd" build -quiet
app="$work/dd/Build/Products/Debug/LambeDash.app"

cat > "$work/wid.swift" <<'SWIFT'
import CoreGraphics
let pid = Int32(CommandLine.arguments[1])!
let list = CGWindowListCopyWindowInfo([.optionAll], kCGNullWindowID) as! [[String: Any]]
for w in list {
    guard (w[kCGWindowOwnerPID as String] as? Int32) == pid, (w[kCGWindowLayer as String] as? Int) == 0 else { continue }
    let b = w[kCGWindowBounds as String] as! [String: Any]
    guard (b["Height"] as! Double) > 100 else { continue }
    print(w[kCGWindowNumber as String] as! Int); break
}
SWIFT
swiftc -o "$work/wid" "$work/wid.swift"

shoot() { # name, launch args
  local name="$1"; shift
  local suite="lambe.shots.$$.$name" pid="" wid=""
  open -g -n "$app" --args -LambeHeadless -LambeDefaultsSuite "$suite" -LambeStateDir "$empty_state" \
    -LambeWindowSize 1200x780 "$@"
  for _ in $(seq 1 30); do
    sleep 0.5
    pid="$(pgrep -f -- "$suite" | head -1 || true)"
    [ -n "$pid" ] && wid="$("$work/wid" "$pid" || true)"
    [ -n "$wid" ] && break
  done
  if [ -n "$wid" ]; then
    sleep 3
    screencapture -x -o -l "$wid" "$out/$name.png"
    echo "wrote $out/$name.png"
  else
    echo "no window for $name" >&2
  fi
  [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  sleep 0.5
  defaults delete "$suite" 2>/dev/null || true
  rm -f "$HOME/Library/Preferences/$suite.plist"
}

if [ $# -eq 0 ]; then
  shoot demo-light --demo -LambeIssue demo-hk4 -LambeAppearance light
  shoot demo-dark --demo -LambeIssue demo-hk4 -LambeAppearance dark
else
  for spec in "$@"; do
    name="${spec%%=*}"; args="${spec#*=}"
    # shellcheck disable=SC2086 # args is a deliberate word list
    shoot "$name" $args
  done
fi
