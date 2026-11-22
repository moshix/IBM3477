// Command appwebibm3477 is the desktop launcher: it runs the emulated
// IBM InfoWindow terminal for this computer only (loopback), opens it in
// the default browser, and quits a while after the last browser tab
// closes. Adapted from appweb5250 (../web5250/cmd/appweb5250).
//
// Usage: appwebibm3477 [-listen 127.0.0.1:53477] [-model 3477]
//
//	[-romdir DIR] [-host h] [-port p] [-idle-exit 30s] [-no-browser]
//	[-foreground]
//
// It returns to the shell as soon as the terminal is serving; the server
// runs on in the background until its last browser tab closes
// (detach.go). -foreground (or -idle-exit 0) keeps it in the shell.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ibm3486/internal/board"
	release "ibm3486/internal/version"
	"ibm3486/internal/web"
)

// version is the release of all programs (internal/version)
var version = release.Number

// defaultListen is fixed (not port 0) so the page's browser-side settings,
// which are per origin including the port, survive restarts.
const defaultListen = "127.0.0.1:53477"

// startupTimeout: exit if no browser ever connects.
const startupTimeout = 5 * time.Minute

func main() {
	listenAddr := flag.String("listen", defaultListen, "loopback address to serve on")
	model := flag.String("model", "3477", "terminal model (3477 or 3486)")
	romDir := flag.String("romdir", "", "directory holding the ROM files "+
		"(default: the ROMs built into this program)")
	host := flag.String("host", "pub400.com", "default TN5250 host")
	port := flag.Int("port", 23, "default TN5250 port")
	hostTLS := flag.Bool("host-tls", false, "connect to the TN5250 host over TLS (usually port 992)")
	idle := flag.Duration("idle-exit", 30*time.Second,
		"quit this long after the last tab closes (0 = never)")
	noBrowser := flag.Bool("no-browser", false, "only print the URL")
	showVersion := flag.Bool("version", false, "print the version and exit")
	foreground := flag.Bool("foreground", false,
		"stay in the shell until quit (default: return to the shell at once)")
	flag.Parse()
	if *showVersion {
		fmt.Println("appwebibm3477", version)
		return
	}
	// With -idle-exit 0 nothing would ever stop a background server, so
	// it then stays in the foreground, where Ctrl-C ends it.
	if !*foreground && *idle > 0 && !isServerChild() {
		os.Exit(runInBackground(os.Stdout))
	}
	if err := validateListen(*listenAddr); err != nil {
		log.Fatal(err)
	}
	_, where, err := board.ROMSource(*romDir, *model)
	if err != nil {
		log.Fatal(err)
	}

	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "listen" })
	ln, err := listen(*listenAddr, explicit)
	if err != nil {
		log.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)

	token, err := randomSecret(24)
	if err != nil {
		log.Fatal(err)
	}
	cookie, err := randomSecret(24)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mon := newIdleMonitor(*idle, func(reason string) {
		log.Printf("exiting: %s", reason)
		stop()
	})

	srv := web.New(web.Config{
		Model: *model, RomDir: *romDir, Host: *host, Port: *port, Version: version, HostTLS: *hostTLS,
		ExtraHead: presenceScript,
		// the desktop app prints on the printers of the user's own computer
		LocalPrinters: true,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.Page)
	mux.Handle("/static/", srv.Handler())
	mux.HandleFunc("/ws", requireSameOrigin(countWS(mon, srv.WS)))
	mux.HandleFunc(presencePath, requireSameOrigin(presenceHandler(mon)))
	gate := newAppGate(addr, token, cookie, "/", mux)
	hs := &http.Server{Handler: gate, ReadHeaderTimeout: 10 * time.Second}

	url := fmt.Sprintf("http://%s/?t=%s", addr.String(), token)
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("appwebibm3477 %s serving IBM %s on %s (ROMs: %s)", version, *model, addr, where)
	mon.start(startupTimeout)
	if *noBrowser {
		printManualURL(url)
	} else {
		openBrowser(url)
	}
	if isServerChild() {
		serverReady()
	}
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hs.Shutdown(shut)
}
