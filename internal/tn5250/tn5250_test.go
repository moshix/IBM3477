// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

// mockConn is an in-memory io.ReadWriteCloser: the client reads negotiation /
// data from `in` and its writes are captured in `out`.
type mockConn struct {
	mu     sync.Mutex
	in     *bytes.Buffer
	out    *bytes.Buffer
	closed bool
}

func newMockConn(hostData []byte) *mockConn {
	return &mockConn{in: bytes.NewBuffer(hostData), out: &bytes.Buffer{}}
}

func (m *mockConn) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.in.Len() == 0 {
		return 0, io.EOF
	}
	return m.in.Read(p)
}

func (m *mockConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.Write(p)
}

func (m *mockConn) Close() error { m.closed = true; return nil }

func (m *mockConn) written() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.out.Bytes()...)
}

// makeRecord wraps 5250 command-stream bytes in the 10-byte GDS header the
// framing expects (data[6]=4 so the read cursor lands at offset 10, opcode at
// data[9]).
func makeRecord(opcode byte, data []byte) []byte {
	length := len(data) + 10
	hdr := []byte{
		byte(length >> 8), byte(length & 0xff),
		0x12, 0xa0,
		0x00, flowDisplay,
		4, recHNone, 0, opcode,
	}
	return append(hdr, data...)
}

// ── Malformed-record robustness (FIX 2 bounds checks) ───────────────────

// TestMalformedRecordsDoNotPanic feeds edge-case host data that would index the
// display buffer out of range in a naive port (Go turns tn5250's silent
// overruns into panics). Each record must be processed without panicking:
//   - a Write-Error-Code message longer than the screen (setMsgLine clamp),
//   - a WEC-embedded IC of 0,0 -> cursor -1,-1 then an addch (writeErrorCode clamp),
//   - an Erase-to-Address whose target row is past the bottom (eraseToAddress clamp),
//   - a Roll whose window runs past the last line (dbuffer.roll clamp).
func TestMalformedRecordsDoNotPanic(t *testing.T) {
	m := newCharMap("37")

	// overWidth: WEC with a message far longer than 80 columns.
	overWidth := []byte{ESC, CmdWriteErrorCode}
	for i := 0; i < 200; i++ {
		overWidth = append(overWidth, m.remote('X'))
	}

	// wecIC00: WEC embeds IC 0,0 (endY/endX -> -1), then a WTD writes a char at
	// that cursor — the addch is what panics pre-fix.
	wecIC00 := []byte{
		ESC, CmdWriteErrorCode, m.remote('E'), m.remote('R'), m.remote('R'),
		OrderIC, 0, 0,
		ESC, CmdWriteToDisplay, 0x00, 0x08, m.remote('Z'),
	}

	// eaPastBottom: cursor at 1,1 then EA to row 30 (past 24), length 2, attr 0xFF.
	eaPastBottom := []byte{
		ESC, CmdWriteToDisplay, 0x00, 0x08,
		OrderSBA, 1, 1,
		OrderEA, 30, 1, 2, 0xFF,
	}

	// rollPastLast: roll DOWN one line over window [0,200] — bot past the buffer.
	rollPastLast := []byte{ESC, CmdRoll, 0x81, 0, 200}

	cases := []struct {
		name string
		data []byte
	}{
		{"over-width WEC", overWidth},
		{"WEC IC 0,0", wecIC00},
		{"EA past bottom row", eaPastBottom},
		{"Roll past last line", rollPastLast},
	}

	for _, tc := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", tc.name, r)
				}
			}()
			c := NewClient("3179-2", "37")
			c.attach(newMockConn(nil))
			c.sess.processRecord(makeRecord(opPutGet, tc.data))
			// A snapshot must also render without indexing out of range.
			_ = c.buildSnapshot()
		}()
	}
}

// TestWriteDataStructuredField covers FIX 4: a WRITE-DATA WDSF (0x54) whose
// flag byte 1 bit 0x80 is set writes its payload text into the current word-wrap
// field (gold session.c:3575 -> tn5250_display_wordwrap). A word-wrap input
// field is created, then a WRITE-DATA WDSF delivers "HELLO"; the text must land
// in the field data.
func TestWriteDataStructuredField(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// WRITE-DATA payload: flagbyte1=0x80 then "HELLO".
	wd := []byte{0x80, m.remote('H'), m.remote('E'), m.remote('L'), m.remote('L'), m.remote('O')}
	wdsfLen := 4 + len(wd)

	// A word-wrap input field (FCW 0x8680) at row 2,col 5 (0-based), length 10,
	// followed by the WRITE-DATA WDSF (cursor is at the field start after the SF).
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5,
		OrderSF, 0x00, 0x00, 0x86, 0x80, Attr5250Green, 0x00, 0x0A,
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFWriteData,
	}
	data = append(data, wd...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	// Field data starts at row 2, col 5; "HELLO" must occupy the first 5 cells.
	want := []byte("HELLO")
	for i, r := range want {
		if got := m.local(c.disp.charAt(2, 5+i)); got != r {
			t.Errorf("field cell (2,%d) = %q, want %q", 5+i, rune(got), rune(r))
		}
	}
	// The field's MDT must be set (the host-written data is "modified").
	f := c.disp.dbuf().fieldList
	if f == nil || !f.mdt() {
		t.Errorf("word-wrap field MDT not set after WRITE-DATA")
	}
}

// TestWriteDataStructuredFieldFlagClear verifies a WRITE-DATA WDSF with the
// 0x80 flag CLEAR is consumed without touching the field (and without desyncing
// the following order stream).
func TestWriteDataStructuredFieldFlagClear(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	wd := []byte{0x00, m.remote('X'), m.remote('Y')} // flag clear
	wdsfLen := 4 + len(wd)
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5,
		OrderSF, 0x00, 0x00, 0x86, 0x80, Attr5250Green, 0x00, 0x0A,
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFWriteData,
	}
	data = append(data, wd...)
	// Ordinary text after the WDSF proves the stream stayed aligned.
	data = append(data, OrderSBA, 10, 10, m.remote('Z'))
	c.sess.processRecord(makeRecord(opPutGet, data))

	if got := c.disp.charAt(2, 5); got != 0x00 {
		t.Errorf("flag-clear WRITE-DATA wrote to the field (cell 2,5 = 0x%02X), want null", got)
	}
	if got := m.local(c.disp.charAt(9, 9)); got != 'Z' {
		t.Errorf("text after WRITE-DATA at (9,9) = %q, want 'Z' (stream desynced)", got)
	}
}

// ── EBCDIC CP037 round-trip ─────────────────────────────────────────────

func TestEBCDICRoundTrip037(t *testing.T) {
	m := newCharMap("37")
	// Every local byte must survive local -> EBCDIC -> local unchanged.
	for b := 0; b < 256; b++ {
		e := m.toRemote[b]
		back := m.toLocal[e]
		if int(back) != b {
			t.Fatalf("CP037 round-trip failed for 0x%02X: -> 0x%02X -> 0x%02X", b, e, back)
		}
	}
	// Spot-check a few well-known code points.
	cases := []struct {
		local byte
		ebc   byte
	}{
		{'A', 0xC1}, {'Z', 0xE9}, {'a', 0x81}, {'0', 0xF0}, {'9', 0xF9}, {' ', 0x40},
	}
	for _, c := range cases {
		if got := m.toRemoteRune(rune(c.local)); got != c.ebc {
			t.Errorf("toRemote(%q) = 0x%02X, want 0x%02X", c.local, got, c.ebc)
		}
		if got := m.toLocalRune(c.ebc); got != rune(c.local) {
			t.Errorf("toLocal(0x%02X) = %q, want %q", c.ebc, got, c.local)
		}
	}
}

func TestEBCDICBracketVariant(t *testing.T) {
	m := newCharMap("37-bracket")
	if m.toRemote['['] != 0x4A || m.toRemote[']'] != 0x5A {
		t.Fatalf("bracket map remote mismatch: [=0x%02X ]=0x%02X", m.toRemote['['], m.toRemote[']'])
	}
	if m.local(0x4A) != '[' || m.local(0x5A) != ']' {
		t.Fatalf("bracket map local mismatch: 0x4A=%q 0x5A=%q", m.local(0x4A), m.local(0x5A))
	}
}

// ── Telnet negotiation ──────────────────────────────────────────────────

func TestTelnetNegotiation(t *testing.T) {
	host := []byte{
		tnIAC, tnDO, optNewEnviron,
		tnIAC, tnDO, optTerminalType,
		tnIAC, tnDO, optEndOfRecord,
		tnIAC, tnDO, optTransmitBinary,
		tnIAC, tnSB, optTerminalType, sbSEND, tnIAC, tnSE,
		tnIAC, tnSB, optNewEnviron, sbSEND, tnIAC, tnSE,
	}
	conn := newMockConn(host)
	st := newStream(conn, "IBM-3179-2", "37", "DEV1", "USB")

	// Drive the state machine until the input is exhausted; negotiation replies
	// are written as a side effect.
	_, err := st.readRecord()
	if err == nil {
		t.Fatal("expected EOF from readRecord after negotiation")
	}

	out := conn.written()

	wantSub := [][]byte{
		{tnIAC, tnWILL, optNewEnviron},
		{tnIAC, tnWILL, optTerminalType},
		{tnIAC, tnWILL, optEndOfRecord},
		{tnIAC, tnWILL, optTransmitBinary},
		// Terminal-type IS reply carrying the device string.
		append([]byte{tnIAC, tnSB, optTerminalType, sbIS}, []byte("IBM-3179-2")...),
	}
	for _, w := range wantSub {
		if !bytes.Contains(out, w) {
			t.Errorf("negotiation reply missing subsequence %v\n got %v", w, out)
		}
	}
	// NEW-ENVIRON IS reply present and mentions our env vars.
	if !bytes.Contains(out, []byte{tnIAC, tnSB, optNewEnviron, sbIS}) {
		t.Error("missing NEW-ENVIRON IS reply")
	}
	if !bytes.Contains(out, []byte("DEVNAME")) || !bytes.Contains(out, []byte("CODEPAGE")) {
		t.Error("NEW-ENVIRON reply missing DEVNAME/CODEPAGE")
	}
}

// TestNegotiationDeviceAndNoCharset covers FIX 5 (configurable DEVNAME/KBDTYPE,
// DEVNAME omitted when empty) and FIX 8 (CHARSET is never sent).
func TestNegotiationDeviceAndNoCharset(t *testing.T) {
	trigger := []byte{
		tnIAC, tnDO, optNewEnviron,
		tnIAC, tnSB, optNewEnviron, sbSEND, tnIAC, tnSE,
	}

	// Configured device name + kbd type are sent; CHARSET is never sent.
	conn := newMockConn(trigger)
	st := newStream(conn, "IBM-3179-2", "273", "MYDEV", "AGB")
	_, _ = st.readRecord()
	out := conn.written()
	if !bytes.Contains(out, []byte("DEVNAME")) || !bytes.Contains(out, []byte("MYDEV")) {
		t.Error("configured DEVNAME not sent")
	}
	if !bytes.Contains(out, []byte("KBDTYPE")) || !bytes.Contains(out, []byte("AGB")) {
		t.Error("configured KBDTYPE not sent")
	}
	if bytes.Contains(out, []byte("CHARSET")) {
		t.Error("CHARSET must not be sent — lib5250 never sends it")
	}

	// An empty device name omits DEVNAME entirely (host auto-assigns); CODEPAGE
	// is always present.
	conn2 := newMockConn(trigger)
	st2 := newStream(conn2, "IBM-3179-2", "37", "", "USB")
	_, _ = st2.readRecord()
	out2 := conn2.written()
	if bytes.Contains(out2, []byte("DEVNAME")) {
		t.Error("empty device name must omit DEVNAME")
	}
	if !bytes.Contains(out2, []byte("CODEPAGE")) {
		t.Error("CODEPAGE must always be sent")
	}
}

// ── Write-To-Display parsing ────────────────────────────────────────────

func TestWriteToDisplay(t *testing.T) {
	conn := newMockConn(nil)
	st := newStream(conn, "IBM-3179-2", "37", "", "USB")
	disp := newDisplay(80, 24, "37")
	sess := newSession(disp, st, "IBM-3179-2")

	m := newCharMap("37")
	// WTD: CC1=0x00, CC2=0x08(unlock); SBA(2,2) + "HI"; SBA(3,5) + SF input
	// field len 5, green attr; IC(5,5).
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 2, 2, m.remote('H'), m.remote('I'),
		OrderSBA, 3, 5, OrderSF, 0x00, 0x00, Attr5250Green, 0x00, 0x05,
		OrderIC, 5, 5,
	}
	sess.processRecord(makeRecord(opPutGet, data))

	// Text landed at row 1 (0-based), cols 1 and 2.
	if got := m.local(disp.charAt(1, 1)); got != 'H' {
		t.Errorf("char at (1,1) = %q, want 'H'", got)
	}
	if got := m.local(disp.charAt(1, 2)); got != 'I' {
		t.Errorf("char at (1,2) = %q, want 'I'", got)
	}

	// One input field created; data starts just past the attribute at (2,5).
	db := disp.dbuf()
	if db.fieldCount != 1 {
		t.Fatalf("field count = %d, want 1", db.fieldCount)
	}
	f := db.fieldList
	if f.startRow != 2 || f.startCol != 5 || f.length != 5 {
		t.Errorf("field = (row %d,col %d,len %d), want (2,5,5)", f.startRow, f.startCol, f.length)
	}
	// Attribute byte sits one position before the field data.
	if got := disp.charAt(2, 4); got != Attr5250Green {
		t.Errorf("attribute at (2,4) = 0x%02X, want 0x%02X", got, Attr5250Green)
	}

	// Keyboard unlocked (CC2 unlock) so the cursor homed to the IC target (4,4).
	if disp.cursorY() != 4 || disp.cursorX() != 4 {
		t.Errorf("cursor = (%d,%d), want (4,4)", disp.cursorY(), disp.cursorX())
	}
	if !disp.pendingInsert {
		t.Error("expected pending insert cursor after IC order")
	}
}

// ── AID response (modified field + Enter) ───────────────────────────────

func TestAIDResponse(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// One record: define an input field then enter a Read MDT Fields.
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x00, 0x00, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadMDTFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))

	if c.sess.readOpcode != CmdReadMDTFields {
		t.Fatalf("read opcode = 0x%02X, want 0x%02X", c.sess.readOpcode, CmdReadMDTFields)
	}

	// Field data start position = row 2, col 5.
	addr := 2*80 + 5
	if err := c.SendAID("Enter", 2, 5, map[int]string{addr: "AB"}); err != nil {
		t.Fatalf("SendAID: %v", err)
	}

	// Extract the inbound data from the framed packet: strip the 10-byte header
	// and the trailing IAC EOR.
	out := conn.written()
	if len(out) < 12 {
		t.Fatalf("inbound packet too short: %v", out)
	}
	if out[9] != opPutGet {
		t.Errorf("inbound opcode = 0x%02X, want 0x%02X (PUT/GET)", out[9], opPutGet)
	}
	inbound := out[10 : len(out)-2] // drop IAC EOR

	want := []byte{
		3, 6, AIDEnter, // cursor row+1, col+1, Enter aid
		OrderSBA, 3, 6, // SBA to field start (row+1, col+1)
		m.remote('A'), m.remote('B'),
	}
	if !bytes.Equal(inbound, want) {
		t.Fatalf("inbound = %v\n            want %v", inbound, want)
	}
}

// ── Snapshot colour/field mapping ───────────────────────────────────────

func TestSnapshotColorsAndFields(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// White protected text then a green input field.
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 1, 1, Attr5250White, m.remote('H'), m.remote('I'),
		OrderSBA, 2, 1, OrderSF, 0x00, 0x00, Attr5250Green, 0x00, 0x04,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))

	snap := c.buildSnapshot()
	if snap.Rows != 24 || snap.Cols != 80 {
		t.Fatalf("snapshot size = %dx%d, want 24x80", snap.Rows, snap.Cols)
	}
	// The attribute cell at (0,0) is a field-attribute position. tn5250 draws
	// attribute positions as a plain normal-green blank regardless of the colour
	// they establish for the cells that follow (cursesterm.c: attribute_map[0]).
	attrCell := snap.Cells[0]
	if !attrCell.FieldAttr || attrCell.Fg != "green" || attrCell.Char != ' ' || attrCell.Intense {
		t.Errorf("attr cell = %+v, want green blank FieldAttr", attrCell)
	}
	// The 'H' following it is white and protected (output text).
	hCell := snap.Cells[1]
	if hCell.Char != 'H' || hCell.Fg != "white" || !hCell.Protected {
		t.Errorf("H cell = %+v, want white protected 'H'", hCell)
	}
	// Exactly one field, unprotected, at data addr row1,col1.
	if len(snap.Fields) != 1 {
		t.Fatalf("fields = %d, want 1", len(snap.Fields))
	}
	sf := snap.Fields[0]
	if sf.Addr != 1*80+1 || sf.Length != 4 || sf.Protected {
		t.Errorf("field = %+v, want addr %d len 4 unprotected", sf, 1*80+1)
	}
}

// ── Enhanced GUI: Create Window WDSF ─────────────────────────────────────

func TestCreateWindowStructuredField(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// Border Presentation Minor Structure chars: UL,top,UR,left,right,LL,bottom,LR.
	border := []byte{
		m.remote('+'), m.remote('-'), m.remote('+'), m.remote('|'),
		m.remote('|'), m.remote('+'), m.remote('-'), m.remote('+'),
	}
	// Create Window payload: flag1, 2 reserved, depth(5), width(10), then a border
	// minor structure [len=13][type=0x01][flags][mono][color][8 chars].
	payload := []byte{0x00, 0x00, 0x00, 5, 10, 13, 0x01, 0x80, 0x00, 0x00}
	payload = append(payload, border...)
	// WDSF length field counts the 2 length bytes + class + type + payload.
	wdsfLen := 4 + len(payload)

	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, // cursor -> row 2, col 4 (0-based): the window's top-left corner
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFCreateWindow,
	}
	data = append(data, payload...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	snap := c.buildSnapshot()
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(snap.Windows))
	}
	w := snap.Windows[0]
	if w.Row != 2 || w.Col != 4 || w.Width != 10 || w.Height != 5 {
		t.Errorf("window geometry = (row %d,col %d,w %d,h %d), want (2,4,10,5)",
			w.Row, w.Col, w.Width, w.Height)
	}
	if w.Border != "+-+||+-+" {
		t.Errorf("window border = %q, want %q", w.Border, "+-+||+-+")
	}

	// The ordinary (non-GUI) cell grid must be untouched by the overlay.
	if len(snap.Cells) != 24*80 {
		t.Fatalf("cells = %d, want %d", len(snap.Cells), 24*80)
	}
}

// TestSaveScreenPreservesWindow covers FIX 3: a GUI overlay (Create Window)
// must survive Save Screen -> Restore Screen. The saved WTD data (makeWTDData)
// must contain the window's WDSF byte-sequence (SBA-prefixed) so that replaying
// it recreates the window with the same geometry and border.
func TestSaveScreenPreservesWindow(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	border := []byte{
		m.remote('+'), m.remote('-'), m.remote('+'), m.remote('|'),
		m.remote('|'), m.remote('+'), m.remote('-'), m.remote('+'),
	}
	payload := []byte{0x00, 0x00, 0x00, 5, 10, 13, 0x01, 0x80, 0x00, 0x00}
	payload = append(payload, border...)
	wdsfLen := 4 + len(payload)
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, // window top-left corner -> row 2, col 4 (0-based)
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFCreateWindow,
	}
	data = append(data, payload...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	if len(c.buildSnapshot().Windows) != 1 {
		t.Fatalf("precondition: expected 1 window before save")
	}

	// Save Screen: reconstruct the WTD data for the active buffer.
	var buf []byte
	c.disp.makeWTDData(&buf)

	// The save-data must carry the window's WDSF so a replay can recreate it.
	if !bytes.Contains(buf, []byte{OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFCreateWindow}) {
		t.Fatalf("save-data missing the Create Window WDSF; a Restore would lose the window\n buf=% X", buf)
	}

	// Full round-trip: replay the saved data into a fresh client and confirm the
	// window is recreated with the same geometry and border.
	c2 := NewClient("3179-2", "37")
	c2.attach(newMockConn(nil))
	c2.sess.processRecord(makeRecord(opRestoreScr, buf))
	snap := c2.buildSnapshot()
	if len(snap.Windows) != 1 {
		t.Fatalf("restore recreated %d windows, want 1", len(snap.Windows))
	}
	w := snap.Windows[0]
	if w.Row != 2 || w.Col != 4 || w.Width != 10 || w.Height != 5 {
		t.Errorf("restored window geometry = (row %d,col %d,w %d,h %d), want (2,4,10,5)",
			w.Row, w.Col, w.Width, w.Height)
	}
	if w.Border != "+-+||+-+" {
		t.Errorf("restored window border = %q, want %q", w.Border, "+-+||+-+")
	}
}

// TestSaveScreenPreservesMultipleWindows extends FIX 3 coverage to TWO windows at
// different positions — exercising the convert emit-loop that re-emits every
// construct in the live list, each SBA-prefixed to its own stored position.
func TestSaveScreenPreservesMultipleWindows(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	border := []byte{
		m.remote('+'), m.remote('-'), m.remote('+'), m.remote('|'),
		m.remote('|'), m.remote('+'), m.remote('-'), m.remote('+'),
	}
	// A Create Window WDSF of given depth×width (with a border minor structure).
	mkWin := func(depth, width byte) []byte {
		payload := []byte{0x00, 0x00, 0x00, depth, width, 13, 0x01, 0x80, 0x00, 0x00}
		payload = append(payload, border...)
		wdsfLen := 4 + len(payload)
		return append([]byte{OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFCreateWindow}, payload...)
	}
	// Window A: 5×10 at (2,4); Window B: 3×8 at (10,20).
	data := []byte{ESC, CmdWriteToDisplay, 0x00, ctlUnlock, OrderSBA, 3, 5}
	data = append(data, mkWin(5, 10)...)
	data = append(data, OrderSBA, 11, 21)
	data = append(data, mkWin(3, 8)...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	if got := len(c.buildSnapshot().Windows); got != 2 {
		t.Fatalf("precondition: expected 2 windows before save, got %d", got)
	}

	var buf []byte
	c.disp.makeWTDData(&buf)

	// Round-trip: replay the saved data into a fresh client.
	c2 := NewClient("3179-2", "37")
	c2.attach(newMockConn(nil))
	c2.sess.processRecord(makeRecord(opRestoreScr, buf))
	snap := c2.buildSnapshot()
	if len(snap.Windows) != 2 {
		t.Fatalf("restore recreated %d windows, want 2\n buf=% X", len(snap.Windows), buf)
	}
	// Both geometries must survive (order-independent).
	var wA, wB bool
	for _, w := range snap.Windows {
		if w.Row == 2 && w.Col == 4 && w.Width == 10 && w.Height == 5 {
			wA = true
		}
		if w.Row == 10 && w.Col == 20 && w.Width == 8 && w.Height == 3 {
			wB = true
		}
	}
	if !wA || !wB {
		t.Errorf("restored windows missing expected geometry; got %+v", snap.Windows)
	}
}

func TestDefineSelectionFieldStructuredField(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// 16-byte header: flags(3), type=0x11 (single-choice selection field),
	// 5 reserved, itemsize=8, height=3, items=2, padding, separator, selchar,
	// cancelaid.
	header := []byte{0x00, 0x00, 0x00, 0x11, 0, 0, 0, 0, 0, 8, 3, 2, 0, 0, 0, 0}

	// Choice 1 "OK", default/selected. Minor struct = [len][0x10][flag1,flag2,
	// flag3, text...]; flag1 0x40 => selected+available, flag3 0x80 => GUI device.
	item1 := []byte{0x40, 0x00, 0x80, m.remote('O'), m.remote('K')}
	minor1 := append([]byte{byte(len(item1) + 2), 0x10}, item1...)
	// Choice 2 "Cancel", available, not selected.
	item2 := []byte{0x00, 0x00, 0x80,
		m.remote('C'), m.remote('a'), m.remote('n'), m.remote('c'), m.remote('e'), m.remote('l')}
	minor2 := append([]byte{byte(len(item2) + 2), 0x10}, item2...)

	payload := append(append(append([]byte{}, header...), minor1...), minor2...)
	wdsfLen := 4 + len(payload)

	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 5, 10, // cursor -> row 4, col 9 (0-based)
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFDefineSelectionField,
	}
	data = append(data, payload...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	snap := c.buildSnapshot()
	if len(snap.Selections) != 1 {
		t.Fatalf("selections = %d, want 1", len(snap.Selections))
	}
	sel := snap.Selections[0]
	if sel.Row != 4 || sel.Col != 9 || sel.Type != 0x11 {
		t.Errorf("selection = (row %d,col %d,type 0x%02X), want (4,9,0x11)", sel.Row, sel.Col, sel.Type)
	}
	if len(sel.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(sel.Items))
	}
	i0 := sel.Items[0]
	if i0.Text != "OK" || !i0.Selected || !i0.Available || i0.Row != 4 || i0.Col != 10 {
		t.Errorf("item0 = %+v, want text OK selected available at (4,10)", i0)
	}
	i1 := sel.Items[1]
	if i1.Text != "Cancel" || i1.Selected || !i1.Available || i1.Row != 5 || i1.Col != 10 {
		t.Errorf("item1 = %+v, want text Cancel available (not selected) at (5,10)", i1)
	}
}

func TestDefineScrollbarStructuredField(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)

	// flag1=0x00 (vertical), reserved, total=0025, slider=0010, size=8.
	payload := []byte{0x00, 0x00, 0, 0, 2, 5, 0, 0, 1, 0, 8}
	wdsfLen := 4 + len(payload)

	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 2, 3, // cursor -> row 1, col 2 (0-based)
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFDefineScrollBarField,
	}
	data = append(data, payload...)
	c.sess.processRecord(makeRecord(opPutGet, data))

	snap := c.buildSnapshot()
	if len(snap.Scrollbars) != 1 {
		t.Fatalf("scrollbars = %d, want 1", len(snap.Scrollbars))
	}
	sb := snap.Scrollbars[0]
	if sb.Row != 1 || sb.Col != 2 || sb.Horizontal || sb.Total != 25 || sb.Slider != 10 || sb.Size != 8 {
		t.Errorf("scrollbar = %+v, want (row1,col2,vert,total25,slider10,size8)", sb)
	}
}

// TestGUIByteStreamStaysSynced feeds a Create Window WDSF followed by ordinary
// Write-To-Display text and asserts the text still lands correctly — i.e. the
// GUI parser consumed exactly the SF payload and left the stream aligned.
func TestGUIByteStreamStaysSynced(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	payload := []byte{0x00, 0x00, 0x00, 4, 8} // no border minor structure
	wdsfLen := 4 + len(payload)
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 2, 2,
		OrderWDSF, byte(wdsfLen >> 8), byte(wdsfLen & 0xff), 0xD9, WDSFCreateWindow,
	}
	data = append(data, payload...)
	// Ordinary text AFTER the SF: SBA(10,10) + "ZZ".
	data = append(data, OrderSBA, 10, 10, m.remote('Z'), m.remote('Z'))
	c.sess.processRecord(makeRecord(opPutGet, data))

	if got := m.local(c.disp.charAt(9, 9)); got != 'Z' {
		t.Errorf("text after WDSF at (9,9) = %q, want 'Z' (stream desynced)", got)
	}
	if got := m.local(c.disp.charAt(9, 10)); got != 'Z' {
		t.Errorf("text after WDSF at (9,10) = %q, want 'Z' (stream desynced)", got)
	}
	if len(c.buildSnapshot().Windows) != 1 {
		t.Errorf("expected 1 window parsed")
	}
}

// TestNoGUIOverlaysOnPlainScreen guards the additive-overlay contract: a plain
// screen carries no GUI overlays at all.
func TestNoGUIOverlaysOnPlainScreen(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 1, 1, m.remote('H'), m.remote('I'),
	}
	c.sess.processRecord(makeRecord(opPutGet, data))

	snap := c.buildSnapshot()
	if len(snap.Windows) != 0 || len(snap.Selections) != 0 || len(snap.Scrollbars) != 0 {
		t.Errorf("plain screen carried GUI overlays: w=%d s=%d sb=%d",
			len(snap.Windows), len(snap.Selections), len(snap.Scrollbars))
	}
}

// TestResetClearsHostInhibit covers the engine's Error-Reset (Client.Reset),
// mirroring tn5250 display.c do_key(K_RESET). A host Write-Error-Code leaves the
// display inhibited with the message line overwritten by the error text; Reset
// must clear the inhibit, restore the saved message line, drop the snapshot's
// Inhibit flag, and return to the unlocked keystate.
func TestResetClearsHostInhibit(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	d := c.disp
	m := newCharMap("37")

	// Lay down known original content on the message line, then reproduce the
	// state writeErrorCode leaves (session.go): save that line, overwrite it with
	// an error, enter the post-help keystate, and inhibit input.
	l := d.buffers.msgLine()
	w := d.width()
	for i := 0; i < w; i++ {
		d.buffers.data[l*w+i] = m.remote('.')
	}
	d.saveMsgLine()
	d.setMsgLine([]byte{m.remote('E'), m.remote('R'), m.remote('R')})
	d.keystate = KeyStatePostHelp
	d.inhibit()

	// Precondition: inhibited, error text on the message line.
	if !d.inhibited() || !c.buildSnapshot().Inhibit {
		t.Fatal("precondition: display should be inhibited")
	}
	if got := m.local(d.charAt(l, 0)); got != 'E' {
		t.Fatalf("precondition: message line should show error, got %q", got)
	}

	c.Reset()

	if d.inhibited() {
		t.Error("Reset left the inhibit indicator set")
	}
	if c.buildSnapshot().Inhibit {
		t.Error("snapshot still reports Inhibit after Reset")
	}
	if d.keystate != KeyStateUnlocked {
		t.Errorf("keystate = %d, want KeyStateUnlocked", d.keystate)
	}
	if got := m.local(d.charAt(l, 0)); got != '.' {
		t.Errorf("message line not restored: got %q, want '.'", got)
	}
}

// TestTestReqWired verifies the Test Request key is exposed to the UI: the name
// "TestReq" maps to the engine's aidTestReq AID (handled in session_read.go as a
// recHTRQ packet), and SendAID drives it without error. This is the missing
// 5250 key added to the Key Mapper.
func TestTestReqWired(t *testing.T) {
	code, local, ok := aidCodeForName("TestReq")
	if !ok || local || code != aidTestReq {
		t.Fatalf("aidCodeForName(TestReq) = (%d, local=%v, ok=%v), want (%d, false, true)",
			code, local, ok, aidTestReq)
	}

	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	if err := c.SendAID("TestReq", 0, 0, nil); err != nil {
		t.Fatalf("SendAID(TestReq) returned error: %v", err)
	}
	if len(conn.written()) == 0 {
		t.Error("SendAID(TestReq) sent nothing; expected a Test Request packet")
	}
}

// TestResetDeniedWhenHostLocked verifies Reset does not unlock while the host
// holds the keyboard (KeyStateLocked / X SYSTEM), matching do_key's LOCKED-state
// gate: the operator must not jump ahead of a host still processing a response.
// It ALSO verifies a snapshot IS still emitted in the denied case, so the
// frontend (whose doReset optimistically cleared its own lock) re-syncs to the
// authoritative, still-locked keystate instead of desyncing.
func TestResetDeniedWhenHostLocked(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	d := c.disp

	// Record the emitted snapshot so we can assert Reset re-syncs the frontend.
	var got *Snapshot
	c.SetUpdateCallback(func(s *Snapshot) { got = s })

	d.keystate = KeyStateLocked
	d.inhibit()

	c.Reset()

	if !d.inhibited() {
		t.Error("Reset cleared the inhibit while host-locked; it should be denied")
	}
	if d.keystate != KeyStateLocked {
		t.Errorf("keystate = %d, want KeyStateLocked (unchanged)", d.keystate)
	}
	// Even when denied, Reset must emit a snapshot carrying the still-locked state.
	if got == nil {
		t.Fatal("Reset emitted no snapshot when host-locked; frontend would desync")
	}
	if !got.Inhibit || !got.KeyboardLocked {
		t.Errorf("emitted snapshot = {Inhibit:%v KeyboardLocked:%v}, want still inhibited & locked",
			got.Inhibit, got.KeyboardLocked)
	}
}

// TestAttnUninhibitsAndUnlocks covers Attn's K_ATTENTION recovery semantics
// (display.c ~1468: tn5250_display_uninhibit(This); This->keystate = UNLOCKED;
// BEFORE dispatching the ATTN AID). A host Write-Error-Code leaves the display
// inhibited ("X II") with the message line overwritten and the keyboard in the
// post-help state; pressing Attn must clear the inhibit, restore the saved
// message line, force the keyboard back to UNLOCKED, and still send.
func TestAttnUninhibitsAndUnlocks(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	d := c.disp
	m := newCharMap("37")

	// Reproduce the inhibited/overwritten state writeErrorCode leaves (mirrors
	// TestResetClearsHostInhibit): original '.' content on the message line, saved
	// and overwritten with "ERR", post-help keystate, input inhibited.
	l := d.buffers.msgLine()
	w := d.width()
	for i := 0; i < w; i++ {
		d.buffers.data[l*w+i] = m.remote('.')
	}
	d.saveMsgLine()
	d.setMsgLine([]byte{m.remote('E'), m.remote('R'), m.remote('R')})
	d.keystate = KeyStatePostHelp
	d.inhibit()

	if !d.inhibited() {
		t.Fatal("precondition: display should be inhibited")
	}
	if got := m.local(d.charAt(l, 0)); got != 'E' {
		t.Fatalf("precondition: message line should show error, got %q", got)
	}

	c.SendAttn()

	if d.inhibited() {
		t.Error("Attn left the inhibit indicator set")
	}
	if got := m.local(d.charAt(l, 0)); got != '.' {
		t.Errorf("message line not restored after Attn: got %q, want '.'", got)
	}
	if d.keystate != KeyStateUnlocked {
		t.Errorf("keystate after Attn = %d, want KeyStateUnlocked", d.keystate)
	}
	// The ATTN packet must still have been transmitted to the host.
	if len(conn.written()) == 0 {
		t.Error("SendAttn sent nothing; expected an ATTN packet")
	}
}

// TestHomeTarget verifies display.homeTarget reports the tn5250 Home/IC target
// WITHOUT moving the cursor (client.buildSnapshot exposes it so the browser's
// Home key can honor the host's Insert-Cursor position): the IC position when
// pendingInsert is set, otherwise the first NON-bypass field (skipping bypass
// fields), otherwise 0,0.
func TestHomeTarget(t *testing.T) {
	d := newDisplay(80, 24, "37")

	// No fields, no pending insert: home target is 0,0.
	if r, cc := d.homeTarget(); r != 0 || cc != 0 {
		t.Errorf("homeTarget with no fields = (%d,%d), want (0,0)", r, cc)
	}

	// pendingInsert set: home target is the host IC position, and the cursor is
	// NOT moved by querying it.
	d.buffers.setIC(7, 12)
	d.pendingInsert = true
	beforeY, beforeX := d.cursorY(), d.cursorX()
	if r, cc := d.homeTarget(); r != 7 || cc != 12 {
		t.Errorf("homeTarget with pendingInsert = (%d,%d), want (7,12)", r, cc)
	}
	if d.cursorY() != beforeY || d.cursorX() != beforeX {
		t.Errorf("homeTarget moved the cursor to (%d,%d); it must not move it", d.cursorY(), d.cursorX())
	}

	// Without pendingInsert, home target is the first non-bypass field. Build a
	// bypass field FIRST, then a non-bypass field, to prove the loop skips bypass.
	d.pendingInsert = false
	bypass := newField(80)
	bypass.ffw = FieldBypass
	bypass.startRow, bypass.startCol = 3, 4
	input := newField(80)
	input.ffw = 0x0000 // alpha-shift, not bypass
	input.startRow, input.startCol = 9, 15
	list := fieldListAdd(nil, bypass)
	list = fieldListAdd(list, input)
	d.buffers.fieldList = list

	if r, cc := d.homeTarget(); r != 9 || cc != 15 {
		t.Errorf("homeTarget = (%d,%d), want first non-bypass field (9,15)", r, cc)
	}
}

// TestNumericMinusOverpunchMapping locks in the EBCDIC overpunch contract the
// JS numeric-only Field-minus key relies on: replacing the last digit's DISPLAY
// char with its overpunch letter (0->} 1->J ... 9->R) must translate to EBCDIC
// 0xD0..0xD9 through charMap.toRemoteRune. If any of these fail the JS approach
// would transmit the wrong byte and must be revisited (see task FIX D).
func TestNumericMinusOverpunchMapping(t *testing.T) {
	m := newCharMap("37")
	cases := []struct {
		ch  rune
		ebc byte
	}{
		{'}', 0xD0}, {'J', 0xD1}, {'K', 0xD2}, {'L', 0xD3}, {'M', 0xD4},
		{'N', 0xD5}, {'O', 0xD6}, {'P', 0xD7}, {'Q', 0xD8}, {'R', 0xD9},
	}
	for _, c := range cases {
		if got := m.toRemoteRune(c.ch); got != c.ebc {
			t.Errorf("toRemoteRune(%q) = 0x%02X, want 0x%02X (overpunch contract broken)",
				c.ch, got, c.ebc)
		}
	}
}

// TestNumOnlyOverpunchCodepageIndependent proves the numeric-only negative
// overpunch is code-page independent. A num-only field carries the sign in the
// ZONE of its last digit; the browser writes the digit's EBCDIC overpunch glyph
// ('}' = −0, J..R = −1..−9). setFieldText must translate '}' to 0xD0 DIRECTLY —
// NOT via toRemoteRune — because '}' is a variant byte on national pages
// (toRemoteRune('}') = 0xDC on CP273). We use CP273 (German) so the buggy path
// would emit 0xDC.
func TestNumOnlyOverpunchCodepageIndependent(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "273")
	c.attach(conn)
	// A 4-cell numeric-only field (FFW 0x0300) at row 3,col 5, then Read MDT.
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x03, 0x00, Attr5250Green, 0x00, 0x04,
		ESC, CmdReadMDTFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))
	addr := 2*80 + 5 // first data cell (row 2,col 5 0-based)
	// "123}" — digits 1,2,3 with a negative-zero overpunch in the last position.
	if err := c.SendAID("Enter", 2, 5, map[int]string{addr: "123}"}); err != nil {
		t.Fatalf("SendAID: %v", err)
	}
	inbound := conn.written()
	if !bytes.Contains(inbound, []byte{0xD0}) {
		t.Errorf("overpunch not encoded as 0xD0 (want code-page independent); inbound=% X", inbound)
	}
	if bytes.Contains(inbound, []byte{0xDC}) {
		t.Errorf("overpunch wrongly code-page mapped to 0xDC (CP273 '}'); inbound=% X", inbound)
	}
}

// TestSignedFieldRightJustifiedOnTransmit confirms that a signed-numeric field
// entered left-justified by the browser (as a plain Field Exit does) is right-
// justified by the engine before transmit — setFieldText -> fieldAdjust forces
// RIGHT_BLANK for signed fields — so the units digit is preserved, not lost to
// NULs. (Guards against a regression that would corrupt the value.)
func TestSignedFieldRightJustifiedOnTransmit(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")
	// A 5-cell signed-numeric field (FFW 0x0700 = 4 digits + sign).
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x07, 0x00, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadMDTFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))
	addr := 2*80 + 5
	// Browser sends left-justified "12" (no local right-adjust on plain exit).
	if err := c.SendAID("Enter", 2, 5, map[int]string{addr: "12"}); err != nil {
		t.Fatalf("SendAID: %v", err)
	}
	inbound := conn.written()
	if !bytes.Contains(inbound, []byte{m.remote('1')}) || !bytes.Contains(inbound, []byte{m.remote('2')}) {
		t.Errorf("signed field lost a digit after left-justified entry; inbound=% X", inbound)
	}
}

// TestRefuses3270Host: a TN3270E server (it asks for option 40) and a
// TN3270 server sending a 3270 record (Erase/Write, no 5250 GDS header)
// both end the session with ErrNot5250Host and are hung up on.
func TestRefuses3270Host(t *testing.T) {
	for name, host := range map[string][]byte{
		"TN3270E": {tnIAC, tnDO, optTN3270E},
		"3270 record": {tnIAC, tnDO, optTerminalType, 0xF5, 0xC3, 0x11, 0x40, 0x40,
			0xC8, 0xC5, 0xD3, 0xD3, 0xD6, tnIAC, tnEOR},
	} {
		conn := newMockConn(host)
		c := NewClient("3179-2", "37")
		c.attach(conn)
		err := c.Run()
		if !errors.Is(err, ErrNot5250Host) {
			t.Errorf("%s: Run = %v, want ErrNot5250Host", name, err)
		}
		if !conn.closed { // Close does not take the mutex
			t.Errorf("%s: connection not closed", name)
		}
	}
}

// TestAccepts5250Record: a record with the 5250 GDS header is accepted.
func TestAccepts5250Record(t *testing.T) {
	if !is5250Record(makeRecord(opPutGet, []byte{ESC, CmdClearUnit})) {
		t.Fatal("a 5250 record was taken for a 3270 one")
	}
}
