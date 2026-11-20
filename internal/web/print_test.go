package web

import (
	"bytes"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ibm3486/internal/printer"
)

// fakeCUPS replaces lpstat and lp; it records lp's arguments and input.
func fakeCUPS(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	oldLook, oldRun := lookPath, runCmd
	resetCache := func() {
		printerCache.Lock()
		printerCache.at = time.Time{}
		printerCache.Unlock()
	}
	resetCache()
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun; resetCache() })
	lookPath = func(string) (string, error) { return "/usr/bin/lpstat", nil }
	runCmd = func(name string, stdin []byte, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "lpstat" && args[0] == "-e":
			return []byte("HP_LaserJet\nBrother_HL\n"), nil
		case name == "lpstat" && args[0] == "-d":
			return []byte("system default destination: Brother_HL\n"), nil
		case name == "lp":
			if !bytes.HasPrefix(stdin, []byte("%PDF")) {
				return nil, errors.New("lp got no PDF")
			}
			return []byte("request id is HP_LaserJet-1"), nil
		}
		return nil, errors.New("unexpected command")
	}
	return &calls
}

func testJob() *printJob {
	return &printJob{id: 1, name: "ibm3477-print-test",
		doc: printer.Interpret([]byte("HELLO\r\n"))}
}

func TestListPrinters(t *testing.T) {
	fakeCUPS(t)
	names, def := listPrinters()
	if strings.Join(names, ",") != "HP_LaserJet,Brother_HL" || def != "Brother_HL" {
		t.Fatalf("printers %v default %q", names, def)
	}
}

// TestPrintJobOnChecksName: only a printer lpstat lists is passed to lp.
func TestPrintJobOnChecksName(t *testing.T) {
	calls := fakeCUPS(t)
	if err := printJobOn(testJob(), "HP_LaserJet; rm -rf /"); err == nil {
		t.Fatal("unknown printer name accepted")
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "lp ") {
			t.Fatalf("lp called for an unknown printer: %q", c)
		}
	}
	if err := printJobOn(testJob(), "HP_LaserJet"); err != nil {
		t.Fatal(err)
	}
	if last := (*calls)[len(*calls)-1]; last != "lp -d HP_LaserJet -t ibm3477-print-test" {
		t.Fatalf("lp called as %q", last)
	}
}

func TestJobStoreAndFiles(t *testing.T) {
	var s jobStore
	for i := 0; i < maxJobs+2; i++ {
		s.add(printer.Interpret([]byte("X\r\n")))
	}
	if s.get(1) != nil || s.get(maxJobs+2) == nil {
		t.Fatal("job store does not keep the latest jobs only")
	}
	data, name := jobFile(testJob(), "txt")
	if string(data) != "HELLO\n" || name != "ibm3477-print-test.txt" {
		t.Fatalf("text file %q %q", data, name)
	}
	if data, name = jobFile(testJob(), "pdf"); !bytes.HasPrefix(data, []byte("%PDF")) || !strings.HasSuffix(name, ".pdf") {
		t.Fatalf("pdf file %q", name)
	}
}

// TestPrintersNotOfferedWhenLocked: a server without LocalPrinters (a
// locked public server) offers no printers of its own.
func TestPrintersNotOfferedWhenLocked(t *testing.T) {
	fakeCUPS(t)
	srv := httptest.NewServer(New(Config{Model: "3477"}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(clientMsg{Type: "printers"})
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "printers" {
			if m.Local || len(m.Printers) != 0 {
				t.Fatalf("printers offered: %+v", m)
			}
			return
		}
	}
}

func TestPrinterDeviceName(t *testing.T) {
	for in, want := range map[string]string{"prt3477": "PRT3477", " Q$PRT_1 ": "Q$PRT_1"} {
		if got, ok := printerDevice(in); !ok || got != want {
			t.Errorf("printerDevice(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "1PRT", "TOOLONGNAME1", "PRT 1", "PRT;RM"} {
		if _, ok := printerDevice(bad); ok {
			t.Errorf("printerDevice(%q) accepted", bad)
		}
	}
}

// TestLockedServerOpensNoPrinterSession: on a -lock server a printer
// device from the browser is refused (a visitor must not receive the
// host's spooled output); the display session still connects.
func TestLockedServerOpensNoPrinterSession(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			defer c.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	srv := httptest.NewServer(New(Config{Model: "3477", RomDir: "../..", Host: host, Port: p, Lock: true}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(clientMsg{Type: "power", Model: "3477"})
	ws.WriteJSON(clientMsg{Type: "connect", Size: "24x80", PrinterDevice: "PRT01"})
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var m serverMsg
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "status" && strings.Contains(m.Text, "not offered") {
			break
		}
	}
	time.Sleep(500 * time.Millisecond)
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d connections to the host, want 1 (the display only)", n)
	}
}
