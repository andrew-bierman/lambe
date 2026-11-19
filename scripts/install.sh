#!/bin/sh
# Install a checksummed lambe release binary on macOS or Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/andrew-bierman/lambe/master/scripts/install.sh | sh
#
# Environment:
#   LAMBE_VERSION          release version, with or without the leading "v"
#                          (default: 0.8.4)
#   LAMBE_INSTALL_DIR      directory to install the binary into
#                          (default: ~/.local/bin)
#   LAMBE_PRIVATE_RELEASE  set to 1 to download through `gh` instead of curl;
#                          required while the repository is private and `gh`
#                          is signed in
set -eu

(
version=${LAMBE_VERSION:-0.8.4}
install_dir=${LAMBE_INSTALL_DIR:-$HOME/.local/bin}

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
)
