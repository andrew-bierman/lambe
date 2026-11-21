#!/bin/sh
# Package the LambeDash dashboard as a distributable ZIP.
#
# Builds a native Xcode Release universal (arm64 + x86_64) LambeDash.app with
# the project's own signing defaults (ad-hoc, "Sign to Run Locally"), verifies
# the signature, and packs the app into a ZIP with a SHA256 checksum.
#
# All output lands under apps/dashboard/.build/package/ (git-ignored). The
# packaging build uses its own DerivedData path so it never contends with the
# dashboard:build / dashboard:test tasks' DerivedData.
#
# The same machine and Xcode rebuild the same app; the ZIP's timestamps vary,
# so the emitted SHA256 identifies each build rather than being bit-stable.

# Version: pass X.Y.Z as $1, or build from a checkout whose HEAD is tagged vX.Y.Z.
# CFBundleShortVersionString and CFBundleVersion both come from that tag
# (prerelease/build suffixes dropped: Apple requires numeric X.Y.Z).

set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
PROJ="$ROOT/apps/dashboard/LambeDash.xcodeproj"
OUT="$ROOT/apps/dashboard/.build/package"
DD="$OUT/DerivedData"
APP="$DD/Build/Products/Release/LambeDash.app"

if [ $# -ge 1 ]; then
	VERSION=${1#v}
else
	TAG=$(git -C "$ROOT" describe --exact-match --tags --match 'v[0-9]*.[0-9]*.[0-9]*' HEAD 2>/dev/null) ||
		{ echo "error: HEAD is not tagged vX.Y.Z; pass the version as an argument" >&2; exit 1; }
	VERSION=${TAG#v}
fi
CORE=${VERSION%%[-+]*}
case $CORE in
*[!0-9.]* | '' | .* | *.) echo "error: '$VERSION' is not X.Y.Z" >&2; exit 1 ;;
esac
[ "$(echo "$CORE" | tr -cd . | wc -c)" -eq 2 ] || { echo "error: '$VERSION' is not X.Y.Z" >&2; exit 1; }

mkdir -p -- "$OUT"

echo "==> Building LambeDash.app $CORE (Release, universal arm64+x86_64)"
xcodebuild \
	-project "$PROJ" \
	-scheme LambeDash \
	-configuration Release \
	-destination 'platform=macOS' \
	-derivedDataPath "$DD" \
	ARCHS="arm64 x86_64" \
	ONLY_ACTIVE_ARCH=NO \
	MARKETING_VERSION="$CORE" \
	CURRENT_PROJECT_VERSION="$CORE" \
	build

echo "==> Checking the binary is universal"
BIN="$APP/Contents/MacOS/LambeDash"
ARCHS_BUILT=$(lipo "$BIN" -archs)
echo "$ARCHS_BUILT"
for arch in arm64 x86_64; do
	case " $ARCHS_BUILT " in
	*" $arch "*) ;;
	*) echo "error: missing $arch in built binary" >&2; exit 1 ;;
	esac
done

echo "==> Verifying ad-hoc code signature (project defaults)"
codesign --verify --strict --verbose=2 -- "$APP"
codesign -dv -- "$APP" 2>&1 | sed 's/^/    /'

echo "==> Checking bundle version"
PLIST="$APP/Contents/Info.plist"
for key in CFBundleShortVersionString CFBundleVersion; do
	got=$(/usr/libexec/PlistBuddy -c "Print :$key" "$PLIST")
	[ "$got" = "$CORE" ] || { echo "error: $key is $got, want $CORE" >&2; exit 1; }
done

echo "==> Packing ZIP"
ZIP="$OUT/LambeDash.zip"
STAGED=$(mktemp "$OUT/.LambeDash-zip.XXXXXXXX")
trap 'test ! -e "$STAGED" || unlink "$STAGED"' EXIT
# ditto creates the archive itself.
unlink "$STAGED"
ditto -c -k --sequesterRsrc --keepParent -- "$APP" "$STAGED"
mv -f -- "$STAGED" "$ZIP"

echo "==> Checksum"
(cd -- "$OUT" && shasum -a 256 -- LambeDash.zip > LambeDash.zip.sha256)
cat "$OUT/LambeDash.zip.sha256"

echo "==> Done"
echo "    app:      $APP"
echo "    zip:      $ZIP"
echo "    checksum: $OUT/LambeDash.zip.sha256"
echo "    verify:   cd apps/dashboard/.build/package && shasum -a 256 -c LambeDash.zip.sha256"
