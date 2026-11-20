// Command webibm3477 is the multi-user server: it serves emulated IBM
// InfoWindow twinax terminals (3477, and the 3486 once a complete ROM
// dump exists) to any browser that can reach it. Each browser tab gets
// its own emulated terminal running the original firmware.
//
// Usage: webibm3477 [-listen :8050] [-model 3477] [-romdir DIR]
//
//	[-host pub400.com] [-port 23] [-lock]
//
// With -lock every terminal connects to -host:-port only (which must
// then be given explicitly); the host and port typed in the page are
// ignored, so visitors cannot use the server to reach other addresses.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"ibm3486/internal/board"
	release "ibm3486/internal/version"
	"ibm3486/internal/web"
)

// version is the release of all programs (internal/version)
var version = release.Number

func main() {
	listen := flag.String("listen", ":8050", "HTTP listen address")
	model := flag.String("model", "3477", "default terminal model (3477 or 3486)")
	romDir := flag.String("romdir", "", "directory holding the ROM files "+
		"(default: the ROMs built into this program)")
	host := flag.String("host", "pub400.com", "default TN5250 host shown in the page")
	port := flag.Int("port", 23, "default TN5250 port")
	hostTLS := flag.Bool("host-tls", false, "connect to the TN5250 host over TLS (usually port 992)")
	lock := flag.Bool("lock", false, "connect only to -host:-port (requires -host); "+
		"the host and port entered in the page are ignored")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("webibm3477", version)
		return
	}
	if *lock {
		hostGiven := false
		flag.Visit(func(f *flag.Flag) { hostGiven = hostGiven || f.Name == "host" })
		if !hostGiven || *host == "" {
			log.Fatal("-lock requires -host: the host every terminal is locked to")
		}
		if *port < 1 || *port > 65535 {
			log.Fatalf("-port %d is not a valid TCP port", *port)
		}
	}
	_, where, err := board.ROMSource(*romDir, *model)
	if err != nil {
		log.Fatal(err)
	}
	srv := web.New(web.Config{
		Model: *model, RomDir: *romDir, Host: *host, Port: *port, Version: version, HostTLS: *hostTLS,
		Lock: *lock,
		// print jobs on this computer's printers, but not for the
		// visitors of a locked public server
		LocalPrinters: !*lock,
	})
	log.Printf("webibm3477 %s on %s (model %s, ROMs: %s)",
		version, *listen, *model, where)
	if *lock {
		log.Printf("locked to TN5250 host %s:%d", *host, *port)
	}
	log.Fatal(http.ListenAndServe(*listen, srv.Handler()))
}
