#!/bin/sh
# Print the CHANGELOG.md body for a release version (X.Y.Z, optional leading v).
# Fails if the section is missing or empty. Used by CI, release.yml, release.sh.
set -eu
v=${1:?usage: changelog-section.sh <version>}
v=${v#v}
file=${CHANGELOG:-$(dirname -- "$0")/../CHANGELOG.md}
body=$(awk -v h="## [$v]" '
	index($0, h) == 1 { on = 1; next }
	on && /^## \[/ { exit }
	on && /^\[[^]]+\]: / { exit }
	on { print }
' "$file")
if [ -z "$(printf %s "$body" | tr -d '[:space:]')" ]; then
	echo "error: CHANGELOG.md has no non-empty section for [$v]" >&2
	exit 1
fi
printf '%s\n' "$body"
