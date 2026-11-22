package main

import (
	"bytes"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestRelayUntilReady: startup lines are shown, the ready mark is not, and
// a stream that ends without the mark means the server failed.
func TestRelayUntilReady(t *testing.T) {
	var out bytes.Buffer
	ready, _ := relayUntilReady(strings.NewReader("a\nb\n"+readyMark+"\nlater\n"), &out)
	if !ready || out.String() != "a\nb\n" {
		t.Errorf("ready %v, out %q", ready, out.String())
	}
	out.Reset()
	ready, _ = relayUntilReady(strings.NewReader("fatal: no ROMs\n"), &out)
	if ready || out.String() != "fatal: no ROMs\n" {
		t.Errorf("ready %v, out %q", ready, out.String())
	}
}

// buildApp compiles appwebibm3477 into a temp directory.
func buildApp(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "appwebibm3477")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	return exe
}

// TestBackgroundStart runs the real program: it must return to the shell
// at once with the URL shown, keep serving in the background, and quit
// by itself once its last tab (here a presence socket) closes.
func TestBackgroundStart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the program")
	}
	exe := buildApp(t)
	cmd := exec.Command(exe, "-listen", "127.0.0.1:0", "-no-browser", "-idle-exit", "1s")
	// the log file goes to a temp cache dir, not the user's
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_CACHE_HOME="+t.TempDir())
	start := time.Now()
	out, err := cmd.CombinedOutput() // returns only when the launcher exits
	if err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("launcher took %v to return", d)
	}
	if strings.Contains(string(out), readyMark) {
		t.Error("ready mark shown to the user")
	}
	if !strings.Contains(string(out), "running in the background") {
		t.Errorf("no background notice:\n%s", out)
	}
	m := regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/\?t=\S+`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no URL in output:\n%s", out)
	}
	url, host := m[0], m[1]
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > 79 && !strings.Contains(line, "serving IBM") && !strings.Contains(line, url) {
			t.Errorf("line longer than 79 columns: %q", line)
		}
	}

	// the server still runs after the launcher exited: open the page
	// (token -> cookie) and hold a tab-presence socket like a browser tab
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("server not running after the launcher exited: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("page status %d", resp.StatusCode)
	}
	hdr := http.Header{"Origin": {"http://" + host}}
	for _, c := range jar.Cookies(resp.Request.URL) {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws://"+host+presencePath, hdr)
	if err != nil {
		t.Fatalf("presence socket: %v", err)
	}
	// "close the tab": the server must quit after the 1 s idle grace
	ws.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + host + "/"); err != nil {
			return // gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("server still running 10 s after its last tab closed")
}

// TestBackgroundStartFails: a startup error (no ROMs) reaches the shell
// and the launcher exits non-zero instead of leaving a dead server.
func TestBackgroundStartFails(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the program")
	}
	exe := buildApp(t)
	out, err := exec.Command(exe, "-romdir", t.TempDir(), "-no-browser").CombinedOutput()
	if err == nil {
		t.Fatalf("launcher succeeded without ROMs:\n%s", out)
	}
	if !strings.Contains(string(out), "ROMs not found") {
		t.Errorf("error not shown:\n%s", out)
	}
}
