package main

// Background start: appwebibm3477 returns the shell prompt at once. The
// program started from the shell (the "launcher") runs a copy of itself
// (the "server") in the background and relays the server's startup
// output (log lines, errors, the URL) until the server reports that it
// is serving and the browser was asked to open. The launcher then exits
// with status 0; the server keeps running and quits by itself through
// the idle monitor when the last browser tab closes.
//
// Launcher                         Server (env childEnv=1, own session)
//   start server, read its output ---- stdout+stderr pipe ---->
//   copy lines to the terminal  <----- startup log lines ------
//   see readyMark -> exit 0     <----- readyMark --------------
//                                      log -> log file from now on
// If the pipe closes without readyMark the server failed to start
// (log.Fatal); the launcher exits with the server's status.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// childEnv marks the background server process.
const childEnv = "APPWEBIBM3477_SERVER"

// readyMark is the line the server writes once it is up; it is never
// shown to the user.
const readyMark = "\x00appwebibm3477-ready"

// isServerChild reports whether this process is the background server.
func isServerChild() bool { return os.Getenv(childEnv) == "1" }

// logFilePath is where the background server logs after startup.
func logFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return os.DevNull
	}
	return filepath.Join(dir, "appwebibm3477", "appwebibm3477.log")
}

// runInBackground starts the server copy of this program with the same
// arguments and relays its startup output to out. It returns the exit
// status the launcher should end with.
func runInBackground(out io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, "appwebibm3477: cannot find own program:", err)
		return 1
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(out, "appwebibm3477: pipe:", err)
		return 1
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	// one pipe for both, so log lines and printed URL keep their order
	cmd.Stdout, cmd.Stderr = pw, pw
	detachAttrs(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(out, "appwebibm3477: cannot start server:", err)
		return 1
	}
	pw.Close() // only the server holds the write end now
	ready, err := relayUntilReady(pr, out)
	pr.Close()
	if ready {
		// the server lives on; Release frees our handle without waiting
		cmd.Process.Release()
		return 0
	}
	if err != nil {
		fmt.Fprintln(out, "appwebibm3477:", err)
	}
	// the server ended during startup: report its status
	var exit *exec.ExitError
	if werr := cmd.Wait(); errors.As(werr, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	return 1
}

// relayUntilReady copies lines from r to out until readyMark (true) or
// the end of r (false: the server stopped before it was ready).
func relayUntilReady(r io.Reader, out io.Writer) (bool, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == readyMark {
			return true, nil
		}
		fmt.Fprintln(out, line)
	}
	return false, sc.Err()
}

// serverReady is called by the background server once it serves and the
// browser was asked to open: it tells the launcher (which then exits)
// and sends all further output to the log file, since the pipe to the
// launcher is about to close.
func serverReady() {
	path := logFilePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if path != os.DevNull && f != nil {
		log.Print("running in the background; quits when the last tab closes")
		log.Printf("log: %s", shortenHome(path))
	}
	// a write to the closed pipe must fail quietly, not kill the server
	signal.Ignore(syscall.SIGPIPE)
	fmt.Fprintln(os.Stdout, readyMark)
	log.SetOutput(f)
	os.Stdout, os.Stderr = f, f
}

// shortenHome writes the home directory as ~ so the log path fits in 79
// columns more often.
func shortenHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}
