#!/bin/sh
# Package the LambeDash dashboard as a distributable ZIP.
#
# Builds a native Xcode Release universal (arm64 + x86_64) LambeDash.app,
# signs it with the hardened runtime, verifies the signature, and packs the
# app into a ZIP with a SHA256 checksum.
#
# Signing is ad hoc (`codesign -s -`) unless APPLE_SIGNING_IDENTITY names a
# Developer ID Application identity. Notarization is a drop-in: with a
# Developer ID identity AND either APPLE_NOTARY_PROFILE (a notarytool keychain
# profile) or APPLE_ID + APPLE_TEAM_ID + APPLE_APP_SPECIFIC_PASSWORD, the
# script submits the ZIP, waits, staples the ticket, and re-packs. With none
# of these set it produces the ad-hoc build described in docs/RELEASING.md.
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

echo "==> Checking the app icon (mascot iconset, 16..1024 px, in the compiled catalog)"
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIconName' "$APP/Contents/Info.plist")" = AppIcon ] ||
	{ echo "error: CFBundleIconName is not AppIcon" >&2; exit 1; }
ICONS=$(assetutil --info "$APP/Contents/Resources/Assets.car" |
	awk '/"Name" : "AppIcon"/ {icon=1} icon && /"PixelWidth"/ {gsub(/[^0-9]/, "", $3); print $3; icon=0}' | sort -n | uniq | tr '\n' ' ')
echo "    AppIcon pixel widths: $ICONS"
for px in 16 32 64 128 256 512 1024; do
	case " $ICONS " in
	*" $px "*) ;;
	*) echo "error: AppIcon is missing the ${px}px image" >&2; exit 1 ;;
	esac
done
[ -f "$APP/Contents/Resources/AppIcon.icns" ] || { echo "error: AppIcon.icns missing" >&2; exit 1; }

IDENTITY=${APPLE_SIGNING_IDENTITY:--}
if [ "$IDENTITY" = "-" ]; then
	echo "==> Signing ad hoc with the hardened runtime"
	codesign --force --options runtime --timestamp=none --sign - -- "$APP"
else
	echo "==> Signing with Developer ID identity: $IDENTITY"
	codesign --force --options runtime --timestamp --sign "$IDENTITY" -- "$APP"
fi

echo "==> Verifying code signature"
codesign --verify --strict --deep --verbose=2 -- "$APP"
SIGINFO=$(codesign -dv -- "$APP" 2>&1)
echo "$SIGINFO" | sed 's/^/    /'
case $SIGINFO in
*runtime*) ;;
*) echo "error: hardened runtime flag missing from the signature" >&2; exit 1 ;;
esac

echo "==> Checking bundle version"
PLIST="$APP/Contents/Info.plist"
for key in CFBundleShortVersionString CFBundleVersion; do
	got=$(/usr/libexec/PlistBuddy -c "Print :$key" "$PLIST")
	[ "$got" = "$CORE" ] || { echo "error: $key is $got, want $CORE" >&2; exit 1; }
done

ZIP="$OUT/LambeDash.zip"
pack() {
	STAGED=$(mktemp "$OUT/.LambeDash-zip.XXXXXXXX")
	trap 'test ! -e "$STAGED" || unlink "$STAGED"' EXIT
	# ditto creates the archive itself.
	unlink "$STAGED"
	ditto -c -k --sequesterRsrc --keepParent -- "$APP" "$STAGED"
	mv -f -- "$STAGED" "$ZIP"
}

echo "==> Packing ZIP"
pack

NOTARY_ARGS=""
if [ -n "${APPLE_NOTARY_PROFILE:-}" ]; then
	NOTARY_ARGS="--keychain-profile $APPLE_NOTARY_PROFILE"
elif [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_TEAM_ID:-}" ] && [ -n "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]; then
	NOTARY_ARGS="--apple-id $APPLE_ID --team-id $APPLE_TEAM_ID --password $APPLE_APP_SPECIFIC_PASSWORD"
fi
if [ -n "$NOTARY_ARGS" ]; then
	[ "$IDENTITY" != "-" ] || { echo "error: notarization needs APPLE_SIGNING_IDENTITY (Developer ID Application)" >&2; exit 1; }
	echo "==> Notarizing (this waits for Apple)"
	# shellcheck disable=SC2086 # NOTARY_ARGS is a deliberate word list
	xcrun notarytool submit "$ZIP" $NOTARY_ARGS --wait
	xcrun stapler staple "$APP"
	xcrun stapler validate "$APP"
	echo "==> Re-packing the stapled app"
	pack
else
	echo "==> Not notarized (no APPLE_NOTARY_PROFILE or APPLE_ID/APPLE_TEAM_ID/APPLE_APP_SPECIFIC_PASSWORD)"
fi

echo "==> Checksum"
(cd -- "$OUT" && shasum -a 256 -- LambeDash.zip > LambeDash.zip.sha256)
cat "$OUT/LambeDash.zip.sha256"

echo "==> Done"
echo "    app:      $APP"
echo "    zip:      $ZIP"
echo "    checksum: $OUT/LambeDash.zip.sha256"
echo "    verify:   cd apps/dashboard/.build/package && shasum -a 256 -c LambeDash.zip.sha256"
