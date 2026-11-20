package tn5250

import (
	"bytes"
	"testing"
)

// newOperatorClient returns a connected client showing one 5-byte input
// field at row 2 (0-based), cols 5-9, inside a Read MDT Fields.
func newOperatorClient(t *testing.T, ffw1 byte) (*Client, *mockConn) {
	t.Helper()
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, ffw1, 0x00, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadMDTFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))
	c.disp.setCursor(2, 5)
	return c, conn
}

func fieldText(c *Client) string {
	st := c.Screen()
	var b []byte
	for _, e := range st.Data[2*st.Cols+5 : 2*st.Cols+10] {
		b = append(b, c.disp.cmap.local(e))
	}
	return string(bytes.TrimRight(b, "\x00 ")) // blank = EBCDIC 0x40 or null
}

func TestTypeCharFillsFieldAndAdvances(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40) // FFW 0x40xx: plain alphanumeric
	for _, r := range "AB" {
		c.TypeChar(r)
	}
	if got := fieldText(c); got != "AB" {
		t.Fatalf("field = %q, want AB", got)
	}
	if st := c.Screen(); st.CursorRow != 2 || st.CursorCol != 7 {
		t.Fatalf("cursor = %d,%d, want 2,7", st.CursorRow, st.CursorCol)
	}
	// Insert mode shifts the rest of the field right
	c.disp.setCursor(2, 5)
	c.Key("Insert")
	c.TypeChar('X')
	if got := fieldText(c); got != "XAB" {
		t.Fatalf("after insert: %q, want XAB", got)
	}
	c.Key("Delete")
	if got := fieldText(c); got != "XB" {
		t.Fatalf("after delete: %q, want XB", got)
	}
}

func TestTypeOutsideFieldLocksKeyboard(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40)
	c.disp.setCursor(0, 0)
	c.TypeChar('A')
	st := c.Screen()
	if !st.Inhibited || st.ErrorCode != KBDSrcProtect {
		t.Fatalf("inhibited=%v code=%04X, want protect error", st.Inhibited, st.ErrorCode)
	}
	c.TypeChar('B') // ignored while locked
	c.Key("Reset")
	if c.Screen().Inhibited {
		t.Fatal("Reset did not unlock the keyboard")
	}
}

func TestNumericFieldRejectsLetters(t *testing.T) {
	c, _ := newOperatorClient(t, 0x43) // FFW type 3 = numeric only
	c.TypeChar('7')
	c.TypeChar('Z')
	st := c.Screen()
	if !st.Inhibited || st.ErrorCode != KBDSrcNumOnly {
		t.Fatalf("code=%04X, want numeric-only error", st.ErrorCode)
	}
	if got := fieldText(c); got != "7" {
		t.Fatalf("field = %q, want 7", got)
	}
}

func TestKeyEnterSendsTypedField(t *testing.T) {
	c, conn := newOperatorClient(t, 0x40)
	for _, r := range "HI" {
		c.TypeChar(r)
	}
	c.Key("Enter")
	out := conn.written()
	m := newCharMap("37")
	if len(out) < 12 || !bytes.Contains(out, []byte{m.remote('H'), m.remote('I')}) {
		t.Fatalf("inbound record lacks the typed field: % X", out)
	}
	if out[len(out)-2] != 0xFF || out[len(out)-1] != 0xEF {
		t.Fatalf("inbound record not terminated by IAC EOR: % X", out)
	}
}

func TestEraseEOFAndTab(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40)
	for _, r := range "ABCD" {
		c.TypeChar(r)
	}
	c.disp.setCursor(2, 6)
	c.Key("EraseEOF")
	if got := fieldText(c); got != "A" {
		t.Fatalf("after ErEOF: %q, want A", got)
	}
	c.disp.setCursor(0, 0)
	c.Key("Tab")
	if st := c.Screen(); st.CursorRow != 2 || st.CursorCol != 5 {
		t.Fatalf("Tab went to %d,%d, want 2,5", st.CursorRow, st.CursorCol)
	}
}

// TestScreenOIAState: Screen reports the OIA state the twinax controller
// turns into station indicators.
func TestScreenOIAState(t *testing.T) {
	if st := NewClient("3179-2", "37").Screen(); st.Connected {
		t.Fatal("unconnected client reports Connected")
	}
	c, _ := newOperatorClient(t, 0x40)
	st := c.Screen()
	if !st.Connected || st.HostLocked || st.MessageWait || st.ErrorRow != 23 {
		t.Fatalf("idle: %+v", st)
	}
	// an operator error is not a host lock
	c.disp.setCursor(0, 0)
	c.TypeChar('A')
	if st := c.Screen(); !st.Inhibited || st.HostLocked {
		t.Fatalf("operator error: inhibited=%v hostLocked=%v", st.Inhibited, st.HostLocked)
	}
	c.Key("Reset")
	// the host locks the keyboard (X SYSTEM) and sets Message Waiting
	c.disp.indicatorSet(indXSystem | indMessageWaiting)
	c.disp.keystate = KeyStateLocked
	if st := c.Screen(); !st.Inhibited || !st.HostLocked || !st.MessageWait || st.ErrorCode != 0 {
		t.Fatalf("host lock: %+v", st)
	}
}

// TestBackspaceDeletes: Backspace moves left and deletes that character,
// the rest of the field shifting left; at the field start it only moves.
func TestBackspaceDeletes(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40) // field starts at row 2, col 5
	for _, r := range "ABC" {
		c.TypeChar(r)
	}
	c.Key("Backspace")
	if got := fieldText(c); got != "AB" {
		t.Fatalf("after backspace: %q, want AB", got)
	}
	if st := c.Screen(); st.CursorRow != 2 || st.CursorCol != 7 {
		t.Fatalf("cursor = %d,%d, want 2,7", st.CursorRow, st.CursorCol)
	}
	// in the middle: the following characters close the gap
	c.disp.setCursor(2, 6)
	c.Key("Backspace")
	if got := fieldText(c); got != "B" {
		t.Fatalf("after middle backspace: %q, want B", got)
	}
	// at the field start: only the cursor moves, nothing is deleted
	c.Key("Backspace")
	if got := fieldText(c); got != "B" {
		t.Fatalf("backspace at field start deleted: %q", got)
	}
	if st := c.Screen(); st.CursorRow != 2 || st.CursorCol != 4 || st.Inhibited {
		t.Fatalf("cursor = %d,%d inhibited=%v, want 2,4 unlocked", st.CursorRow, st.CursorCol, st.Inhibited)
	}
}

// TestMoveCursor: a click moves the cursor; off-screen positions do not.
func TestMoveCursor(t *testing.T) {
	c, _ := newOperatorClient(t, 0x40)
	c.MoveCursor(10, 33)
	if st := c.Screen(); st.CursorRow != 10 || st.CursorCol != 33 {
		t.Fatalf("cursor = %d,%d, want 10,33", st.CursorRow, st.CursorCol)
	}
	c.MoveCursor(24, 0) // the status row is not part of the host screen
	c.MoveCursor(-1, 5)
	if st := c.Screen(); st.CursorRow != 10 || st.CursorCol != 33 {
		t.Fatalf("cursor moved to %d,%d by an off-screen click", st.CursorRow, st.CursorCol)
	}
}
