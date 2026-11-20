package emu

import (
	"os"
	"sync"
	"testing"
	"time"

	"ibm3486/internal/board"
	"ibm3486/internal/controller"
	"ibm3486/internal/tn5250"
)

// recHost is a host session that records what the controller types.
type recHost struct {
	scr   tn5250.ScreenState
	typed []rune
	keys  []string
}

func (h *recHost) Screen() tn5250.ScreenState { return h.scr }
func (h *recHost) TypeChar(r rune)            { h.typed = append(h.typed, r) }
func (h *recHost) Key(name string)            { h.keys = append(h.keys, name) }

// pasteRig boots the real 3477 firmware with a controller and a
// recording host attached.
func pasteRig(t *testing.T) (*board.Board, *controller.Controller, *recHost, func(int)) {
	t.Helper()
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("boots the firmware")
	}
	b, err := board.New(&board.Model3477, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	b.Run(600_000_000)
	h := &recHost{}
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	const ms = cpuHz / 1000
	c := controller.New(b.Twinax, -1, ms, h)
	step := func(n int) {
		for i := 0; i < n; i++ {
			b.Run(ms)
			c.Tick(ms)
		}
	}
	step(2000)
	return b, c, h, step
}

// TestFirmwarePasteASCII: every printable US-ASCII character pasted with
// the paste key sequences and pacing reaches the host unchanged.
func TestFirmwarePasteASCII(t *testing.T) {
	b, _, h, step := pasteRig(t)
	var text []rune
	for r := rune(0x20); r < 0x7F; r++ {
		text = append(text, r)
	}
	for _, g := range keyGroups(string(text)) {
		b.IO.Keyboard().Key(g...)
		step(int(pasteKeyGap / (cpuHz / 1000))) // as the machine pastes
	}
	step(200)
	if string(h.typed) != string(text) || len(h.keys) != 0 {
		t.Fatalf("pasted %q\n     got %q keys %v", string(text), string(h.typed), h.keys)
	}
}

// errHost rejects letters like a numeric field: the keyboard locks with
// an operator error.
type errHost struct{ recHost }

func (h *errHost) TypeChar(r rune) {
	if h.scr.Inhibited {
		return // locked: rejected (a real session beeps)
	}
	h.typed = append(h.typed, r)
	if r >= 'a' && r <= 'z' {
		h.scr.Inhibited, h.scr.ErrorCode = true, 0x0010
	}
}

// TestMachinePaste: on the running terminal, a paste types keyboard
// characters as keys and the others (ä, ß) directly, in order; and a
// paste stops at the first rejected character.
func TestMachinePaste(t *testing.T) {
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
	var clicks uint32 // key clicks played, as the browser gets them
	m.Start(func(s *Snapshot) { mu.Lock(); clicks = s.Sound.Clicks; mu.Unlock() })
	played := func() uint32 { mu.Lock(); defer mu.Unlock(); return clicks }
	time.Sleep(9 * time.Second) // POST and logo, in real time
	h := &recHost{}
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	h.scr.Connected, h.scr.ErrorRow = true, 23
	m.Attach(h)
	time.Sleep(time.Second) // first repaint
	before := played()
	m.Paste("Grüße ZÄZ 1A")
	time.Sleep(2 * time.Second)
	if got := played() - before; got != 0 {
		t.Errorf("paste played %d key clicks, want none", got)
	}
	// typing still clicks
	m.Key(ScanCodes("Space", true))
	time.Sleep(100 * time.Millisecond)
	m.Key(ScanCodes("Space", false))
	time.Sleep(300 * time.Millisecond)
	if got := played() - before; got != 1 {
		t.Errorf("typed key played %d clicks, want 1", got)
	}

	e := &errHost{}
	e.scr = h.scr
	m.Attach(e)
	time.Sleep(time.Second)
	m.Paste("12x3456")
	time.Sleep(2 * time.Second)
	m.Stop() // the machine goroutine has exited: the hosts can be read
	// direct characters (ü ß Ä) between shifted keys must keep their
	// place; the final space is the key typed after the paste
	if got := string(h.typed); got != "Grüße ZÄZ 1A " {
		t.Errorf("pasted %q, want %q", got, "Grüße ZÄZ 1A ")
	}
	if got := string(e.typed); got != "12x" {
		t.Errorf("paste after an error went on: typed %q, want %q", got, "12x")
	}
}
