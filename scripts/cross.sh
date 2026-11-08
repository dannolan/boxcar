#!/usr/bin/env bash
# Cross-compile all release targets into dist/ (no packaging, just binaries).
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="$(grep 'const version' cmd/boxcar/main.go | sed 's/.*"\(.*\)".*/\1/')"
STAGE=dist
mkdir -p "$STAGE"

build() {
  local os="$1" arch="$2" ext=""
  [ "$os" = "windows" ] && ext=".exe"
  echo "==> $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" -o "$STAGE/boxcar-$VERSION-$os-$arch$ext" ./cmd/boxcar
}

build linux amd64
build linux arm64
build windows amd64
build windows arm64
build darwin amd64
build darwin arm64

echo "==> darwin universal (lipo)"
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$STAGE/.boxcar-x86_64" ./cmd/boxcar
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$STAGE/.boxcar-arm64" ./cmd/boxcar
lipo -create "$STAGE/.boxcar-x86_64" "$STAGE/.boxcar-arm64" -output "$STAGE/boxcar-$VERSION-darwin-universal"
rm "$STAGE/.boxcar-x86_64" "$STAGE/.boxcar-arm64"
ls -la "$STAGE"
