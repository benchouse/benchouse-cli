#!/bin/sh
# Install the benchouse CLI from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/benchouse/benchouse-cli/main/install.sh | sh
#
# Installs to $BENCHOUSE_INSTALL_DIR, else ~/.local/bin. Pin a version with
# BENCHOUSE_VERSION=v0.1.0. Verifies the archive against checksums.txt, and
# checksums.txt against its keyless signature when cosign is installed
# (set BENCHOUSE_REQUIRE_SIGNATURE=1 to refuse to install without it).
set -eu

repo="benchouse/benchouse-cli"
dir="${BENCHOUSE_INSTALL_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os (download a release from https://github.com/$repo/releases)" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

asset="benchouse_${os}_${arch}.tar.gz"
version="${BENCHOUSE_VERSION:-}"
case "$version" in
  "") base="https://github.com/$repo/releases/latest/download" ;;
  v*) base="https://github.com/$repo/releases/download/$version" ;;
  *) echo "BENCHOUSE_VERSION must look like v0.1.0" >&2; exit 1 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
if command -v cosign >/dev/null 2>&1; then
  curl -fsSL "$base/checksums.txt.sigstore.json" -o "$tmp/checksums.txt.sigstore.json"
  cosign verify-blob "$tmp/checksums.txt" --bundle "$tmp/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "^https://github.com/$repo/" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>&1 \
    || { echo "signature check failed: not installing" >&2; exit 1; }
  echo "signature verified"
elif [ "${BENCHOUSE_REQUIRE_SIGNATURE:-}" = 1 ]; then
  echo "cosign not found and BENCHOUSE_REQUIRE_SIGNATURE=1: not installing" >&2; exit 1
fi
(cd "$tmp" && grep " $asset\$" checksums.txt | { command -v sha256sum >/dev/null && sha256sum -c - || shasum -a 256 -c -; }) >/dev/null

tar -xzf "$tmp/$asset" -C "$tmp" benchouse
mkdir -p "$dir"
install -m 0755 "$tmp/benchouse" "$dir/benchouse"
echo "installed $("$dir/benchouse" version) to $dir/benchouse"
case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH" ;; esac
