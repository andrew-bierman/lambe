#!/bin/sh
# List placeholders still unfilled in a repo after copying the template files.
# NON-LOAD-BEARING helper: it only reports; CI and the tag are the real gates.
#
#   docs/release-template/check.sh /path/to/repo
set -eu
repo=${1:?usage: check.sh <repo-dir>}
cd -- "$repo"
# __UPPER_SNAKE__ tokens, in the files the template installs.
files=".goreleaser.yaml .github/workflows/release.yml scripts/release.sh scripts/changelog-section.sh scripts/install.sh CHANGELOG.md"
found=0
for f in $files; do
	[ -f "$f" ] || { echo "missing: $f"; found=1; continue; }
	# /dev/null forces grep to prefix matches with the file name.
	if grep -nE '__[A-Z][A-Z0-9_]*__' "$f" /dev/null; then found=1; fi
done
[ "$found" = 0 ] && echo "ok: no placeholders left"
exit 0
