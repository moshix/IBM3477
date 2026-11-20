package web

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPageInjectsDefaults(t *testing.T) {
	srv := New(Config{Model: "3477", Host: "pub400.com", Port: 23})
	rec := httptest.NewRecorder()
	srv.Page(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "window.WEBTWINAX_DEFAULTS=") {
		t.Fatalf("status %d, defaults not injected", rec.Code)
	}
	if !strings.Contains(body, `"host":"pub400.com"`) {
		t.Fatal("default host pub400.com missing from page")
	}
}

func TestSameOrigin(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost:8050/ws", nil)
	r.Host = "localhost:8050"
	r.Header.Set("Origin", "http://localhost:8050")
	if !sameOrigin(r) {
		t.Fatal("same-origin request rejected")
	}
	r.Header.Set("Origin", "http://evil.example")
	if sameOrigin(r) {
		t.Fatal("cross-origin request accepted")
	}
}

func TestTerminalTypePerModel(t *testing.T) {
	cases := []struct {
		model, size, want string
		ok                bool
	}{
		{"3477", "24x80", "IBM-3179-2", true},
		{"3477", "27x132", "IBM-3477-FC", true},
		{"3486", "24x80", "IBM-3179-2", true},
		{"3486", "27x132", "", false}, // 3486 is locked to 24x80
	}
	for _, c := range cases {
		got, err := terminalType(c.model, c.size)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%s %s: got %q, %v", c.model, c.size, got, err)
		}
	}
}

// TestPowerOnOverWebSocket boots a 3477 through the WebSocket protocol
// and expects the font and a screen frame back.
func TestPowerOnOverWebSocket(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	web := New(Config{Model: "3477", RomDir: "../.."})
	srv := httptest.NewServer(web.Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(clientMsg{Type: "power", Model: "3477"}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var gotFont, gotScreen bool
	for !(gotFont && gotScreen) {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("font=%v screen=%v: %v", gotFont, gotScreen, err)
		}
		switch m.Type {
		case "font":
			gotFont = len(m.Font) > 0
		case "screen":
			gotScreen = m.Rows > 0 && m.Cols > 0 && len(m.Cells) > 0
		}
	}
}

// TestPowerOffOverWebSocket: "poweroff" stops the terminal and reports it.
func TestPowerOffOverWebSocket(t *testing.T) {
	srv := httptest.NewServer(New(Config{Model: "3477", RomDir: "../.."}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(clientMsg{Type: "poweroff"}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m serverMsg
	if err := ws.ReadJSON(&m); err != nil || m.Type != "status" || m.Text != "Powered off" {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// TestSettingsUIServed: the page has the Settings dialog and terminal.js
// carries the web5250-style keymap and the 4:3 screen lock.
func TestSettingsUIServed(t *testing.T) {
	srv := httptest.NewServer(New(Config{Model: "3477"}).Handler())
	defer srv.Close()
	get := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/")
	for _, want := range []string{`id="btn-settings"`, `id="keymap-list"`, `id="btn-keymap-reset"`, `id="codepage-select"`, `value="273"`, `id="setup-link" href="#" style="display:none"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	js := get("/static/terminal.js")
	for _, want := range []string{"DEFAULT_KEYMAP", "'Shift+F1': 'PF13'", "SCREEN_ASPECT = 4 / 3", "'fn:' + fn",
		"getElementById('setup-link').style.display = on ? '' : 'none'"} {
		if !strings.Contains(js, want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
}

// TestConnectToHost powers on a 3477, connects it to a local TCP listener
// standing in for the AS/400, and expects the session to come up and go
// down cleanly.
func TestConnectToHost(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	srv := httptest.NewServer(New(Config{Model: "3477", RomDir: "../.."}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ws.WriteJSON(clientMsg{Type: "power", Model: "3477"})
	ws.WriteJSON(clientMsg{Type: "connect", Host: host, Port: port, Size: "24x80"})
	waitStatus := func(prefix string) {
		t.Helper()
		ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			var m serverMsg
			if err := ws.ReadJSON(&m); err != nil {
				t.Fatalf("waiting for %q: %v", prefix, err)
			}
			if m.Type == "status" && strings.HasPrefix(m.Text, prefix) {
				if len(m.Text) > 79 {
					t.Errorf("status wider than 79 columns: %q", m.Text)
				}
				return
			}
		}
	}
	waitStatus("Connected to " + ln.Addr().String())
	// the byte counters arrive while the session runs
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("no traffic message: %v", err)
		}
		if m.Type == "traffic" {
			break
		}
	}
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("host never saw the connection")
	}
	ws.WriteJSON(clientMsg{Type: "disconnect"})
	waitStatus("Disconnected")
}

// TestConnectButtonStyle: the Connect button has web5250's teal colour
// and turns red (class "disconnect") while connected.
func TestConnectButtonStyle(t *testing.T) {
	css, _ := staticFiles.ReadFile("static/style.css")
	js, _ := staticFiles.ReadFile("static/terminal.js")
	for _, want := range []string{"#btn-connect {", "#4ec9b0", "#btn-connect.disconnect", "#f44747",
		"min-height: 0", "#toolbar, #statusbar { flex-shrink: 0; }"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	if !strings.Contains(string(js), "connectBtn.className = on ? 'disconnect' : ''") {
		t.Error("terminal.js does not switch the button to the disconnect style")
	}
}

// TestTerminalJSRuns executes terminal.js against a minimal fake DOM with
// Node (when installed), catching runtime errors such as a reference to a
// removed variable, which would leave the page dead (no power button).
func TestTerminalJSRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "testdata/domcheck.js", "static/terminal.js").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ran OK") {
		t.Fatalf("terminal.js failed in the fake DOM: %s %v", out, err)
	}
}

// TestMonoAttributeTable: the colour attributes 0x38-0x3E (pink/blue,
// e.g. pub400's sign-on labels in 0x3A) neither blink nor show column
// separators; red 0x2A blinks and turquoise 0x30 has separators, as in
// IBM's 5250 attribute table.
func TestMonoAttributeTable(t *testing.T) {
	js, _ := staticFiles.ReadFile("static/terminal.js")
	src := string(js)
	start := strings.Index(src, "const MONO_ATTR = [")
	end := strings.Index(src[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("MONO_ATTR table not found")
	}
	var styles []string
	for _, f := range strings.Split(src[start:start+end], "'") {
		if strings.TrimSpace(f) != "," && !strings.Contains(f, "const") &&
			!strings.Contains(f, "//") && !strings.Contains(f, "\n") {
			styles = append(styles, f)
		}
	}
	if len(styles) != 32 {
		t.Fatalf("table has %d entries, want 32: %q", len(styles), styles)
	}
	check := func(attr int, want string) {
		if got := styles[attr&0x1F]; got != want {
			t.Errorf("attribute %02X style %q, want %q", attr, got, want)
		}
	}
	check(0x23, "hr") // high intensity + reverse
	check(0x3A, "")   // blue: plain
	check(0x2A, "b")  // red blink
	check(0x30, "c")  // turquoise: column separators
	check(0x24, "u")  // underlined input field
	check(0x27, "n")  // non-display
}

// TestAttrStyleRules: host attributes count without bit 5, column
// separators are never drawn (as on a real 3477), the status row is plain
// apart from its top line.
func TestAttrStyleRules(t *testing.T) {
	js, _ := staticFiles.ReadFile("static/terminal.js")
	for _, want := range []string{
		"return MONO_ATTR[attr & 0x1F].replace('c', '');",
		"if (statusRow) return '';",
		"if (extra & 0x80) return 'n';",
		"ctx.fillRect(0, (rows - 1) * cellH, cols * cellW, 1);",
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
}

// TestSettingsRemembered: host, port, model, colour and codepage saved by
// an earlier visit are restored when the page loads again.
func TestSettingsRemembered(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	saved := `{"webibm3477-settings":"{\"host\":\"as400.example\",\"port\":\"992\",` +
		`\"model\":\"3477\",\"color\":\"amber\",\"muteClick\":true,\"clickStyle\":\"realistic\"}","webibm3477-codepage":"273"}`
	out, err := exec.Command("node", "testdata/domcheck.js", "static/terminal.js", saved).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{`"host":"as400.example"`, `"port":"992"`,
		`"model":"3477"`, `"color":"amber"`, `"codepage":"273"`, `"muteClick":true`, `"clickStyle":"realistic"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("restored values lack %s: %s", want, out)
		}
	}
	html, _ := staticFiles.ReadFile("static/index.html")
	for _, opt := range []string{`value="green"`, `value="amber"`, `value="white"`} {
		if !strings.Contains(string(html), opt) {
			t.Errorf("colour setting lacks option %s", opt)
		}
	}
}

// TestBadMessageKeepsSession: a message that does not decode is skipped;
// the WebSocket (and a host session on it) stays up.
func TestBadMessageKeepsSession(t *testing.T) {
	web := New(Config{Model: "3477", RomDir: "../.."})
	srv := httptest.NewServer(web.Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	for _, bad := range []string{`{"type":"key","down":"yes"}`, `not json`, `{"type":"cursor","row":"x"}`} {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(bad)); err != nil {
			t.Fatal(err)
		}
	}
	// still answering: "connect" without a terminal reports a status
	if err := ws.WriteJSON(clientMsg{Type: "connect", Host: "localhost", Port: "1", Size: "24x80"}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("session dropped after bad messages: %v", err)
		}
		if m.Type == "status" && strings.Contains(m.Text, "Switch the terminal on") {
			return
		}
	}
}

// TestPasteNeedsNoClipboardPermission: paste never reads the clipboard
// through navigator.clipboard (Chrome/Brave would ask "Share
// clipboard?"); it takes the browser's own paste into a hidden text box.
func TestPasteNeedsNoClipboardPermission(t *testing.T) {
	js, _ := staticFiles.ReadFile("static/terminal.js")
	if strings.Contains(string(js), "readText") {
		t.Error("terminal.js reads the clipboard through the permission-prompting API")
	}
	for _, want := range []string{"pasteSink.addEventListener('paste'", "if (down) armPaste();"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
}

// TestVersionLabel: the page shows the build version at the top right of
// the screen, and toolbar buttons give the focus back to the screen.
func TestVersionLabel(t *testing.T) {
	html, _ := staticFiles.ReadFile("static/index.html")
	js, _ := staticFiles.ReadFile("static/terminal.js")
	if !strings.Contains(string(html), `id="version-label"`) {
		t.Error("index.html lacks the version label")
	}
	for _, want := range []string{"'webibm3477 v' + defaults.version",
		"b.addEventListener('click', () => canvas.focus());",
		"send({ type: 'cursor', row: sel.a.r, col: sel.a.c });",
		"const CLICK_SLOP_PX = 5;"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
	rec := httptest.NewRecorder()
	New(Config{Model: "3477", Version: "0.7.0"}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `"version":"0.7.0"`) {
		t.Error("page does not carry the version")
	}
}

// TestSoundOverWebSocket: the power-on beep (POST switches the tone on
// and off) reaches the browser as "sound" messages, and the page plays
// them (Settings > Display > Sound).
func TestSoundOverWebSocket(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	web := New(Config{Model: "3477", RomDir: "../.."})
	srv := httptest.NewServer(web.Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(clientMsg{Type: "power", Model: "3477"}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var sawOn, sawOff bool
	for !(sawOn && sawOff) {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("tone on=%v off=%v: %v", sawOn, sawOff, err)
		}
		if m.Type == "sound" {
			if m.Tone {
				sawOn = true
			} else if sawOn {
				sawOff = true
			}
		}
	}
	js, _ := staticFiles.ReadFile("static/terminal.js")
	html, _ := staticFiles.ReadFile("static/index.html")
	for _, want := range []string{"} else if (m.type === 'sound') {", "function playSound(m)",
		"const CLICK_HZ = 3000;"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
	if !strings.Contains(string(html), `id="sound-select"`) {
		t.Error("Settings lack the sound switch")
	}
}

// TestRefuses3270Host: a host that asks for TN3270E (a mainframe/SNA
// host) is hung up on, with a status line of at most 79 columns.
func TestRefuses3270Host(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hungUp := make(chan bool, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte{255, 253, 40}) // IAC DO TN3270E
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				hungUp <- !strings.Contains(err.Error(), "timeout")
				return
			}
		}
	}()
	srv := httptest.NewServer(New(Config{Model: "3477", RomDir: "../.."}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ws.WriteJSON(clientMsg{Type: "power", Model: "3477"})
	ws.WriteJSON(clientMsg{Type: "connect", Host: host, Port: port, Size: "24x80"})
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("no refusal status: %v", err)
		}
		if m.Type == "status" && strings.HasPrefix(m.Text, "Refused:") {
			if len(m.Text) > 79 {
				t.Errorf("status wider than 79 columns: %q", m.Text)
			}
			break
		}
	}
	if !<-hungUp {
		t.Error("the 3270 host was not hung up on")
	}
}

// TestRecordedClicksServed: the four recorded IBM 3477 key clicks are
// served as WAV files, and Settings offers them.
func TestRecordedClicksServed(t *testing.T) {
	h := New(Config{Model: "3477"}).Handler()
	for i := 1; i <= 4; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/static/sounds/key-click-%d.wav", i), nil))
		b := rec.Body.Bytes()
		if rec.Code != 200 || len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
			t.Errorf("key-click-%d.wav: status %d, %d bytes", i, rec.Code, len(b))
		}
	}
	html, _ := staticFiles.ReadFile("static/index.html")
	if !strings.Contains(string(html), `<option value="realistic">Realistic (recorded IBM 3477)</option>`) {
		t.Error("Settings lack the realistic key click")
	}
}

// TestLockedServerIgnoresBrowserHost: with Lock the server connects only
// to its configured host:port, whatever host and port the browser sends,
// and tells the page that the host is fixed.
func TestLockedServerIgnoresBrowserHost(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	lhost, lport, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(lport)
	s := New(Config{Model: "3477", RomDir: "../..", Host: lhost, Port: port, Lock: true})

	rec := httptest.NewRecorder()
	s.Page(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `"locked":true`) {
		t.Error("page does not say the host is locked")
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(clientMsg{Type: "power", Model: "3477"})
	// an address the visitor must not reach through the server
	ws.WriteJSON(clientMsg{Type: "connect", Host: "10.255.255.1", Port: "6379", Size: "24x80"})
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for the connection: %v", err)
		}
		if m.Type != "status" {
			continue
		}
		if strings.Contains(m.Text, "10.255.255.1") {
			t.Fatalf("server used the browser's host: %q", m.Text)
		}
		if strings.HasPrefix(m.Text, "Connected to "+ln.Addr().String()) {
			break
		}
	}
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the locked host never saw the connection")
	}
}

// TestPrintKeyMappable: the Print key is near the top of the key mapping
// list in Settings.
func TestPrintKeyMappable(t *testing.T) {
	js, _ := staticFiles.ReadFile("static/terminal.js")
	if !strings.Contains(string(js), "  'Enter', 'Print', 'NewLine',") {
		t.Error("Print is not at the top of the mappable keys")
	}
}

// TestPrintDialogQueuesJobs: a job arriving while the print dialog is open
// waits its turn, empty job files are handled, blob URLs are released.
func TestPrintDialogQueuesJobs(t *testing.T) {
	js, _ := staticFiles.ReadFile("static/terminal.js")
	for _, want := range []string{"printQueue.push(m);", "if (printQueue.length > 0) showPrintDialog(printQueue.shift());",
		"if (!m.data) {", "URL.revokeObjectURL(url)", "URL.revokeObjectURL(a.href)"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("terminal.js lacks %q", want)
		}
	}
}
