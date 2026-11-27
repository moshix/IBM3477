#!/bin/bash
# Build webibm3477 (server) and appwebibm3477 (desktop app) for:
#   macOS universal (Intel + Apple Silicon in one binary, via lipo)
#   Linux amd64
#   Linux arm64
# Output: bin/<app>-<version>-<target>, plus bin/<app> (the macOS
# universal binary, for running on this Mac), and "bin/IBM3477.app", the
# native macOS app (tools/make-macos-app.bash) with bin/appwebibm3477 in it.
#
# The IBM 3477 ROMs are embedded: they are copied from this directory into
# internal/roms/files/ (gitignored) and picked up by go:embed. The partial
# 3486 dump is not embedded (it cannot boot).
# Requires Go; the macOS universal binary and the app also need lipo and
# swiftc (Xcode command line tools), so run this script on a Mac.
set -euo pipefail
cd "$(dirname "$0")"

# the one version number of all programs lives in internal/version/version.go
VERSION="$(sed -n 's/^const Number = "\(.*\)"$/\1/p' internal/version/version.go)"
[ -n "$VERSION" ] || { echo "error: no version in internal/version/version.go" >&2; exit 1; }
LDFLAGS=""
APPS="webibm3477 appwebibm3477"
EMBED_ROMS="ibm3477_part1.bin ibm3477_part2.bin"

for t in lipo swiftc; do
  command -v $t >/dev/null || { echo "error: $t not found (install the Xcode command line tools)" >&2; exit 1; }
done
for r in $EMBED_ROMS; do
  [ -f "$r" ] || { echo "error: ROM $r not found in $(pwd)" >&2; exit 1; }
  cp "$r" internal/roms/files/
done
echo "Embedding ROMs: $EMBED_ROMS"

echo "Vetting and testing..."
go vet ./...
go test -short ./...

mkdir -p bin
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
rm -rf bin/* # -r: the .app bundle is a directory
# build GOOS GOARCH APP OUT: pure Go, no cgo, so it cross-compiles anywhere
build() {
  echo "  $3: $1/$2"
  GOOS="$1" GOARCH="$2" CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$4" "./cmd/$3/"
}

for app in $APPS; do
  build darwin amd64 "$app" "$tmp/$app-amd64"
  build darwin arm64 "$app" "$tmp/$app-arm64"
  lipo -create -output "bin/$app-$VERSION-macos-universal" "$tmp/$app-amd64" "$tmp/$app-arm64"
  # convenience copy for running on this Mac: bin/webibm3477, bin/appwebibm3477
  cp "bin/$app-$VERSION-macos-universal" "bin/$app"
  build linux amd64 "$app" "bin/$app-$VERSION-linux-amd64"
  build linux arm64 "$app" "bin/$app-$VERSION-linux-arm64"
done

echo "  IBM3477.app: macOS app"
tools/make-macos-app.bash IBM_3477.jpg bin bin/appwebibm3477 >/dev/null

echo ""
echo "Done:"
for app in $APPS; do
  for t in macos-universal linux-amd64 linux-arm64; do
    f="bin/$app-$VERSION-$t"
    printf '  %-44s %s\n' "$f" "$(file -b "$f" | head -1 | cut -c1-60)"
  done
done
printf '  %-44s %s\n' "bin/IBM3477.app" "native macOS app (Cocoa), $VERSION"
