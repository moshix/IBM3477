// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"bytes"
	"testing"
)

// TestMalformedRecordNoPanic verifies the M1 fix: a truncated/malformed record
// (an order whose operands run off the end before EOR, or a record shorter than
// its header) must degrade gracefully rather than panic and crash the whole
// server process. Before the fix, record.getByte() indexed past the slice.
func TestMalformedRecordNoPanic(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)

	// A Write-To-Display whose Start-of-Field order is missing all of its
	// FFW/FCW/attribute/length operands — the SF handler will read several bytes
	// past the end of the record.
	truncSF := []byte{ESC, CmdWriteToDisplay, 0x00, ctlUnlock, OrderSBA, 3, 5, OrderSF}
	// Direct processing must not panic (getByte now returns 0 past the end).
	c.sess.processRecord(makeRecord(opPutGet, truncSF))

	// The recover() wrapper must also complete cleanly and never take the
	// process down, even for a record shorter than its 10-byte GDS header.
	for _, rec := range [][]byte{
		makeRecord(opPutGet, truncSF),
		{0x00, 0x05, 0x12}, // 3-byte junk "record" (no full header)
		{},                 // empty
	} {
		if _, err := c.processRecordSafe(rec); err != nil {
			t.Errorf("processRecordSafe returned error for malformed record: %v", err)
		}
	}
}

// TestReadInputFieldsResponse covers the READ_INPUT_FIELDS inbound layout, which
// differs from READ_MDT_FIELDS: field data is sent WITHOUT an SBA prefix (all
// input fields are returned in screen order, so positions are implicit) and null
// cells become blanks. Only READ_MDT_FIELDS was previously tested.
func TestReadInputFieldsResponse(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// Define a 5-char input field at (row 2,col 5) then issue Read Input Fields.
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x00, 0x00, Attr5250Green, 0x00, 0x05,
		ESC, CmdReadInputFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))

	// Operator types "AB" into the field and presses Enter.
	addr := 2*80 + 5
	if err := c.SendAID("Enter", 2, 5, map[int]string{addr: "AB"}); err != nil {
		t.Fatalf("SendAID: %v", err)
	}

	out := conn.written()
	if len(out) < 12 {
		t.Fatalf("inbound packet too short: %v", out)
	}
	inbound := out[10 : len(out)-2] // strip 10-byte header + trailing IAC EOR

	// row+1, col+1, Enter, then the 5 field bytes with NO SBA prefix and the
	// three trailing null positions blanked to 0x40.
	want := []byte{
		3, 6, AIDEnter,
		m.remote('A'), m.remote('B'), 0x40, 0x40, 0x40,
	}
	if !bytes.Equal(inbound, want) {
		t.Fatalf("READ_INPUT_FIELDS inbound = %v\n                        want %v", inbound, want)
	}
}

// TestContinuedFieldReconstruction covers the word-wrap/continued field fix:
// a field defined as continued-first + continued-last must transmit the WHOLE
// reconstructed field under the first subfield's position, not just the first
// segment (session_read.go, from session.c:487-520).
func TestContinuedFieldReconstruction(t *testing.T) {
	conn := newMockConn(nil)
	c := NewClient("3179-2", "37")
	c.attach(conn)
	m := newCharMap("37")

	// Two continued subfields then Read Input Fields.
	// FCW 0x8601 = continued-first (len 3 @ row2,col5); 0x8602 = continued-last
	// (len 2 @ row2,col10).
	data := []byte{
		ESC, CmdWriteToDisplay, 0x00, ctlUnlock,
		OrderSBA, 3, 5, OrderSF, 0x00, 0x00, 0x86, 0x01, Attr5250Green, 0x00, 0x03,
		OrderSBA, 3, 10, OrderSF, 0x00, 0x00, 0x86, 0x02, Attr5250Green, 0x00, 0x02,
		ESC, CmdReadInputFields, 0x00, 0x00,
	}
	c.sess.processRecord(makeRecord(opPutGet, data))

	// Operator types "ABC" in the first subfield and "DE" in the second.
	if err := c.SendAID("Enter", 2, 5, map[int]string{2*80 + 5: "ABC", 2*80 + 10: "DE"}); err != nil {
		t.Fatalf("SendAID: %v", err)
	}

	out := conn.written()
	inbound := out[10 : len(out)-2]
	// row+1,col+1,Enter, then the reconstructed "ABCDE" (both subfields joined),
	// with NO separate entry for the second subfield.
	want := []byte{
		3, 6, AIDEnter,
		m.remote('A'), m.remote('B'), m.remote('C'), m.remote('D'), m.remote('E'),
	}
	if !bytes.Equal(inbound, want) {
		t.Fatalf("continued inbound = %v\n                want %v", inbound, want)
	}
}
