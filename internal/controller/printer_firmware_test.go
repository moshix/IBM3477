package controller

import (
	"os"
	"strings"
	"testing"

	"ibm3486/internal/board"
	"ibm3486/internal/printer"
)

// displayPrinterSetup is the default setup memory switched to Terminal
// Mode Display-Printer, printer address 1 (check bytes recomputed).
func displayPrinterSetup() []uint8 {
	nv := make([]uint8, 0x900)
	copy(nv, board.Default3477Setup)
	nv[0x06] = 3                 // 0x1A6 terminal mode: Display-Printer
	nv[0x04], nv[0x05] = 1, 0xE9 // 0x1A4/5 printer address 1 + check
	nv[0x0A] = 2                 // 0x1AA printer address, index form
	sum := 0
	for i := 0x06; i <= 0x3D; i++ {
		sum += int(nv[i])
	}
	nv[0x3E] = uint8(0xEA - sum) // 0x1DE check byte of 0x1A6-0x1DD
	return nv
}

// printerRig boots the real 3477 in Display-Printer mode with the display
// controller (address 0) and the printer station (address 1) attached.
type printerRig struct {
	b    *board.Board
	c    *Controller
	p    *PrinterStation
	h    *fakeHost
	step func(ms int)
}

func newPrinterRig(t *testing.T, localCopy bool) *printerRig {
	t.Helper()
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROM not present")
	}
	if testing.Short() {
		t.Skip("boots the firmware")
	}
	b, err := board.New(&board.Model3477, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.LoadNVRAM(displayPrinterSetup()); err != nil {
		t.Fatal(err)
	}
	b.Run(600_000_000)
	b.IO.PrinterOutput() // the printer set-up sent at start
	const ms = 59818500 / 8 / 1000
	h := newHost()
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	c := New(b.Twinax, 0, ms, h)
	p := NewPrinterStation(b.Twinax, 1, ms)
	c.SetPrinter(p, localCopy)
	r := &printerRig{b: b, c: c, p: p, h: h}
	r.step = func(n int) {
		for i := 0; i < n; i++ {
			b.Run(ms)
			c.Tick(ms)
			p.Tick(ms)
		}
	}
	r.step(2000)
	if !c.Online || !p.Online {
		t.Fatalf("display online %v, printer online %v", c.Online, p.Online)
	}
	return r
}

// put writes ASCII text (letters, digits, blank) as EBCDIC at row, col.
func (r *printerRig) put(row, col int, text string) {
	for i, ch := range text {
		r.h.scr.Data[row*80+col+i] = asciiToEBCDIC(ch)
	}
}

// TestFirmwarePrintKeyLocalCopy: with local copy on, the Print key makes
// the controller send the screen to the terminal's printer station; the
// 3477 formats it and prints it on its port. A password field does not
// print.
func TestFirmwarePrintKeyLocalCopy(t *testing.T) {
	r := newPrinterRig(t, true)
	r.put(0, 2, "MAIN MENU")
	r.put(5, 2, "1. USER TASKS")
	d := r.h.scr.Data
	d[7*80+10] = 0x27 // non-display field: a password
	r.put(7, 11, "SECRET")
	d[7*80+17] = 0x20
	r.step(500)
	kb := r.b.IO.Keyboard()
	kb.Key(0x0C) // Print
	r.step(50)
	kb.Key(0xF0, 0x0C)
	r.step(4000)
	if len(r.h.keys) != 0 {
		t.Errorf("Print went to the host: %v", r.h.keys)
	}
	doc := printer.Interpret(r.b.IO.PrinterOutput())
	text := doc.Text()
	if !strings.Contains(text, "MAIN MENU") || !strings.Contains(text, "1. USER TASKS") {
		t.Fatalf("printed %q", text)
	}
	if strings.Contains(text, "SECRET") {
		t.Fatalf("password printed: %q", text)
	}
}

// TestFirmwarePrintKeyToHost: without local copy the Print key goes to
// the host, as before; nothing is printed.
func TestFirmwarePrintKeyToHost(t *testing.T) {
	r := newPrinterRig(t, false)
	kb := r.b.IO.Keyboard()
	kb.Key(0x0C)
	r.step(50)
	kb.Key(0xF0, 0x0C)
	r.step(2000)
	if len(r.h.keys) != 1 || r.h.keys[0] != "Print" {
		t.Fatalf("host keys %v", r.h.keys)
	}
	if out := r.b.IO.PrinterOutput(); printer.Interpret(out).HasText() {
		t.Fatalf("printed without local copy: %q", printer.Interpret(out).Text())
	}
}

// TestFirmwarePrinterStationLongJob: SCS data longer than one 256-byte
// block reaches the printer completely and in order.
func TestFirmwarePrinterStationLongJob(t *testing.T) {
	r := newPrinterRig(t, true)
	var scs []byte
	var want []string
	for i := 0; i < 30; i++ {
		line := "LINE " + string(rune('A'+i%26)) + strings.Repeat("X", 70)
		want = append(want, line)
		for _, ch := range line {
			scs = append(scs, asciiToEBCDIC(ch))
		}
		scs = append(scs, scsNL)
	}
	scs = append(scs, scsFF)
	r.p.Print(scs)
	for i := 0; i < 200 && !r.p.Idle(); i++ {
		r.step(100)
	}
	r.step(4000)
	text := printer.Interpret(r.b.IO.PrinterOutput()).Text()
	got := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(got) < len(want) {
		t.Fatalf("%d lines printed, want %d:\n%s", len(got), len(want), text)
	}
	for i, w := range want {
		if strings.TrimSpace(got[i]) != w {
			t.Fatalf("line %d = %q, want %q", i, got[i], w)
		}
	}
}
