#!/bin/sh
# Release helper (NON-LOAD-BEARING: the tag and CI are the source of truth).
# Validates, then creates an annotated tag vX.Y.Z locally. Dry-run by default.
#
#   scripts/release.sh 0.9.3          # validate only
#   scripts/release.sh 0.9.3 --tag    # also create the local tag (never pushes)
set -eu
cd -- "$(dirname -- "$0")/.."
ver=${1:?usage: release.sh X.Y.Z [--tag]}
ver=${ver#v}
tag=v$ver
fail() { echo "error: $*" >&2; exit 1; }

echo "$ver" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || fail "$ver is not SemVer X.Y.Z"
[ "$(git rev-parse --abbrev-ref HEAD)" = master ] || fail "not on master"
[ -z "$(git status --porcelain)" ] || fail "working tree not clean"
git fetch -q origin master
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/master)" ] || fail "HEAD differs from origin/master"
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && fail "tag $tag already exists"
scripts/changelog-section.sh "$ver" >/dev/null || fail "write the CHANGELOG section first"
grep -q "^\[$ver\]: " CHANGELOG.md || fail "CHANGELOG.md is missing the [$ver] compare link"
state=$(gh run list --branch master --commit "$(git rev-parse HEAD)" --workflow ci --json conclusion -q '.[0].conclusion')
[ "$state" = success ] || fail "CI on $(git rev-parse --short HEAD) is '${state:-missing}', not success"
echo "ok: $tag is releasable from $(git rev-parse --short HEAD)"

if [ "${2:-}" = --tag ]; then
	git tag -a "$tag" -m "lambe $tag"
	echo "created $tag. Next: git push origin $tag   (release.yml publishes); then see docs/RELEASING.md"
else
	echo "dry run: re-run with --tag to create the annotated tag"
fi
