package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAnnounceURL: the URL line is exactly prefix + URL, one line.
func TestAnnounceURL(t *testing.T) {
	var b bytes.Buffer
	announceURL(&b, "http://127.0.0.1:1/?t=x")
	if got := b.String(); got != "APPURL http://127.0.0.1:1/?t=x\n" {
		t.Errorf("got %q", got)
	}
}

// TestWatchParent: stop is called once the reader ends, not before.
func TestWatchParent(t *testing.T) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	watchParent(pr, func() { close(done) })
	pw.Write([]byte("ignored"))
	select {
	case <-done:
		t.Fatal("stopped while the parent is still there")
	case <-time.After(50 * time.Millisecond):
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not stopped after EOF")
	}
}

// TestEmbeddedMode runs the real program the way the macOS app does: it
// must stay in the foreground, print the URL line, serve the page, and
// quit as soon as its stdin (the app) goes away.
func TestEmbeddedMode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the program")
	}
	exe := buildApp(t)
	cmd := exec.Command(exe, "-embedded", "-listen", "127.0.0.1:0")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// the first APPURL line is the page; other lines are ignored
	url := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), urlPrefix); ok {
			url = rest
			break
		}
	}
	if url == "" {
		t.Fatal("no URL line")
	}
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(url)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("page status %d", resp.StatusCode)
	}

	// "quit the app": closing stdin must end the server promptly
	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("server still running 10 s after stdin closed")
	}
}
