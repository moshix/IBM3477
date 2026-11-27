#!/bin/bash
# Smoke test of the native Linux app tarball (tools/make-linux-app.bash) on
# a virtual display (Xvfb) in a Linux container:
#   1. install.sh installs it, as a user would;
#   2. the app starts its server, which serves the page; the page loads;
#   3. clicking Power on and Connect (xdotool) opens a TN5250 session
#      to HOST:PORT;
#   4. a screenshot of the display is saved (SHOT, to look at);
#   5. quitting the app (SIGTERM) stops the server;
#   6. killing the app (SIGKILL) also stops the server.
#
# usage: tools/test-linux-app.bash TARBALL [SHOT] [HOST] [PORT]
#   TARBALL  bin/ibm3477-linux-<version>-<arch>.tar.gz
#   SHOT     where to save the screenshot PNG (default: none)
#   HOST     TN5250 host typed into the page (default: pub400.com)
#   PORT     its port (default: 23)
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tarball=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
shot=${2:-}
host=${3:-pub400.com}
port=${4:-23}
arch=$(basename "$tarball" .tar.gz); arch=${arch##*-}

# The runtime image: only what a user's desktop has (WebKitGTK runtime),
# plus Xvfb and ImageMagick for the virtual display and the screenshot,
# xdotool to click, and iproute2 (ss) to see the TN5250 connection.
# The tag's -2 marks this package list; change it when the list changes.
# Made with run + commit for the reason given in make-linux-app.bash.
image=ibm3477-gtk-test:$arch-2
if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "making the $arch test image (once; takes a few minutes)" >&2
  cid=$(docker create --platform "linux/$arch" ubuntu:22.04 sh -c \
    'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y \
       --no-install-recommends libwebkit2gtk-4.1-0 xvfb xauth imagemagick \
       procps ca-certificates xdotool iproute2 && rm -rf /var/lib/apt/lists/*')
  docker start -a "$cid" >/dev/null
  docker commit "$cid" "$image" >/dev/null
  docker rm "$cid" >/dev/null
fi

# Shared with the container: the tarball in, the screenshot out. Inside
# the repo because Docker on a Mac shares only the home folder.
mkdir -p "$repo/.build"
io=$(mktemp -d "$repo/.build/test.XXXXXX")
trap 'rm -rf "$io"' EXIT
cp "$tarball" "$io/app.tar.gz"

# The container's bubblewrap sandbox cannot start inside Docker (no user
# namespaces), and Xvfb has no GPU: both are switched off for the test
# only, through WebKit's own environment variables.
status=0
docker run --rm --platform "linux/$arch" -v "$io:/io" \
  -e HOST="$host" -e PORT="$port" \
  -e WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS=1 \
  -e WEBKIT_DISABLE_COMPOSITING_MODE=1 \
  "$image" bash -c '
set -u
# fail: report, show the app log, and keep a screenshot of the moment
fail() { echo "FAIL: $*"; cat /tmp/app.log 2>/dev/null
  [ -n "${DISPLAY:-}" ] && import -window root /io/shot.png; exit 1; }
# wait_for SECONDS COMMAND...: poll until COMMAND succeeds
wait_for() { local n=$(( $1 * 5 )); shift
  for _ in $(seq $n); do "$@" && return 0; sleep 0.2; done; return 1; }
# -x: the process name exactly; -f would also match this very script,
# whose command line contains the server name
server_up()   { pgrep -x appwebibm3477 >/dev/null; }
server_gone() { ! server_up; }

cd /tmp && tar xzf /io/app.tar.gz && cd ibm3477-linux-*/
./install.sh || fail "install.sh"
[ -x ~/.local/bin/ibm3477 ] || fail "not installed"
grep -q "^Exec=$HOME/.local/lib/ibm3477/ibm3477$" \
  ~/.local/share/applications/ibm3477.desktop || fail ".desktop Exec"

Xvfb :99 -screen 0 1400x900x24 >/dev/null 2>&1 &
export DISPLAY=:99
sleep 1

# --- start, page, screenshot, quit ---
~/.local/bin/ibm3477 >/tmp/app.log 2>&1 &
app=$!
wait_for 20 server_up || fail "server did not start"
echo "ok: server started"
# emulated CPUs are slow: give WebKit time to load the page
wait_for 120 grep -q "page loaded" /tmp/app.log || fail "page did not load"
echo "ok: $(grep "page loaded" /tmp/app.log)"
sleep 2

# --- power on and connect, like a user ---
# The window sits at 0,0 without a window manager, so page coordinates
# are screen coordinates. The fields and buttons are found by position
# in the toolbar, measured from a screenshot of this exact layout.
click() { xdotool mousemove "$1" "$2" click 1; sleep 0.3; }
# host field: select all, type the host; port field likewise
click 480 21; xdotool key ctrl+a type --delay 20 "$HOST"
click 656 21; xdotool key ctrl+a type --delay 20 "$PORT"
click 260 21   # Power on
sleep 3
click 1052 21  # Connect
tn5250_up() { ss -tn state established "( dport = :$PORT )" | grep -q ":$PORT"; }
wait_for 30 tn5250_up || fail "no TN5250 connection to $HOST:$PORT"
echo "ok: connected to $HOST:$PORT"
sleep 15       # let the host send its sign-on screen
import -window root /io/shot.png && echo "ok: screenshot"
# make the window smaller, as a user would, before quitting
xdotool search --sync --name "IBM InfoWindow" windowsize 1100 700
sleep 1
kill -TERM $app
wait_for 10 server_gone || fail "server still running after quit"
echo "ok: quitting the app stopped the server"
ini=~/.config/ibm3477/window.ini
grep -q "^width=1100$" $ini && grep -q "^height=700$" $ini ||
  fail "window size not saved: $(cat $ini 2>/dev/null)"
echo "ok: window size saved"

# --- start again, kill hard ---
~/.local/bin/ibm3477 >/tmp/app.log 2>&1 &
app=$!
wait_for 20 server_up || fail "server did not start (2nd)"
win_size() { xdotool search --name "IBM InfoWindow" getwindowgeometry |
  grep -q "Geometry: 1100x700"; }
wait_for 30 win_size || fail "saved window size not used at start"
echo "ok: saved window size used at start"
kill -KILL $app
wait_for 10 server_gone || fail "server still running after kill -9"
echo "ok: killing the app stopped the server"
echo PASS
' || status=$?
# the screenshot is kept also when the test failed: it shows why
if [ -n "$shot" ] && [ -f "$io/shot.png" ]; then
  cp "$io/shot.png" "$shot"
fi
exit $status
