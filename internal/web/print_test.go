package web

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
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
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun })
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
