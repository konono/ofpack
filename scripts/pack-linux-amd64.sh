#!/bin/sh
# Pack the current project's mise toolchain for linux/amd64.
#
# The tools are installed inside a linux/amd64 Podman container and packed
# there, so the archive contains Linux binaries even when this machine is a
# Mac. Run it from the project directory:
#
#     scripts/pack-linux-amd64.sh [output.tar.gz]
#
# Requires: podman, and a Go toolchain to build the Linux mpack binary.
set -eu

out="${1:-$(basename "$PWD").linux-amd64.mpack.tar.gz}"
src="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
bin="$src/mpack-linux-amd64"

if ! command -v podman >/dev/null 2>&1; then
  echo "podman is required but not installed" >&2
  exit 1
fi

need_go() {
  command -v go >/dev/null 2>&1 && return 0
  echo "go is required to build $1, but it is not on PATH." >&2
  echo "Install Go, or build the binary elsewhere and pass it with --mpack-bin." >&2
  exit 1
}

# The pack runs inside the container, so it needs a Linux mpack.
if [ ! -x "$bin" ]; then
  need_go "$bin"
  echo "building $bin"
  ( cd "$src" && GOOS=linux GOARCH=amd64 go build -o "$bin" . )
fi
# And a host mpack to drive the container.
host="$src/mpack"
if [ ! -x "$host" ]; then
  need_go "$host"
  echo "building $host"
  ( cd "$src" && go build -o "$host" . )
fi

exec "$host" pack --target linux/amd64 --mpack-bin "$bin" -o "$out"
