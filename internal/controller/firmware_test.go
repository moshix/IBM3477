package controller

import (
	"os"
	"testing"

	"ibm3486/internal/board"
)

// statusCell returns the character and attribute of status-row column col.
func statusCell(b *board.Board, col int) (uint8, uint8) {
	rows, cols, _, cells := b.Video.Screen()
	i := ((rows-1)*cols + col) * 3
	return cells[i], cells[i+1]
}

// TestFirmwareShowsIndicators runs the controller against the real 3477
// firmware and checks the OIA symbols it draws in the status row for the
// indicator bits (attribute bit7 = symbol font): system available box at
// col 3, X at col 8, insert caret at col 41, message waiting at col 44.
func TestFirmwareShowsIndicators(t *testing.T) {
	const romDir = "../.."
	if _, err := os.Stat(romDir + "/ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROM not present")
	}
	b, err := board.New(&board.Model3477, os.DirFS(romDir))
	if err != nil {
		t.Fatal(err)
	}
	b.Run(600_000_000) // POST, then the run-time firmware
	const ms = 59818500 / 8 / 1000
	h := newHost()
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	c := New(b.Twinax, -1, ms, h)
	step := func(n int) {
		for i := 0; i < n; i++ {
			b.Run(ms)
			c.Tick(ms)
		}
	}
	step(2000)
	if !c.Online {
		t.Fatal("station not online")
	}
	expect := func(what string, col int, glyph uint8) {
		t.Helper()
		ch, at := statusCell(b, col)
		on := at&0x80 != 0 && ch == glyph
		if glyph == 0 {
			on = ch == 0 || ch == 0x40
		}
		if !on {
			t.Fatalf("%s: status col %d = %02X/%02X, want glyph %02X", what, col, ch, at, glyph)
		}
	}
	expect("system available", 3, 0x02)
	expect("not inhibited", 8, 0)
	// 0x45 bit 0x10: the "»" a real 3477 shows at col 12 when attached
	if ch, at := statusCell(b, 12); ch == 0 || ch == 0x40 {
		t.Fatalf("no symbol at status col 12 (%02X/%02X) after 0x45 [10]", ch, at)
	}

	h.scr.Inhibited, h.scr.ErrorCode, h.scr.Insert, h.scr.MessageWait = true, 0x0005, true, true
	step(300)
	expect("input inhibited", 8, 0x03)
	expect("insert", 41, 0x05)
	expect("message waiting", 44, 0x01)
	// the error code went to the error line (row 24), reverse image
	_, cols, _, cells := b.Video.Screen()
	row := cells[23*cols*3:]
	if row[3] != 0xF0 || row[12] != 0xF5 {
		t.Fatalf("error line starts % X", row[:18])
	}
	t.Logf("error line cells (char attr extra): % X", row[:21])

	h.scr.Inhibited, h.scr.ErrorCode, h.scr.Insert, h.scr.MessageWait = false, 0, false, false
	step(300)
	expect("inhibit cleared", 8, 0)
	expect("insert cleared", 41, 0)
	expect("message waiting cleared", 44, 0)
	expect("still available", 3, 0x02)
	_, _, _, cells = b.Video.Screen() // a fresh copy after Reset
	if r := cells[23*cols*3:]; r[3] == 0xF0 {
		t.Fatalf("error line not restored: % X", r[:18])
	}
}
