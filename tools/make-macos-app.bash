#!/bin/bash
# Build "IBM3477.app", the native macOS terminal: a Cocoa window
# (macos/IBM3477/main.swift, WKWebView) around the Go emulator, which runs
# hidden inside the bundle as "appwebibm3477 -embedded". No browser tab.
#
# usage: tools/make-macos-app.bash [image] [dest-dir] [server]
#   image     picture for the icon (default: IBM_3477.jpg in the repo root)
#   dest-dir  where to put "IBM3477.app" (default: bin/ in the repo)
#   server    appwebibm3477 binary to bundle (default: bin/appwebibm3477
#             from ./build.bash, else /usr/local/bin/appwebibm3477)
# Needs the Xcode command line tools (swiftc, lipo, iconutil).
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
img=${1:-$repo/IBM_3477.jpg}
dest=${2:-$repo/bin}
app="$dest/IBM3477.app"
server=${3:-}
if [ -z "$server" ]; then
  server=$repo/bin/appwebibm3477
  [ -x "$server" ] || server=/usr/local/bin/appwebibm3477
fi
[ -x "$server" ] || { echo "error: $server not found; run ./build.bash" >&2; exit 1; }
version=$(sed -n 's/^const Number = "\(.*\)"$/\1/p' "$repo/internal/version/version.go")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- Cocoa app ------------------------------------------------------------
# One binary for Intel and Apple Silicon, like build.bash does for Go.
# macOS 12 is the oldest with every WKDownload API main.swift uses.
for arch in arm64 x86_64; do
  swiftc -swift-version 5 -O -target "$arch-apple-macos12" \
    "$repo/macos/IBM3477/main.swift" -o "$work/IBM3477-$arch"
done
lipo -create -output "$work/IBM3477" "$work/IBM3477-arm64" "$work/IBM3477-x86_64"

# --- icon -----------------------------------------------------------------
# macOS icons must be square. Crop the picture to a centred square of its
# shorter side, then render every size an .iconset needs (16..1024 px, with
# @2x variants) and let iconutil pack them into one .icns file.
w=$(sips -g pixelWidth "$img" | awk '/pixelWidth/ {print $2}')
h=$(sips -g pixelHeight "$img" | awk '/pixelHeight/ {print $2}')
side=$(( w < h ? w : h ))
sips -s format png -c "$side" "$side" "$img" --out "$work/square.png" >/dev/null

set_dir="$work/app.iconset"
mkdir "$set_dir"
for s in 16 32 128 256 512; do
  sips -z $s $s "$work/square.png" --out "$set_dir/icon_${s}x${s}.png" >/dev/null
  d=$(( s * 2 ))
  sips -z $d $d "$work/square.png" --out "$set_dir/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$set_dir" -o "$work/app.icns"

# --- bundle ---------------------------------------------------------------
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$work/app.icns" "$app/Contents/Resources/app.icns"

# The app and the Go server it starts sit side by side in Contents/MacOS,
# so the bundle is self-contained and can be copied to another Mac.
cp "$work/IBM3477" "$app/Contents/MacOS/IBM3477"
cp "$server" "$app/Contents/MacOS/appwebibm3477"

cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>            <string>IBM 3477</string>
  <key>CFBundleDisplayName</key>     <string>IBM 3477</string>
  <key>CFBundleIdentifier</key>      <string>local.ibm3477.app</string>
  <key>CFBundleExecutable</key>      <string>IBM3477</string>
  <key>CFBundleIconFile</key>        <string>app.icns</string>
  <key>CFBundlePackageType</key>     <string>APPL</string>
  <key>CFBundleShortVersionString</key> <string>$version</string>
  <key>CFBundleVersion</key>         <string>$version</string>
  <key>LSMinimumSystemVersion</key>  <string>12.0</string>
  <key>NSHighResolutionCapable</key> <true/>
  <!-- the page comes from the bundled server on 127.0.0.1 over http -->
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key> <true/></dict>
</dict>
</plist>
EOF

# Ad-hoc signature over the whole bundle: Apple Silicon refuses to run
# unsigned code, and the copied server needs to be covered too.
codesign --force --deep -s - "$app"

# Make Finder/Dock notice the new icon instead of showing a cached one.
touch "$app"
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$app" 2>/dev/null || true

echo "built $app"
