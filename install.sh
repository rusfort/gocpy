#!/bin/sh
# curl -fsSL https://raw.githubusercontent.com/rusfort/gocpy/HEAD/install.sh | sh
# Env: GOCPY_VERSION (default: latest), GOCPY_PREFIX (default: /usr/local/bin or ~/.local/bin)
set -eu
REPO="rusfort/gocpy"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported arch: $arch" >&2; exit 1 ;; esac
ver="${GOCPY_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)
fi
ver="${ver#v}"
name="gocpy_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v${ver}"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
echo "downloading $name"
curl -fsSL "$base/$name" -o "$tmp/$name"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && grep " $name\$" checksums.txt | (sha256sum -c - 2>/dev/null || shasum -a 256 -c -))
tar -xzf "$tmp/$name" -C "$tmp" gocpy
dest="${GOCPY_PREFIX:-}"
if [ -z "$dest" ]; then
  if [ -w /usr/local/bin ]; then dest=/usr/local/bin; else dest="$HOME/.local/bin"; fi
fi
mkdir -p "$dest"
install -m 0755 "$tmp/gocpy" "$dest/gocpy"
echo "installed $dest/gocpy"
"$dest/gocpy" doctor || {
  echo
  echo "gocpy needs a C compiler and libgc:"
  echo "  macOS:          xcode-select --install && brew install bdw-gc"
  echo "  Debian/Ubuntu:  sudo apt install gcc libgc-dev"
  echo "  Fedora:         sudo dnf install gcc gc-devel"
  echo "  Alpine:         sudo apk add gcc musl-dev gc-dev"
}
