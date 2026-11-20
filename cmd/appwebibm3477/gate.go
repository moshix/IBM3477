package main

// Access control, idle exit, tab presence and browser launch for the
// desktop app. Adapted from appweb5250 (../web5250/cmd/appweb5250), which
// documents the threat model in more detail.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// fallbackListen is used when the fixed default port is taken.
const fallbackListen = "127.0.0.1:0"

// listenIsLoopback reports whether a host:port binds only to loopback.
func listenIsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// validateListen refuses non-loopback addresses: the app connects to any
// host the page names, so it must never be reachable from other machines.
func validateListen(listen string) error {
	if !listenIsLoopback(listen) {
		return fmt.Errorf("-listen %q is not a loopback host:port; use 127.0.0.1:PORT", listen)
	}
	return nil
}

// listen binds addr; when the default was not overridden and is busy, it
// falls back to an OS-chosen loopback port.
func listen(addr string, explicit bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil || explicit {
		return ln, err
	}
	if ln2, err2 := net.Listen("tcp", fallbackListen); err2 == nil {
		return ln2, nil
	}
	return nil, err
}

// randomSecret returns n random bytes, base64url encoded.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func secretEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// appGate protects every route:
//   - Host must be exactly our loopback address (defeats DNS rebinding);
//   - the browser is sent to /?t=<token>, which sets a SameSite=Strict
//     session cookie and redirects; every other request needs the cookie
//     (defeats other local web pages driving the terminal).
type appGate struct {
	token, cookieName, cookieValue string
	allowedHosts                   map[string]bool
	landing                        string
	next                           http.Handler
}

func newAppGate(addr *net.TCPAddr, token, cookieValue, landing string, next http.Handler) *appGate {
	port := strconv.Itoa(addr.Port)
	return &appGate{
		token: token,
		// cookies are not port-scoped; a per-port name keeps two
		// instances from overwriting each other's session
		cookieName:  "appwebibm3477_" + port,
		cookieValue: cookieValue,
		allowedHosts: map[string]bool{
			net.JoinHostPort(addr.IP.String(), port): true,
			"localhost:" + port:                      true,
		},
		landing: landing,
		next:    next,
	}
}

func (g *appGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.allowedHosts[strings.ToLower(r.Host)] {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/" && r.URL.Query().Has("t") {
		if !secretEqual(r.URL.Query().Get("t"), g.token) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: g.cookieName, Value: g.cookieValue, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, g.landing, http.StatusFound)
		return
	}
	c, err := r.Cookie(g.cookieName)
	if err != nil || !secretEqual(c.Value, g.cookieValue) {
		http.Error(w, "Forbidden - open the URL printed by appwebibm3477", http.StatusForbidden)
		return
	}
	g.next.ServeHTTP(w, r)
}

// strictSameOrigin requires a browser Origin equal to http://<Host>. A page
// on another local port is same-site (so it gets our cookie) but never has
// our Origin, so this check must stay.
func strictSameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Scheme == "http" && strings.EqualFold(u.Host, r.Host)
}

func requireSameOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) && !strictSameOrigin(r) {
			http.Error(w, "Forbidden origin", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// ---- idle exit ------------------------------------------------------------

// idleMonitor counts open WebSockets (terminals and tab-presence sockets)
// and calls exit when none has been open for `grace`, or when no browser
// connected within the startup timeout. grace 0 disables both.
type idleMonitor struct {
	mu     sync.Mutex
	active int
	gen    int // bumped on every (dis)arm so a stale timer does nothing
	timer  *time.Timer
	grace  time.Duration
	exit   func(reason string)
}

func newIdleMonitor(grace time.Duration, exit func(string)) *idleMonitor {
	return &idleMonitor{grace: grace, exit: exit}
}

// arm (re)starts the exit timer; called with mu held.
func (m *idleMonitor) arm(d time.Duration, reason string) {
	m.disarm()
	gen := m.gen
	m.timer = time.AfterFunc(d, func() {
		m.mu.Lock()
		fire := m.gen == gen && m.active == 0
		m.mu.Unlock()
		if fire {
			m.exit(reason)
		}
	})
}

// disarm cancels the timer; called with mu held.
func (m *idleMonitor) disarm() {
	m.gen++
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}

func (m *idleMonitor) start(startup time.Duration) {
	if m.grace <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.arm(startup, "no browser connected")
}

func (m *idleMonitor) opened() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active++
	m.disarm()
}

func (m *idleMonitor) closed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active > 0 {
		m.active--
	}
	if m.active == 0 && m.grace > 0 {
		m.arm(m.grace, "browser tab closed")
	}
}

// countWS brackets a WebSocket handler (which blocks for the session) with
// opened/closed.
func countWS(m *idleMonitor, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			h(w, r)
			return
		}
		m.opened()
		defer m.closed()
		h(w, r)
	}
}

// ---- tab presence ---------------------------------------------------------

// The page holds this extra WebSocket open for as long as the tab exists
// (reconnecting with back-off), so the app does not quit while the tab is
// open but no terminal is powered on.
const presencePath = "/alive"

const presenceScript = `<script>(function(){var d=1000;function c(){` +
	`var w=new WebSocket((location.protocol==="https:"?"wss:":"ws:")+"//"+` +
	`location.host+"` + presencePath + `");w.onopen=function(){d=1000};` +
	`w.onclose=function(){setTimeout(c,d);d=Math.min(d*2,10000)}}c()})();` +
	`</script>`

var presenceUpgrader = websocket.Upgrader{ReadBufferSize: 256, WriteBufferSize: 256}

func presenceHandler(m *idleMonitor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := presenceUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(512)
		m.opened()
		defer m.closed()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}
}

// ---- browser launch -------------------------------------------------------

// browserCommand returns the program that opens url on goos; the URL is
// a separate argument, never passed through a shell.
func browserCommand(goos, url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}

// haveDisplay: on a headless Linux/BSD box xdg-open could start a text
// browser in this terminal, so it is not tried there.
func haveDisplay(goos string) bool {
	if goos == "darwin" || goos == "windows" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// printManualURL prints the URL on its own line (lines stay < 80 cols).
func printManualURL(url string) {
	fmt.Println("Open this URL in your browser:")
	fmt.Println("  " + url)
}

func openBrowser(url string) {
	if !haveDisplay(runtime.GOOS) {
		printManualURL(url)
		return
	}
	name, args := browserCommand(runtime.GOOS, url)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		log.Printf("could not start %s", name)
		printManualURL(url)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("%s failed: %v", name, err)
			printManualURL(url)
		}
	}()
}
