package main

// Embedded mode (-embedded): appwebibm3477 runs as the hidden server of the
// native macOS app (macos/IBM3477/main.swift) instead of a browser tab.
//
//	Cocoa app                          appwebibm3477 -embedded
//	  start server, pipe stdin+stdout ---->
//	  read lines until urlPrefix   <------ "APPURL http://127.0.0.1:…/?t=…"
//	  load URL in its WKWebView           serve the page as usual
//	  quit (or crash): stdin closes ----> EOF on stdin -> shut down
//
// The server's lifetime is tied to the app through stdin: when the app ends
// in any way, even killed, the kernel closes the pipe and the server stops.
// So there is no background detach, no browser launch and no idle exit.

import (
	"fmt"
	"io"
)

// urlPrefix starts the one stdout line that carries the page URL. The app
// ignores every other line (log output goes to stderr anyway).
const urlPrefix = "APPURL "

// embeddedListen is the default address in embedded mode, one above the
// browser mode's defaultListen so both can run at the same time.
const embeddedListen = "127.0.0.1:53478"

// announceURL writes the URL line for the app.
func announceURL(w io.Writer, url string) {
	fmt.Fprintln(w, urlPrefix+url)
}

// watchParent calls stop once r (the app's end of the stdin pipe) reaches
// EOF or fails, i.e. when the app has gone. Anything the app writes is
// ignored; it is only read so EOF can be noticed.
func watchParent(r io.Reader, stop func()) {
	go func() {
		io.Copy(io.Discard, r)
		stop()
	}()
}
