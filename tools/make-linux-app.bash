#!/bin/bash
# Build the native Linux app: a GTK window with a WebKitGTK web view
# (linux/ibm3477-gtk/main.c) around the Go emulator, which runs hidden as
# "appwebibm3477 -embedded". No browser tab. The Linux twin of
# tools/make-macos-app.bash.
#
# usage: tools/make-linux-app.bash ARCH SERVER [OUTDIR]
#   ARCH    amd64 or arm64
#   SERVER  the Linux appwebibm3477 binary of that ARCH to bundle
#   OUTDIR  where the tarball goes (default: bin/ in the repo)
# Output: OUTDIR/ibm3477-linux-<version>-<arch>.tar.gz holding
#   ibm3477, appwebibm3477, ibm3477.png, ibm3477.desktop, install.sh
#
# C code using GTK cannot be cross-compiled from macOS like the Go code,
# so it is compiled in a Linux container (Docker; on a Mac e.g. Colima).
# Ubuntu 22.04 is used on purpose: the program then runs on every
# distribution with glibc 2.35 or newer and WebKitGTK 4.1 (Ubuntu 22.04+,
# Debian 12+, Fedora 36+). Users need the WebKitGTK 4.1 runtime library.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
arch=${1:?usage: make-linux-app.bash ARCH SERVER [OUTDIR]}
server=${2:?usage: make-linux-app.bash ARCH SERVER [OUTDIR]}
out=${3:-$repo/bin}
case $arch in amd64|arm64) ;; *) echo "error: ARCH must be amd64 or arm64" >&2; exit 1 ;; esac
[ -x "$server" ] || { echo "error: $server not found" >&2; exit 1; }
version=$(sed -n 's/^const Number = "\(.*\)"$/\1/p' "$repo/internal/version/version.go")
name=ibm3477-linux-$version-$arch

# The work folder is inside the repo, not in $TMPDIR: Docker on a Mac
# (Colima, Docker Desktop) shares only the home folder with its Linux VM,
# so a container could not write to /var/folders/...
mkdir -p "$repo/.build"
work=$(mktemp -d "$repo/.build/linux.XXXXXX")
trap 'rm -rf "$work"' EXIT
pkg=$work/$name
mkdir -p "$pkg"

# --- compile in a container -------------------------------------------------
# The build image (compiler + GTK/WebKit headers) is made once per arch
# and kept by Docker, so later builds skip the slow apt-get. It is made
# with "docker run --platform" + "docker commit", not "docker build":
# Docker's legacy builder (the default without buildx, e.g. with Colima)
# ignores --platform and quietly builds for the host's arch instead.
image=ibm3477-gtk-build:$arch
if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "  making the $arch build image (once; takes a few minutes)" >&2
  cid=$(docker create --platform "linux/$arch" ubuntu:22.04 sh -c \
    'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y \
       --no-install-recommends gcc libc6-dev pkg-config libwebkit2gtk-4.1-dev \
     && rm -rf /var/lib/apt/lists/*')
  docker start -a "$cid" >/dev/null
  docker commit "$cid" "$image" >/dev/null
  docker rm "$cid" >/dev/null
fi
# make sure the cached image really is for $arch
img_arch=$(docker image inspect -f '{{.Architecture}}' "$image")
[ "$img_arch" = "$arch" ] || { echo "error: $image is $img_arch, not $arch" >&2; exit 1; }
# source read-only, output to the package folder; -Werror keeps it clean
docker run --rm --platform "linux/$arch" \
  -v "$repo/linux/ibm3477-gtk:/src:ro" -v "$pkg:/out" "$image" \
  sh -c 'gcc -O2 -Wall -Wextra -Werror -o /out/ibm3477 /src/main.c \
           $(pkg-config --cflags --libs webkit2gtk-4.1) && strip /out/ibm3477'

# --- package ------------------------------------------------------------------
cp "$server" "$pkg/appwebibm3477"
chmod 755 "$pkg/ibm3477" "$pkg/appwebibm3477"

# Icon: the terminal photo cropped to a centred square, 256 px (sips is
# macOS; this script runs on the Mac like the rest of build.bash).
img=$repo/IBM_3477.jpg
w=$(sips -g pixelWidth "$img" | awk '/pixelWidth/ {print $2}')
h=$(sips -g pixelHeight "$img" | awk '/pixelHeight/ {print $2}')
side=$(( w < h ? w : h ))
sips -s format png -c "$side" "$side" "$img" --out "$work/square.png" >/dev/null
sips -z 256 256 "$work/square.png" --out "$pkg/ibm3477.png" >/dev/null

# The desktop entry; install.sh fills in the real path for @DIR@.
cat > "$pkg/ibm3477.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=IBM 3477
Comment=IBM InfoWindow 3477 twinax terminal (5250) emulator
Exec=@DIR@/ibm3477
Icon=@DIR@/ibm3477.png
Terminal=false
Categories=System;TerminalEmulator;
StartupWMClass=ibm3477
EOF

cat > "$pkg/install.sh" <<'EOF'
#!/bin/sh
# Install the IBM 3477 app for this user (no root needed):
#   ~/.local/lib/ibm3477/      the programs and the icon
#   ~/.local/bin/ibm3477       link, to start it from a shell
#   ~/.local/share/applications/ibm3477.desktop   the app menu entry
# ./install.sh --uninstall removes all of it again.
set -e
lib=$HOME/.local/lib/ibm3477
bin=$HOME/.local/bin
apps=$HOME/.local/share/applications
if [ "${1:-}" = "--uninstall" ]; then
  rm -rf "$lib" "$bin/ibm3477" "$apps/ibm3477.desktop"
  echo "IBM 3477 removed."
  exit 0
fi
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$lib" "$bin" "$apps"
cp "$here/ibm3477" "$here/appwebibm3477" "$here/ibm3477.png" "$lib/"
ln -sf "$lib/ibm3477" "$bin/ibm3477"
sed "s|@DIR@|$lib|g" "$here/ibm3477.desktop" > "$apps/ibm3477.desktop"
command -v update-desktop-database >/dev/null &&
  update-desktop-database "$apps" 2>/dev/null || true
echo "IBM 3477 installed: find it in your app menu, or run ibm3477."
# the one library the app needs from the system
if command -v ldconfig >/dev/null && ! ldconfig -p | grep -q libwebkit2gtk-4.1; then
  echo "Missing WebKitGTK 4.1. Install it with:"
  echo "  Debian/Ubuntu: sudo apt install libwebkit2gtk-4.1-0"
  echo "  Fedora:        sudo dnf install webkit2gtk4.1"
fi
EOF
chmod 755 "$pkg/install.sh"

# COPYFILE_DISABLE: no macOS ._ metadata files in the tarball
mkdir -p "$out"
COPYFILE_DISABLE=1 tar -C "$work" -czf "$out/$name.tar.gz" "$name"
echo "built $out/$name.tar.gz"
