package tn5250

import (
	"bytes"
	"errors"
	"testing"
)

// hostRecord frames a record as the host sends it (escaped, IAC EOR).
func hostRecord(rec []byte) []byte {
	var out []byte
	for _, b := range rec {
		out = append(out, b)
		if b == tnIAC {
			out = append(out, tnIAC)
		}
	}
	return append(out, tnIAC, tnEOR)
}

// startupRecord is a startup response with a 4-character code at data
// offset 5 (EBCDIC).
func startupRecord(code string) []byte {
	data := make([]byte, 16)
	for i, c := range code {
		switch {
		case c >= '0' && c <= '9':
			data[5+i] = 0xF0 + byte(c-'0')
		case c == 'I':
			data[5+i] = 0xC9
		}
	}
	return makeRecord(0, data)
}

// TestPrintSessionJob: negotiation announces a printer (IBM-3812-1,
// DEVNAME, IBMTRANSFORM 0, no KBDTYPE); after the startup response the
// job's SCS data goes to OnJob when the 17-byte end record arrives, and
// every record is acknowledged with Print Complete.
func TestPrintSessionJob(t *testing.T) {
	scs := []byte{0xC8, 0xC5, 0xD3, 0xD3, 0xD6, 0x40, 0xC1, 0x15, 0x0C} // not 7 bytes: 10+7 = 17 marks a job end
	var host []byte
	host = append(host, tnIAC, tnSB, optTerminalType, sbSEND, tnIAC, tnSE)
	host = append(host, tnIAC, tnSB, optNewEnviron, sbSEND, tnIAC, tnSE)
	host = append(host, hostRecord(startupRecord("I902"))...)
	host = append(host, hostRecord(makeRecord(opPrintComplete, scs))...)
	host = append(host, hostRecord(makeRecord(opPrintComplete, make([]byte, 7)))...) // 17 bytes: job end
	conn := newMockConn(host)
	p := NewPrintClient("PRT3477", "37")
	if !p.attach(conn) {
		t.Fatal("attach refused")
	}
	var started string
	var jobs [][]byte
	p.OnStarted = func(code, _ string) { started = code }
	p.OnJob = func(b []byte) { jobs = append(jobs, b) }
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	out := conn.written()
	for _, want := range []string{"IBM-3812-1", "DEVNAME", "PRT3477", "IBMTRANSFORM", "IBMFONT"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("negotiation lacks %q", want)
		}
	}
	if bytes.Contains(out, []byte("KBDTYPE")) {
		t.Error("a printer sends no keyboard type")
	}
	if started != "I902" || len(jobs) != 1 || !bytes.Equal(jobs[0], scs) {
		t.Fatalf("started %q jobs % X", started, jobs)
	}
	ack := []byte{0x00, 0x0A, 0x12, 0xA0, 0x00, flowClientOut, 4, 0, 0, opPrintComplete}
	if n := bytes.Count(out, ack); n != 2 {
		t.Fatalf("%d Print Complete acknowledgements, want 2", n)
	}
}

// TestPrintSessionRefused: a startup response with an error code ends the
// session with ErrPrinterRefused and hangs up.
func TestPrintSessionRefused(t *testing.T) {
	conn := newMockConn(hostRecord(startupRecord("8902")))
	p := NewPrintClient("PRT3477", "37")
	p.attach(conn)
	err := p.Run()
	if !errors.Is(err, ErrPrinterRefused) || !conn.closed {
		t.Fatalf("err %v closed %v", err, conn.closed)
	}
}

// TestPrintSessionHangUpBeforeConnect: a Disconnect before (or while) the
// session dials makes Connect fail and leaves nothing to block Run, so a
// browser session that hangs up never waits for an idle printer session.
func TestPrintSessionHangUpBeforeConnect(t *testing.T) {
	p := NewPrintClient("PRT3477", "37")
	p.Disconnect()
	if err := p.Connect("127.0.0.1", "1", false); !errors.Is(err, ErrHungUp) {
		t.Fatalf("Connect after Disconnect = %v, want ErrHungUp", err)
	}
	conn := newMockConn(nil)
	if p.attach(conn) || !conn.closed {
		t.Fatal("a connection arriving after Disconnect was kept")
	}
	if err := p.Run(); err == nil {
		t.Fatal("Run without a connection returned no error")
	}
}
