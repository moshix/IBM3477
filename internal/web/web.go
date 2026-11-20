// Package web serves an emulated IBM InfoWindow twinax terminal in the
// browser: the page, its static files, and one WebSocket per terminal.
// It is shared by the public server (cmd/webibm3477) and the desktop
// launcher (cmd/appwebibm3477).
package web

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ibm3486/internal/board"
	"ibm3486/internal/emu"
	"ibm3486/internal/printer"
	"ibm3486/internal/tn5250"
)

//go:embed static
var staticFiles embed.FS

// Config holds the server defaults.
type Config struct {
	Model   string // default terminal model ("3477" or "3486")
	RomDir  string // ROM directory override; "" = built-in ROMs, then disk
	Host    string // default TN5250 host shown in the page
	Port    int    // default TN5250 port
	Version string // build version shown in the page
	HostTLS bool   // connect to the TN5250 host over TLS
	// Lock restricts connections to Host:Port: the host and port the
	// browser sends are ignored, so visitors of a public server cannot
	// make it open connections to other (e.g. internal) addresses.
	Lock bool
	// ExtraHead is HTML inserted before </head> (the desktop app adds its
	// tab-presence script here).
	ExtraHead string
	// LocalPrinters lets the page print jobs on this computer's printers
	// (CUPS). Off for a locked public server: visitors must not print
	// on the server's printers. Saving a job as a file always works.
	LocalPrinters bool
}

// Server serves the terminal page and its WebSocket.
type Server struct {
	cfg    Config
	static fs.FS
}

// New builds a server with the given defaults.
func New(cfg Config) *Server {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	return &Server{cfg: cfg, static: sub}
}

// Handler returns the routes: "/" (page), "/static/" (assets) and "/ws".
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.Page)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.static))))
	mux.HandleFunc("/ws", s.WS)
	return mux
}

type pageDefaults struct {
	Model   string `json:"model"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	Locked  bool   `json:"locked,omitempty"` // host and port fixed by the server
}

// Page returns index.html with the server defaults injected as a JS
// global before </head>.
func (s *Server) Page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// json.Marshal escapes <, > and & so the values cannot end the script
	defaults, _ := json.Marshal(pageDefaults{
		Model: s.cfg.Model, Host: s.cfg.Host, Port: s.cfg.Port, Version: s.cfg.Version,
		Locked: s.cfg.Lock,
	})
	inject := "<script>window.WEBTWINAX_DEFAULTS=" + string(defaults) + ";</script>" +
		s.cfg.ExtraHead + "</head>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, strings.Replace(string(page), "</head>", inject, 1))
}

// clientMsg is a message from the browser.
type clientMsg struct {
	Type  string `json:"type"` // "power", "poweroff", "key", "paste", "cursor", "connect", "disconnect", "printers", "jobfile", "jobprint", "jobdrop", "printkey"
	Model string `json:"model,omitempty"`
	Code  string `json:"code,omitempty"` // KeyboardEvent.code
	Down  bool   `json:"down,omitempty"`
	Host  string `json:"host,omitempty"`
	Port  string `json:"port,omitempty"`
	Size  string `json:"size,omitempty"` // "24x80" or "27x132"
	// Codepage is the host EBCDIC codepage ("37", "273", ...)
	Codepage string `json:"codepage,omitempty"`
	// Text is clipboard text to type into the terminal ("paste")
	Text string `json:"text,omitempty"`
	// Row, Col: screen cell clicked ("cursor"), 0-based
	Row int `json:"row,omitempty"`
	Col int `json:"col,omitempty"`
	// Job, Format, Printer: print job requests ("jobfile": Format "pdf"
	// or "txt"; "jobprint": on Printer; "jobdrop")
	Job     int    `json:"job,omitempty"`
	Format  string `json:"format,omitempty"`
	Printer string `json:"printer,omitempty"`
	// PrintKey is what the Print key does in Display-Printer mode:
	// "local" (a copy on the terminal's printer) or "host" ("power",
	// "printkey")
	PrintKey string `json:"printKey,omitempty"`
}

// serverMsg is a message to the browser.
type serverMsg struct {
	Type  string `json:"type"` // "screen", "font", "status", "traffic", "sound", "printjob", "printers", "jobfile"
	Rows  int    `json:"rows,omitempty"`
	Cols  int    `json:"cols,omitempty"`
	CellW int    `json:"cellw,omitempty"` // dots per character cell
	CellH int    `json:"cellh,omitempty"` // scan lines per character row
	// Cursor position from the terminal's CRTC (cursor shown when CurOn)
	CurRow int    `json:"currow"`
	CurCol int    `json:"curcol"`
	CurOn  bool   `json:"curon"`
	Cells  string `json:"cells,omitempty"` // base64, 3 bytes per cell
	// Lines is the displayed text per row, for copying a selection
	Lines []string `json:"lines,omitempty"`
	Font  string   `json:"font,omitempty"` // base64 font RAM
	LED   int      `json:"led"`
	// TX/RX are the bytes sent to / received from the host ("traffic")
	TX   uint64 `json:"tx"`
	RX   uint64 `json:"rx"`
	Text string `json:"text,omitempty"`
	// Tone/Volume/Clicks: the terminal's speaker ("sound"): tone on or
	// off, volume 0-31, key clicks so far
	Tone   bool   `json:"tone,omitempty"`
	Volume int    `json:"volume,omitempty"`
	Clicks uint32 `json:"clicks,omitempty"`
	// Print jobs: a finished job ("printjob": Job, Pages, Name), a job
	// file ("jobfile": Job, Format, Name, Data base64), this computer's
	// printers ("printers": Printers, DefaultPrinter; Local false when
	// printing here is not offered)
	Job            int      `json:"job,omitempty"`
	Pages          int      `json:"pages,omitempty"`
	Name           string   `json:"name,omitempty"`
	Format         string   `json:"format,omitempty"`
	Data           string   `json:"data,omitempty"`
	Printers       []string `json:"printers,omitempty"`
	DefaultPrinter string   `json:"defaultPrinter,omitempty"`
	Local          bool     `json:"local,omitempty"`
}

// terminalTypes maps a screen size to the TN5250 terminal type announced
// to the host; sizesByModel lists what each model supports (the 3486 is
// 24x80 only, 27x132 being a 3487/3488 feature).
var terminalTypes = map[string]string{"24x80": "IBM-3179-2", "27x132": "IBM-3477-FC"}

var sizesByModel = map[string][]string{
	"3477": {"24x80", "27x132"},
	"3486": {"24x80"},
}

// terminalType validates a model/size pair and returns the TN5250
// terminal type for it.
func terminalType(model, size string) (string, error) {
	for _, s := range sizesByModel[model] {
		if s == size {
			return terminalTypes[size], nil
		}
	}
	return "", fmt.Errorf("the IBM %s does not support screen size %s", model, size)
}

var upgrader = websocket.Upgrader{CheckOrigin: sameOrigin}

// sameOrigin accepts WebSocket upgrades only from pages this server
// served (Origin host == Host). Non-browser clients send no Origin.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	o = strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://")
	return strings.EqualFold(o, r.Host)
}

// maxClientMessage is the largest message accepted from the browser.
const maxClientMessage = 128 << 10

// writeWait is how long one message to the browser may take to write
// before the browser counts as gone (a variable for tests).
var writeWait = 10 * time.Second

// WS runs one emulated terminal for the life of a WebSocket connection.
func (s *Server) WS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	// browser messages are small (a paste is cut to emu.MaxPasteChars
	// characters by the page); a larger one ends the connection
	ws.SetReadLimit(maxClientMessage)

	// A single writer goroutine owns the socket. done closes when it
	// stops, so senders never block on a dead connection. A browser that
	// stops reading fails the write deadline: the socket is then closed,
	// which also ends the read loop and with it the session, so the
	// machine goroutine never waits in must() for more than writeWait.
	out := make(chan serverMsg, 8)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for m := range out {
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if ws.WriteJSON(m) != nil {
				ws.Close()
				return
			}
		}
	}()
	// must: deliver unless the socket is gone (fonts, status).
	must := func(m serverMsg) {
		select {
		case out <- m:
		case <-done:
		}
	}
	// lossy: screens may be dropped when the browser lags; a newer one
	// follows within 33 ms.
	lossy := func(m serverMsg) {
		select {
		case out <- m:
		default:
		}
	}

	var mach *emu.Machine
	curModel := s.cfg.Model
	var jobs jobStore // this session's print jobs (print.go)
	// printWG: lp runs in goroutines that report with must(); they are
	// waited for before out is closed
	var printWG sync.WaitGroup
	// The TN5250 host session runs in its own goroutine (sessWG); it is
	// disconnected and waited for before out is closed.
	var host *tn5250.Client
	var sessWG sync.WaitGroup
	hangUp := func() {
		if host != nil {
			host.Disconnect()
			host = nil
		}
		sessWG.Wait()
	}
	defer func() {
		hangUp()
		if mach != nil {
			mach.Stop() // waits for the machine goroutine: no more sends
		}
		printWG.Wait() // and for print commands still running
		close(out)
		wg.Wait()
	}()

	for {
		// A message that does not decode is skipped: only a broken
		// connection ends the session (and with it the host session).
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m clientMsg
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "power":
			hangUp()
			if mach != nil {
				mach.Stop()
				mach = nil
			}
			mdl := m.Model
			if mdl == "" {
				mdl = s.cfg.Model
			}
			roms, _, err := board.ROMSource(s.cfg.RomDir, mdl)
			if err != nil {
				must(serverMsg{Type: "status", Text: err.Error()})
				continue
			}
			nm, err := emu.New(mdl, roms)
			if err != nil {
				must(serverMsg{Type: "status", Text: err.Error()})
				continue
			}
			nm.SetCodepage(m.Codepage) // decodes the screen for copying
			nm.SetPrintKeyToHost(m.PrintKey == "host")
			nm.SetPrintHandler(func(doc *printer.Document) {
				j := jobs.add(doc)
				must(serverMsg{Type: "printjob", Job: j.id, Pages: len(doc.Pages), Name: j.name})
			})
			mach, curModel = nm, mdl
			must(serverMsg{Type: "status", Text: "IBM " + mdl + " powered on"})
			var lastSound board.Sound
			mach.Start(func(snap *emu.Snapshot) {
				if snap.Sound != lastSound {
					// never dropped: a lost "tone off" would not end
					lastSound = snap.Sound
					must(serverMsg{Type: "sound", Tone: snap.Sound.ToneOn,
						Volume: int(snap.Sound.Volume), Clicks: snap.Sound.Clicks})
				}
				if snap.Font != nil {
					must(serverMsg{Type: "font", Font: base64.StdEncoding.EncodeToString(snap.Font)})
				}
				lossy(serverMsg{Type: "screen", Rows: snap.Rows, Cols: snap.Cols,
					CellW: snap.CellWidth, CellH: snap.CellHeight,
					CurRow: snap.CursorRow, CurCol: snap.CursorCol, CurOn: snap.CursorOn,
					Lines: snap.Text,
					LED:   int(snap.LED), Cells: base64.StdEncoding.EncodeToString(snap.Cells)})
			})
		case "poweroff":
			hangUp()
			if mach != nil {
				mach.Stop()
				mach = nil
			}
			must(serverMsg{Type: "status", Text: "Powered off"})
		case "paste":
			if mach != nil {
				switch truncated, busy := mach.Paste(m.Text); {
				case busy:
					must(serverMsg{Type: "status", Text: "Paste ignored: the previous paste is still being typed"})
				case truncated:
					must(serverMsg{Type: "status", Text: fmt.Sprintf("Pasting only the first %d characters", emu.MaxPasteChars)})
				}
			}
		case "cursor":
			// a click on the screen moves the 5250 cursor there (the
			// controller then moves the terminal's cursor)
			if host != nil {
				host.MoveCursor(m.Row, m.Col)
			}
		case "printers":
			msg := serverMsg{Type: "printers", Local: s.cfg.LocalPrinters}
			if s.cfg.LocalPrinters {
				msg.Printers, msg.DefaultPrinter = listPrinters()
			}
			must(msg)
		case "jobfile":
			if j := jobs.get(m.Job); j != nil {
				data, name := jobFile(j, m.Format)
				must(serverMsg{Type: "jobfile", Job: j.id, Format: m.Format, Name: name, Data: encodeFile(data)})
			}
		case "jobprint":
			j := jobs.get(m.Job)
			switch {
			case j == nil:
			case !s.cfg.LocalPrinters:
				must(serverMsg{Type: "status", Text: "Printing on this computer is not offered"})
			default:
				// lp can take a moment: report from a goroutine
				printWG.Add(1)
				go func(j *printJob, prn string) {
					defer printWG.Done()
					if err := printJobOn(j, prn); err != nil {
						must(serverMsg{Type: "status", Text: fit79("Print failed: " + err.Error())})
						return
					}
					must(serverMsg{Type: "status", Text: fit79("Printed " + j.name + " on " + prn)})
				}(j, m.Printer)
			}
		case "jobdrop":
			jobs.drop(m.Job)
		case "printkey":
			if mach != nil {
				mach.SetPrintKeyToHost(m.PrintKey == "host")
			}
		case "key":
			if mach != nil {
				if codes := emu.ScanCodes(m.Code, m.Down); codes != nil {
					mach.Key(codes)
				}
			}
		case "connect":
			tt, err := terminalType(curModel, m.Size)
			if err != nil {
				must(serverMsg{Type: "status", Text: err.Error()})
				continue
			}
			if mach == nil {
				must(serverMsg{Type: "status", Text: "Switch the terminal on first"})
				continue
			}
			hangUp()
			mach.Attach(nil)
			if s.cfg.Lock {
				// only the server's own host: never what the browser sent
				m.Host, m.Port = s.cfg.Host, strconv.Itoa(s.cfg.Port)
			}
			target := net.JoinHostPort(m.Host, m.Port)
			must(serverMsg{Type: "status", Text: "Connecting to " + target + " as " + tt})
			cl := tn5250.NewClient(strings.TrimPrefix(tt, "IBM-"), m.Codepage) // "" = 37
			host = cl
			sessWG.Add(1)
			go s.hostSession(cl, mach, m.Host, m.Port, target, &sessWG, must)
		case "disconnect":
			hangUp()
			if mach != nil {
				mach.Attach(nil)
			}
			must(serverMsg{Type: "status", Text: "Disconnected"})
		}
	}
}

// hostSession connects a TN5250 session, attaches it to the terminal's
// twinax port through the emulated workstation controller, and runs it
// until the host or the user hangs up. Status texts stay within 79
// columns.
func (s *Server) hostSession(cl *tn5250.Client, mach *emu.Machine, host, port, target string,
	wg *sync.WaitGroup, status func(serverMsg)) {
	defer wg.Done()
	if err := cl.Connect(host, port, s.cfg.HostTLS); err != nil {
		if errors.Is(err, tn5250.ErrHungUp) {
			// the user hung up (disconnect, power, tab closed) while the
			// dial or TLS handshake ran: hangUp's Disconnect aborted it
			return
		}
		status(serverMsg{Type: "status", Text: "Cannot connect to " + target + ": " + shortErr(err)})
		return
	}
	mach.Attach(cl)
	status(serverMsg{Type: "status", Text: "Connected to " + target})
	// report the byte counts once a second while the session runs
	stopTraffic := make(chan struct{})
	var trafficWG sync.WaitGroup
	trafficWG.Add(1)
	go func() {
		defer trafficWG.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopTraffic:
				return
			case <-tick.C:
				tx, rx := cl.Traffic()
				status(serverMsg{Type: "traffic", TX: tx, RX: rx})
			}
		}
	}()
	err := cl.Run()
	close(stopTraffic)
	trafficWG.Wait()
	tx, rx := cl.Traffic()
	status(serverMsg{Type: "traffic", TX: tx, RX: rx})
	mach.Attach(nil)
	if errors.Is(err, tn5250.ErrNot5250Host) {
		// a mainframe/SNA host behind TN3270: refused and hung up on
		status(serverMsg{Type: "status", Text: fit79("Refused: " + target +
			" is a 3270 (SNA) host, not a 5250 host")})
		return
	}
	if err != nil {
		status(serverMsg{Type: "status", Text: "Host session ended: " + shortErr(err)})
		return
	}
	status(serverMsg{Type: "status", Text: "Disconnected from " + target})
}

// fit79 cuts a status text to 79 columns.
func fit79(t string) string {
	if r := []rune(t); len(r) > 79 {
		return string(r[:76]) + "..."
	}
	return t
}

// shortErr keeps error texts short enough for a 79-column status line.
func shortErr(err error) string {
	t := err.Error()
	if len(t) > 50 {
		t = t[:47] + "..."
	}
	return t
}
