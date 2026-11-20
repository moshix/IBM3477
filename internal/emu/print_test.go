package emu

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ibm3486/internal/board"
	"ibm3486/internal/printer"
)

// TestMachinePrintKeyPrints: on the running terminal in Display-Printer
// mode, the Print key with a host attached prints a copy of the screen:
// the printer station gets it, the 3477 prints it on its port, and the
// machine hands over one print job with the screen's text.
func TestMachinePrintKeyPrints(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("runs the terminal for about 15 s in real time")
	}
	// the setup memory file: Display-Printer mode, printer address 1
	nv := make([]uint8, 0x900)
	copy(nv, board.Default3477Setup)
	nv[0x06], nv[0x04], nv[0x05], nv[0x0A] = 3, 1, 0xE9, 2
	sum := 0
	for i := 0x06; i <= 0x3D; i++ {
		sum += int(nv[i])
	}
	nv[0x3E] = uint8(0xEA - sum)
	path, err := NVRAMPath("3477")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, nv, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var jobs []*printer.Document
	m.SetPrintHandler(func(d *printer.Document) { mu.Lock(); jobs = append(jobs, d); mu.Unlock() })
	m.Start(func(*Snapshot) {})
	time.Sleep(9 * time.Second) // POST and logo
	h := &recHost{}
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	for i, c := range []byte{0xD7, 0xD9, 0xC9, 0xD5, 0xE3, 0x40, 0xD4, 0xC5} { // PRINT ME
		h.scr.Data[3*80+4+i] = c
	}
	m.Attach(h)
	time.Sleep(2 * time.Second)
	m.Key(ScanCodes("fn:Print", true))
	time.Sleep(100 * time.Millisecond)
	m.Key(ScanCodes("fn:Print", false))
	time.Sleep(5 * time.Second) // printing, then 2 s quiet ends the job
	// a host print job (SCS from the printer session)
	m.PrintSCS([]byte{0x2B, 0xC1, 0x02, 0x50, 0xC8, 0xD6, 0xE2, 0xE3, 0x40, 0xD1, 0xD6, 0xC2, 0x15, 0x0C}) // HOST JOB
	time.Sleep(5 * time.Second)
	m.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(jobs) != 2 {
		t.Fatalf("%d print jobs, want 2", len(jobs))
	}
	if text := jobs[0].Text(); !strings.Contains(text, "PRINT ME") {
		t.Fatalf("printed %q", text)
	}
	if text := jobs[1].Text(); !strings.Contains(text, "HOST JOB") {
		t.Fatalf("host job printed %q", text)
	}
	if len(h.keys) != 0 {
		t.Errorf("Print also went to the host: %v", h.keys)
	}
}

// TestMachinePrintKeyOneDisplay: with the default setup (One-Display, no
// printer of the terminal's own) the Print key with Local copy still
// makes a print job of the screen, and does not go to the host.
func TestMachinePrintKeyOneDisplay(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("runs the terminal for about 12 s in real time")
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var jobs []*printer.Document
	m.SetPrintHandler(func(d *printer.Document) { mu.Lock(); jobs = append(jobs, d); mu.Unlock() })
	m.Start(func(*Snapshot) {})
	time.Sleep(9 * time.Second)
	h := &recHost{}
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	for i, c := range []byte{0xD4, 0xC1, 0xC9, 0xD5} { // MAIN
		h.scr.Data[1+i] = c
	}
	m.Attach(h)
	time.Sleep(2 * time.Second)
	m.Key(ScanCodes("fn:Print", true))
	time.Sleep(100 * time.Millisecond)
	m.Key(ScanCodes("fn:Print", false))
	time.Sleep(500 * time.Millisecond)
	// a host print job (SCS from the printer session)
	m.PrintSCS([]byte{0x2B, 0xC1, 0x02, 0x50, 0xC8, 0xD6, 0xE2, 0xE3, 0x40, 0xD1, 0xD6, 0xC2, 0x15, 0x0C}) // HOST JOB
	time.Sleep(500 * time.Millisecond)
	m.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(jobs) != 2 || !strings.HasPrefix(jobs[0].Text(), " MAIN") ||
		strings.TrimSpace(jobs[1].Text()) != "HOST JOB" {
		t.Fatalf("%d jobs; text %q", len(jobs), func() string {
			if len(jobs) > 0 {
				return jobs[0].Text()
			}
			return ""
		}())
	}
	if len(h.keys) != 0 {
		t.Errorf("Print went to the host: %v", h.keys)
	}
}
