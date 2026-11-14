#!/bin/sh
# Cross-compile compose-installer for all supported platforms.
# Run inside the golang build image (from the repo root):
#   docker run --rm -v <repo-root>:/src -w /src \
#     -e GOFLAGS=-buildvcs=false \
#     golang:1.25-bookworm sh scripts/build-all.sh
# Or simply: make build-all
# The version is the current git commit (mounted .git), injected via -ldflags.
# web/dist must exist before building (Go embeds it): run `make web` first or
# create the placeholder via `make clean`.
set -e
cd "$(dirname "$0")/.."

VER=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS="-s -w -X main.version=${VER}"

for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os=${p%/*}
  arch=${p#*/}
  ext=""
  [ "$os" = windows ] && ext=.exe
  echo "===> bin/compose-installer-$os-$arch$ext (${VER})"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" \
    -o "bin/compose-installer-$os-$arch$ext" .
done

echo ""
echo "Artifacts:"
ls -lh bin
