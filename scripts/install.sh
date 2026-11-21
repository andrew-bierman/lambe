#!/bin/sh
# Install a checksummed lambe release binary on macOS or Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/andrew-bierman/lambe/master/scripts/install.sh | sh
#
# Environment:
#   LAMBE_VERSION          release version, with or without the leading "v"
#                          (default: the latest GitHub release)
#   LAMBE_INSTALL_DIR      directory to install the binary into
#                          (default: ~/.local/bin)
#   LAMBE_PRIVATE_RELEASE  set to 1 to download through `gh` instead of curl;
#                          required while the repository is private and `gh`
#                          is signed in
set -eu

(
install_dir=${LAMBE_INSTALL_DIR:-$HOME/.local/bin}

version=${LAMBE_VERSION:-}
if [ -z "$version" ]; then
  # Resolve the latest release tag; the archive is still checksum-verified below.
  if [ "${LAMBE_PRIVATE_RELEASE:-0}" = 1 ]; then
    version=$(gh release view --repo andrew-bierman/lambe --json tagName --jq .tagName)
  else
    version=$(curl -fsSL https://api.github.com/repos/andrew-bierman/lambe/releases/latest |
      sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  fi
  if [ -z "$version" ]; then
    printf 'install.sh: could not resolve the latest release; set LAMBE_VERSION\n' >&2
    exit 1
  fi
fi

version=${version#v}
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || [ "$(printf %s "$version" | tr -d '0123456789.')" != "" ]; then
  printf 'install.sh: unsupported LAMBE_VERSION: %s\n' "$version" >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    printf 'install.sh: unsupported OS: %s\n' "$(uname -s)" >&2
    exit 1
    ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *)
    printf 'install.sh: unsupported architecture: %s\n' "$(uname -m)" >&2
    exit 1
    ;;
esac

archive="lambe_${version}_${os}_${arch}.tar.gz"
base="https://github.com/andrew-bierman/lambe/releases/download/v$version"

tmp_dir=$(mktemp -d)
staged=
trap 'rm -rf "$tmp_dir"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
trap 'exit 1' INT TERM

printf 'install.sh: downloading lambe v%s (%s/%s)\n' "$version" "$os" "$arch"
if [ "${LAMBE_PRIVATE_RELEASE:-0}" = 1 ]; then
  gh release download "v$version" --repo andrew-bierman/lambe \
    --dir "$tmp_dir" --pattern "$archive" --pattern checksums.txt
else
  curl -fsSL "$base/$archive" -o "$tmp_dir/$archive"
  curl -fsSL "$base/checksums.txt" -o "$tmp_dir/checksums.txt"
fi

# Optional: verify the signed checksums when cosign is installed. Releases
# before v0.10.1 carry no bundle; that is not an error.
if command -v cosign >/dev/null 2>&1; then
  if [ "${LAMBE_PRIVATE_RELEASE:-0}" = 1 ]; then
    gh release download "v$version" --repo andrew-bierman/lambe \
      --dir "$tmp_dir" --pattern checksums.txt.sigstore.json 2>/dev/null || true
  else
    curl -fsSL "$base/checksums.txt.sigstore.json" -o "$tmp_dir/checksums.txt.sigstore.json" 2>/dev/null || true
  fi
  if [ -s "$tmp_dir/checksums.txt.sigstore.json" ]; then
    cosign verify-blob "$tmp_dir/checksums.txt" \
      --bundle "$tmp_dir/checksums.txt.sigstore.json" \
      --certificate-identity "https://github.com/andrew-bierman/lambe/.github/workflows/release.yml@refs/tags/v$version" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com
  else
    printf 'install.sh: no cosign bundle for v%s; skipping signature check\n' "$version"
  fi
fi

# Verify the archive against the release checksums before extracting it.
(
  cd "$tmp_dir"
  awk -v name="$archive" '$2 == name {print; found=1} END {if (!found) exit 1}' \
    checksums.txt > selected-checksum.txt
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c selected-checksum.txt
  else
    sha256sum -c selected-checksum.txt
  fi
  tar -xzf "$archive"
)

mkdir -p "$install_dir"
staged=$(mktemp "$install_dir/.lambe.XXXXXX")
install -m 755 "$tmp_dir/lambe" "$staged"
mv -f "$staged" "$install_dir/lambe"
staged=
printf 'install.sh: installed %s\n' "$install_dir/lambe"
"$install_dir/lambe" version
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'install.sh: add %s to your PATH to run `lambe` by name\n' "$install_dir" ;;
esac
printf 'install.sh: next: lambe init && lambe doctor && lambe service install\n'
)
