#!/usr/bin/env bash
# Regenerate dashboard screenshots (desktop + 40-column mobile) from the
# in-memory demo data. Needs vhs, ttyd and ffmpeg. Never touches a real hub.
# Usage: scripts/capture.sh   (from anywhere; writes into docs/assets/)
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in vhs ttyd ffmpeg; do
  command -v "$tool" >/dev/null || { echo "capture: missing $tool" >&2; exit 1; }
done
mkdir -p .dev/bin
mise exec go -- go build -o .dev/bin/lambe-dev ./cmd/lambe
for tape in scripts/dashboard.tape scripts/dashboard-mobile.tape; do
  vhs "$tape"
done
ls -l docs/assets/dashboard*.png docs/assets/history*.png
