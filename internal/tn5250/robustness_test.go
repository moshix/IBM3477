// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"testing"
	"time"
)

// Regression tests for the session-robustness and data-stream fixes: host
// writes that block, unbounded reassembly buffers, cancellable Connect,
// inbound field data for odd fields, Field Exit/+/-, EA/TD addressing, WEC
// followed by a read, and the code-page tables.

// ── helpers ─────────────────────────────────────────────────────────────

// packets splits what the client wrote into its 5250 packets (IAC EOR
// framed, IAC IAC un-doubled) and returns each one's full GDS record.
func packets(raw []byte) [][]byte {
	var out [][]byte
	var cur []byte
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b == tnIAC && i+1 < len(raw) {
			switch raw[i+1] {
			case tnIAC:
				cur = append(cur, tnIAC)
				i++
				continue
			case tnEOR:
				out = append(out, cur)
				cur = nil
				i++
				continue
			}
		}
		cur = append(cur, b)
	}
	return out
}

// lastPacketData returns the 5250 data (after the 10-byte GDS header) of the
// last packet the client sent.
func lastPacketData(t *testing.T, conn *mockConn) []byte {
	t.Helper()
	ps := packets(conn.written())
	if len(ps) == 0 {
		t.Fatal("client sent no packet")
	}
	p := ps[len(ps)-1]
	if len(p) < 10 {
		t.Fatalf("short packet % X", p)
	}
	return p[10:]
}

// frame wraps a record for the wire: IAC doubled, then IAC EOR.
func frame(rec []byte) []byte {
	var out []byte
	for _, b := range rec {
		out = append(out, b)
		if b == tnIAC {
			out = append(out, tnIAC)
		}
	}
	return append(out, tnIAC, tnEOR)
}

// within runs fn and fails the test if it does not return within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// fieldClient sets up one input field at row 2 (0-based), col 5 with the
// given FFW and length, then issues the read command readCmd.
func fieldClient(t *testing.T, ffw1, ffw2 byte, length int, readCmd byte) (*Client, *mockConn, *field) {
	t.Helper()
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, ffw1, ffw2, Attr5250Green, byte(length >> 8), byte(length),
		ESC, readCmd, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))
	f := c.disp.dbuf().fieldList // the only field (fieldAt misses a 0-length one)
	if f == nil || f.startRow != 2 || f.startCol != 5 {
		t.Fatal("field not created")
	}
	c.disp.setCursor(2, 5)
	return c, conn, f
}

// ── 1. a host that stops reading must not freeze the session ───────────

// hostWontRead attaches a client to one end of a net.Pipe and has the "host"
// send a Read Screen Immediate (whose ~2 KB reply the host then never
// reads), returning once the client is blocked writing that reply.
func hostWontRead(t *testing.T) (*Client, chan error) {
	t.Helper()
	cliEnd, hostEnd := net.Pipe()
	t.Cleanup(func() { hostEnd.Close() })
	c := NewClient("3179-2", "37")
	c.attach(cliEnd)
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run() }()
	rec := makeRecord(opPutGet, []byte{ESC, CmdReadScreenImmediate})
	if _, err := hostEnd.Write(frame(rec)); err != nil {
		t.Fatalf("host write: %v", err)
	}
	// Wait until the reply write has taken the client lock.
	deadline := time.Now().Add(2 * time.Second)
	for c.mu.TryLock() {
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("client never started its reply")
		}
		time.Sleep(time.Millisecond)
	}
	return c, runErr
}

func TestDisconnectWhileHostNotReading(t *testing.T) {
	c, runErr := hostWontRead(t)
	within(t, 2*time.Second, "Disconnect", c.Disconnect)
	within(t, 2*time.Second, "Screen", func() { _ = c.Screen() })
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run after a user hang-up = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Disconnect")
	}
}

func TestWriteDeadlineEndsSession(t *testing.T) {
	old := writeTimeout
	writeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { writeTimeout = old })
	c, runErr := hostWontRead(t)
	select {
	case err := <-runErr:
		if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Run = %v, want a write-deadline error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked write was not timed out")
	}
	within(t, time.Second, "Screen", func() { _ = c.Screen() })
}

// ── 2. reassembly caps ──────────────────────────────────────────────────

func TestRecordWithoutEORIsCapped(t *testing.T) {
	big := bytes.Repeat([]byte{0x40}, maxRecordLen+10)
	st := newStream(newMockConn(big), "IBM-3179-2", "37", "", "")
	if _, err := st.readRecord(); !errors.Is(err, ErrRecordTooLong) {
		t.Fatalf("readRecord = %v, want ErrRecordTooLong", err)
	}

	// Run ends the session with that error.
	c := NewClient("3179-2", "37")
	conn := newMockConn(big)
	c.attach(conn)
	if err := c.Run(); !errors.Is(err, ErrRecordTooLong) {
		t.Fatalf("Run = %v, want ErrRecordTooLong", err)
	}
	if !conn.closed {
		t.Fatal("connection left open after the oversized record")
	}

	// A record right at the cap is still accepted.
	okRec := append(bytes.Repeat([]byte{0x40}, maxRecordLen), tnIAC, tnEOR)
	st = newStream(newMockConn(okRec), "IBM-3179-2", "37", "", "")
	if rec, err := st.readRecord(); err != nil || len(rec) != maxRecordLen {
		t.Fatalf("readRecord(cap) = %d bytes, %v", len(rec), err)
	}
}

func TestSubnegotiationWithoutSEIsCapped(t *testing.T) {
	data := append([]byte{tnIAC, tnSB, optNewEnviron}, bytes.Repeat([]byte{'A'}, maxSubnegLen+10)...)
	st := newStream(newMockConn(data), "IBM-3179-2", "37", "", "")
	if _, err := st.readRecord(); !errors.Is(err, ErrSubnegTooLong) {
		t.Fatalf("readRecord = %v, want ErrSubnegTooLong", err)
	}
	// Escaped IACs count too.
	data = []byte{tnIAC, tnSB, optNewEnviron}
	for i := 0; i < maxSubnegLen+10; i++ {
		data = append(data, tnIAC, tnIAC)
	}
	st = newStream(newMockConn(data), "IBM-3179-2", "37", "", "")
	if _, err := st.readRecord(); !errors.Is(err, ErrSubnegTooLong) {
		t.Fatalf("readRecord (IAC IAC) = %v, want ErrSubnegTooLong", err)
	}
}

// ── 11. Disconnect aborts a Connect in progress ─────────────────────────

func tcpListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestDisconnectAbortsTLSHandshake(t *testing.T) {
	// The listener's backlog completes the TCP handshake, but nobody ever
	// answers the TLS ClientHello: Connect hangs in the handshake.
	ln := tcpListener(t)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c := NewClient("3179-2", "37")
	errc := make(chan error, 1)
	go func() { errc <- c.Connect(host, port, true) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	c.Disconnect()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrHungUp) {
			t.Fatalf("Connect = %v, want ErrHungUp", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Connect took %v after Disconnect", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Connect still running after Disconnect")
	}
	if c.Screen().Connected {
		t.Fatal("session attached after Disconnect")
	}
}

func TestDisconnectBeforeConnect(t *testing.T) {
	ln := tcpListener(t)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c := NewClient("3179-2", "37")
	c.Disconnect()
	if err := c.Connect(host, port, false); !errors.Is(err, ErrHungUp) {
		t.Fatalf("Connect after Disconnect = %v, want ErrHungUp", err)
	}
	// A dial that completes after the hang-up is not attached either.
	if c.attach(newMockConn(nil)) {
		t.Fatal("attach succeeded after Disconnect")
	}
	if c.Screen().Connected {
		t.Fatal("session attached after Disconnect")
	}
}

func TestConnectTLSStillWorks(t *testing.T) {
	// Sanity check of the DialContext-based TLS path against a real server.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if s, err := ln.Accept(); err == nil {
			_ = s.(*tls.Conn).Handshake()
			s.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	c := NewClient("3179-2", "37")
	if err := c.Connect(host, port, true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	c.Disconnect()
}

// ── 6/7/8/12/13. inbound field data ─────────────────────────────────────

func TestZeroLengthFieldReadMDT(t *testing.T) {
	// SF 48 00 20 00 00: an MDT-on input field of length 0.
	c, conn, _ := fieldClient(t, 0x48, 0x00, 0, CmdReadMDTFields)
	c.Key("Enter")
	if c.fault != nil {
		t.Fatalf("Enter faulted: %v", c.fault)
	}
	got := lastPacketData(t, conn)
	want := []byte{3, 6, AIDEnter, OrderSBA, 3, 6} // SBA and no data
	if !bytes.Equal(got, want) {
		t.Fatalf("inbound = % X, want % X", got, want)
	}
}

func TestOnePositionSignedFieldReadImmediate(t *testing.T) {
	// SF 47 00 20 00 01: a 1-position signed numeric field (sign only).
	c, conn, f := fieldClient(t, 0x47, 0x00, 1, CmdReadMDTFields)
	c.disp.fieldData(f)[0] = c.disp.cmap.remote('-')
	f.setMDT(c.disp.dbuf())
	if _, err := c.processRecordSafe(makeRecord(opPutGet, []byte{ESC, CmdReadImmediate})); err != nil {
		t.Fatalf("Read Immediate: %v", err)
	}
	got := lastPacketData(t, conn)
	if want := []byte{3, 6, 0}; !bytes.Equal(got, want) {
		t.Fatalf("inbound = % X, want % X (no digit positions)", got, want)
	}
}

func TestContinuedFieldFirstAndLast(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := c.disp.cmap
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		// one SF marked both first (86 01) and last (86 02)
		OrderSBA, 3, 5, OrderSF, 0x40, 0x00, 0x86, 0x01, 0x86, 0x02, Attr5250Green, 0x00, 0x05,
		// a later stray middle subfield
		OrderSBA, 5, 5, OrderSF, 0x40, 0x00, 0x86, 0x03, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadInputFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))
	c.disp.setCursor(2, 5)
	c.TypeChar('A')
	c.Key("Enter")
	if c.fault != nil {
		t.Fatalf("Enter faulted: %v", c.fault)
	}
	got := lastPacketData(t, conn)
	want := []byte{2 + 1, 6 + 1, AIDEnter, m.remote('A'), 0x40, 0x40, 0x40, 0x40}
	if !bytes.Equal(got, want) {
		t.Fatalf("inbound = % X, want % X", got, want)
	}
}

func TestSignedNumericReadInputFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		sign byte
		want []byte
	}{
		{"positive", 0x00, []byte{0xF1, 0xF2, 0xF3}},
		{"blank sign", 0x40, []byte{0xF1, 0xF2, 0xF3}},
		{"negative", 0x60, []byte{0xF1, 0xF2, 0xD3}}, // EBCDIC '-' = 0x60
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, f := fieldClient(t, 0x47, 0x00, 4, CmdReadInputFields)
			d := c.disp.fieldData(f)
			copy(d, []byte{0xF1, 0xF2, 0xF3, tc.sign})
			f.setMDT(c.disp.dbuf())
			c.Key("Enter")
			got := lastPacketData(t, conn)
			want := append([]byte{3, 6, AIDEnter}, tc.want...)
			if !bytes.Equal(got, want) {
				t.Fatalf("inbound = % X, want % X", got, want)
			}
		})
	}
}

func TestTwoPositionSignedReadMDTNegative(t *testing.T) {
	c, conn, f := fieldClient(t, 0x47, 0x00, 2, CmdReadMDTFields)
	copy(c.disp.fieldData(f), []byte{0xF5, 0x60}) // "5-"
	f.setMDT(c.disp.dbuf())
	c.Key("Enter")
	got := lastPacketData(t, conn)
	if want := []byte{3, 6, AIDEnter, OrderSBA, 3, 6, 0xD5}; !bytes.Equal(got, want) {
		t.Fatalf("inbound = % X, want % X", got, want)
	}
}

// ── 14/15. Field Exit, Field+, Field- ───────────────────────────────────

func TestFieldExitNullsToEnd(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40)
	f := c.disp.fieldAt(2, 5)
	m := c.disp.cmap
	for i, r := range "ABCDE" {
		c.disp.fieldData(f)[i] = m.remote(byte(r))
	}
	c.TypeChar('X')
	c.Key("FieldExit")
	want := []byte{m.remote('X'), 0, 0, 0, 0}
	if got := c.disp.fieldData(f); !bytes.Equal(got, want) {
		t.Fatalf("field = % X, want % X", got, want)
	}
	if !f.mdt() {
		t.Fatal("Field Exit did not set MDT")
	}
}

func TestFieldExitRightAdjustZeroFill(t *testing.T) {
	// numeric-only, right-adjust zero fill (FFW 43 05)
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	c.sess.processRecord(makeRecord(opPutGet, []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x43, 0x05, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadMDTFields, 0x00, 0x00,
	}))
	f := c.disp.fieldAt(2, 5)
	for i, b := range []byte{0xF9, 0xF9, 0xF9, 0xF9, 0xF9} {
		c.disp.fieldData(f)[i] = b // old contents, overtyped and exited
	}
	c.disp.setCursor(2, 5)
	c.TypeChar('4')
	c.TypeChar('2')
	c.Key("FieldExit")
	if got, want := c.disp.fieldData(f), []byte{0xF0, 0xF0, 0xF0, 0xF4, 0xF2}; !bytes.Equal(got, want) {
		t.Fatalf("field = % X, want % X", got, want)
	}
}

func TestFieldMinusNumericOnly(t *testing.T) {
	c, conn := newOperatorClient(t, 0x43) // numeric only, no adjust
	f := c.disp.fieldAt(2, 5)
	c.TypeChar('4')
	c.TypeChar('2')
	c.Key("FieldMinus")
	if got, want := c.disp.fieldData(f), []byte{0xF4, 0xD2, 0, 0, 0}; !bytes.Equal(got, want) {
		t.Fatalf("after Field-: % X, want % X", got, want)
	}
	// Field+ (cursor after the digits) makes it positive again.
	c.disp.setCursor(2, 7)
	c.Key("FieldPlus")
	if got, want := c.disp.fieldData(f), []byte{0xF4, 0xF2, 0, 0, 0}; !bytes.Equal(got, want) {
		t.Fatalf("after Field+: % X, want % X", got, want)
	}
	// Field- again, then send: F4 D2.
	c.disp.setCursor(2, 7)
	c.Key("FieldMinus")
	c.Key("Enter")
	got := lastPacketData(t, conn)
	if want := []byte{OrderSBA, 3, 6, 0xF4, 0xD2}; len(got) < 3 || !bytes.Equal(got[3:], want) {
		t.Fatalf("inbound = % X, want cursor, AID, % X", got, want)
	}
}

func TestFieldMinusPlusSignedNumeric(t *testing.T) {
	c, _ := newOperatorClient(t, 0x47) // signed numeric, 5 positions
	f := c.disp.fieldAt(2, 5)
	c.TypeChar('1')
	c.TypeChar('2')
	c.Key("FieldMinus")
	// right-adjusted blank-filled into the digit positions, '-' in the sign
	if got, want := c.disp.fieldData(f), []byte{0x40, 0x40, 0xF1, 0xF2, 0x60}; !bytes.Equal(got, want) {
		t.Fatalf("after Field-: % X, want % X", got, want)
	}
	c.disp.setCursor(2, 9) // the sign position
	c.Key("FieldPlus")
	if got, want := c.disp.fieldData(f), []byte{0x40, 0x40, 0xF1, 0xF2, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("after Field+: % X, want % X", got, want)
	}
}

func TestFieldMinusInAlphaFieldIsError(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40)
	c.TypeChar('A')
	c.Key("FieldMinus")
	st := c.Screen()
	if !st.Inhibited || st.ErrorCode != KBDSrcFldMDisallowed {
		t.Fatalf("inhibited=%v code=%04X, want Field- disallowed (%04X)",
			st.Inhibited, st.ErrorCode, KBDSrcFldMDisallowed)
	}
}

// ── 16/17. EA and TD addressing ─────────────────────────────────────────

func TestEraseToAddressCursor(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := c.disp.cmap
	for i := range c.disp.dbuf().data {
		c.disp.dbuf().data[i] = m.remote('.')
	}
	c.sess.processRecord(makeRecord(opPutGet, []byte{
		ESC, CmdWriteToDisplay, 0x00, 0x00,
		OrderSBA, 5, 1, OrderEA, 5, 10, 0x02, 0xFF, m.remote('Z'),
	}))
	for col := 0; col <= 9; col++ {
		if b := c.disp.charAt(4, col); b != 0x40 {
			t.Fatalf("(4,%d) = %02X, want erased", col, b)
		}
	}
	if b := c.disp.charAt(4, 10); b != m.remote('Z') {
		t.Fatalf("Z not at (4,10) - the address after EA's end (4,9); got %02X", b)
	}
	if b := c.disp.charAt(5, 10); b != m.remote('.') {
		t.Fatalf("(5,10) = %02X: Z landed a row too low", b)
	}
}

func TestTransparentDataPastEndRejected(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	td := []byte{ESC, CmdWriteToDisplay, 0x00, 0x00, OrderSBA, 24, 70, OrderTD, 0x00, 50}
	td = append(td, bytes.Repeat([]byte{0xC1}, 50)...)
	c.sess.processRecord(makeRecord(opPutGet, td))
	for col := 0; col < 80; col++ {
		if b := c.disp.charAt(0, col); b == 0xC1 {
			t.Fatalf("TD wrapped to row 0 at col %d", col)
		}
	}
	ps := packets(conn.written())
	if len(ps) == 0 || ps[0][7] != recHERR {
		t.Fatalf("no negative response sent: % X", conn.written())
	}
	if got := ps[0][10:]; !bytes.Equal(got, []byte{0x10, 0x05, 0x01, 0x22}) {
		t.Fatalf("negative response = % X, want 10 05 01 22", got)
	}
	// TD that exactly fits (11 bytes at row 24 col 70) is accepted.
	conn2 := newMockConn(nil)
	c2 := NewClient("3179-2", "37")
	c2.attach(conn2)
	td = []byte{ESC, CmdWriteToDisplay, 0x00, 0x00, OrderSBA, 24, 70, OrderTD, 0x00, 11}
	td = append(td, bytes.Repeat([]byte{0xC1}, 11)...)
	c2.sess.processRecord(makeRecord(opPutGet, td))
	if len(conn2.written()) != 0 {
		t.Fatalf("fitting TD rejected: % X", conn2.written())
	}
	if b := c2.disp.charAt(23, 79); b != 0xC1 {
		t.Fatalf("(23,79) = %02X, want C1", b)
	}
}

// ── 18. Write Error Code then a read keeps the error ────────────────────

func TestWriteErrorCodeSurvivesRead(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := c.disp.cmap
	row := c.disp.dbuf().msgLine()
	// something on the message line that the error overwrites
	c.sess.processRecord(makeRecord(opPutGet, []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, byte(row + 1), 1, m.remote('O'), m.remote('K'),
		OrderSBA, 3, 5, OrderSF, 0x40, 0x00, Attr5250Green, 0x00, 0x05,
	}))
	c.sess.processRecord(makeRecord(opPutGet, []byte{
		ESC, CmdWriteErrorCode, m.remote('E'), m.remote('R'), m.remote('R'),
		ESC, CmdReadMDTFields, 0x20, 0x00,
	}))
	st := c.Screen()
	if !st.Inhibited || c.disp.errorText != "ERR" {
		t.Fatalf("after WEC+read: inhibited=%v error=%q, want inhibited ERR", st.Inhibited, c.disp.errorText)
	}
	if b := c.disp.charAt(row, 0); b != m.remote('E') {
		t.Fatalf("message line lost the error: %02X", b)
	}
	if c.sess.readOpcode != CmdReadMDTFields {
		t.Fatal("read not pending")
	}
	c.TypeChar('A') // refused while inhibited
	if f := c.disp.fieldAt(2, 5); c.disp.fieldData(f)[0] != 0 {
		t.Fatal("typing accepted while the error is shown")
	}
	c.Key("Reset")
	st = c.Screen()
	if st.Inhibited || st.HostLocked {
		t.Fatalf("after Reset: inhibited=%v hostLocked=%v", st.Inhibited, st.HostLocked)
	}
	if b := c.disp.charAt(row, 0); b != m.remote('O') {
		t.Fatalf("Reset did not restore the message line: %02X", b)
	}
}

func TestReadWithoutErrorUnlocks(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	c.sess.processRecord(makeRecord(opPutGet, []byte{
		ESC, CmdWriteToDisplay, 0x20, 0x00, // CC1 locks
		ESC, CmdReadMDTFields, 0x20, 0x00,
	}))
	if st := c.Screen(); st.Inhibited || st.HostLocked {
		t.Fatalf("read left keyboard locked: inhibited=%v hostLocked=%v", st.Inhibited, st.HostLocked)
	}
}

// ── 19. code pages ──────────────────────────────────────────────────────

func TestCodePagesRoundTrip(t *testing.T) {
	for _, name := range []string{"37", "500", "37-bracket", "273", "277", "278",
		"280", "284", "285", "297", "871", "870"} {
		m := newCharMap(name)
		for i := 0; i < 256; i++ {
			b := byte(i)
			if got := m.toLocal[m.toRemote[b]]; got != b {
				t.Errorf("CP%s: local %02X -> remote %02X -> local %02X", name, b, m.toRemote[b], got)
			}
			if got := m.toRemote[m.toLocal[b]]; got != b {
				t.Errorf("CP%s: remote %02X -> local %02X -> remote %02X", name, b, m.toLocal[b], got)
			}
			// Through Unicode (the browser / twinax path); local() shows
			// 0x00 and 0x1C specially, so they are excluded.
			if b != 0x00 && b != 0x1C {
				if got := m.toRemoteRune(m.toLocalRune(b)); got != b {
					t.Errorf("CP%s: EBCDIC %02X -> %q -> %02X", name, b, m.toLocalRune(b), got)
				}
			}
		}
	}
	// the Latin-2 bridge itself
	m := newCharMap("870")
	for i := 0; i < 256; i++ {
		r := m.localToUnicode[i]
		if lb, ok := m.unicodeToLocal[r]; !ok || int(lb) != i {
			t.Errorf("Latin-2 bridge: local %02X -> U+%04X -> %02X", i, r, lb)
		}
	}
}

func TestBracketMapIsSwap(t *testing.T) {
	m := newCharMap("37-bracket")
	for _, tc := range []struct {
		ebc   byte
		local rune
	}{{0x4A, '['}, {0x5A, ']'}, {0xBA, '¢'}, {0xBB, '!'}} {
		if got := m.toLocalRune(tc.ebc); got != tc.local {
			t.Errorf("0x%02X -> %q, want %q", tc.ebc, got, tc.local)
		}
		if got := m.toRemoteRune(tc.local); got != tc.ebc {
			t.Errorf("%q -> 0x%02X, want 0x%02X", tc.local, got, tc.ebc)
		}
	}
}

func TestCP870FixedDuplicates(t *testing.T) {
	m := newCharMap("870")
	seen := map[rune]byte{}
	for i := 0; i < 256; i++ {
		r := m.toLocalRune(byte(i))
		if i == 0x00 || i == 0x1C {
			continue // local() display overrides: blank, '*'
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("EBCDIC %02X and %02X both decode to %q", prev, i, r)
		}
		seen[r] = byte(i)
	}
	for _, r := range "ĘęźŹŽž" {
		if m.toRemoteRune(r) == m.toRemoteRune(' ') {
			t.Errorf("%q has no CP870 code point", r)
		}
	}
	for ebc, want := range map[byte]rune{0x72: 'Ę', 0x52: 'ę', 0xAF: 'Ş', 0xB7: 'ź', 0xB8: 'Ž', 0x66: 'Ă'} {
		if got := m.toLocalRune(ebc); got != want {
			t.Errorf("CP870 0x%02X -> %q, want %q", ebc, got, want)
		}
	}
}
