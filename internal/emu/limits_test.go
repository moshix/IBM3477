package emu

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ibm3486/internal/tn5250"
)

func newTestMachine(t testing.TB) *Machine {
	t.Helper()
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStopTwice: a second Stop (or concurrent ones) must not close the
// stop channel again (that panicked).
func TestStopTwice(t *testing.T) {
	m := newTestMachine(t)
	m.Start(func(*Snapshot) {})
	time.Sleep(20 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Stop() }()
	}
	wg.Wait()
	m.Stop()
}

// TestPasteTruncated: one paste types at most MaxPasteChars characters.
func TestPasteTruncated(t *testing.T) {
	m := &Machine{paste: make(chan []pasteStep, 1)}
	if tr, busy := m.Paste(strings.Repeat("ab", 10)); tr || busy {
		t.Fatalf("short paste: truncated=%v busy=%v", tr, busy)
	}
	<-m.paste
	if tr, _ := m.Paste(strings.Repeat("ä", MaxPasteChars+100)); !tr {
		t.Fatal("long paste not reported as truncated")
	}
	if steps := <-m.paste; len(steps) != MaxPasteChars {
		t.Fatalf("%d steps queued, want %d", len(steps), MaxPasteChars)
	}
}

// TestPasteWhileTypingDropped: pastes that arrive while one is being
// typed are dropped instead of piling up (200 pastes of 5000 characters
// used to queue a million steps).
func TestPasteWhileTypingDropped(t *testing.T) {
	m := newTestMachine(t)
	m.Start(func(*Snapshot) {})
	text := strings.Repeat("a", 5000)
	for i := 0; i < 200; i++ {
		m.Paste(text)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	m.Stop() // the machine goroutine is gone: pending can be read
	if n := len(m.pending); n > len(text) {
		t.Fatalf("%d paste steps pending, want at most one paste (%d)", n, len(text))
	}
}

// TestPublisherIdleNoWork: while nothing on the screen changes, checking
// for changes allocates nothing (no text decoding, no font copy) and
// publishes nothing; a change is published.
func TestPublisherIdleNoWork(t *testing.T) {
	m := newTestMachine(t)
	m.b.Run(cpuHz) // the firmware loads the font and clears the screen
	p := newPublisher()
	now := time.Now()
	if s := p.check(m, now); s == nil || s.Font == nil || len(s.Text) == 0 {
		t.Fatal("first check must publish the screen and the font")
	}
	allocs := testing.AllocsPerRun(100, func() {
		now = now.Add(40 * time.Millisecond)
		if s := p.check(m, now); s != nil {
			t.Fatal("unchanged screen published again")
		}
	})
	if allocs != 0 {
		t.Fatalf("idle check allocates %.0f times, want 0", allocs)
	}
	// a changed cell is published, with fresh text and no font
	start := uint32(0x8000) + uint32(m.b.Video.Reg16(0x30))
	m.b.Mem()[start] = 0xC1 // 'A' in row 0, col 0
	now = now.Add(40 * time.Millisecond)
	s := p.check(m, now)
	if s == nil || s.Font != nil || !strings.HasPrefix(s.Text[0], "A") {
		t.Fatalf("changed cell: snapshot %v", s)
	}
}

// TestScreenTextAllocs: decoding a screen allocates per row, not per
// cell (it built a codepage map for every cell).
func TestScreenTextAllocs(t *testing.T) {
	cells := make([]byte, 25*80*3)
	for i := 0; i < len(cells); i += 3 {
		cells[i], cells[i+1] = 0xC1, 0x20
	}
	dec := tn5250.EBCDICTable("37")
	allocs := testing.AllocsPerRun(20, func() { screenText(cells, 80, dec) })
	if allocs > 25+2 {
		t.Fatalf("screenText allocates %.0f times for 25 rows", allocs)
	}
}

// BenchmarkScreenText decodes a 25x80 screen with the per-codepage
// table; BenchmarkScreenTextPerCellLookup is the former way (a codepage
// lookup for every cell), for comparison.
func BenchmarkScreenText(b *testing.B) {
	cells := make([]byte, 25*80*3)
	for i := 0; i < len(cells); i += 3 {
		cells[i], cells[i+1] = 0xC1, 0x20
	}
	dec := tn5250.EBCDICTable("37")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		screenText(cells, 80, dec)
	}
}

func BenchmarkScreenTextPerCellLookup(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for c := 0; c < 25*80; c++ {
			tn5250.EBCDICToRune("37", 0xC1)
		}
	}
}

// TestPasteWaitsForKeyboard: a pasted key is pressed only once the keys
// before it have left the keyboard's 16-byte buffer, so a paste can't
// overflow it (overflow drops keys) while the firmware is busy; if the
// buffer never drains, the paste is given up instead of hanging.
func TestPasteWaitsForKeyboard(t *testing.T) {
	m := newTestMachine(t)
	kb := m.b.IO.Keyboard()
	// let the firmware boot and take the keyboard's power-on bytes
	for i := 0; i < 50 && kb.Pending(); i++ {
		m.b.Run(10_000_000)
	}
	if !m.pasteKeyboardReady() {
		t.Fatal("empty keyboard buffer: paste not ready")
	}
	kb.Key(0x1C) // a key the terminal has not taken yet
	m.pending = pasteSteps("abc")
	m.pasting.Store(true)
	if m.pasteKeyboardReady() {
		t.Fatal("paste ready while key codes still wait in the keyboard buffer")
	}
	if len(m.pending) != 3 {
		t.Fatalf("pending = %d steps, want the paste kept (3)", len(m.pending))
	}
	// the buffer stays full for longer than the stall limit: give up
	m.b.CPU.Cycles += pasteStallLimit + 1
	m.pasteKeyboardReady()
	if len(m.pending) != 0 || m.pasting.Load() {
		t.Fatalf("paste not given up after a stalled keyboard: %d steps, pasting=%v",
			len(m.pending), m.pasting.Load())
	}
}

// TestPasteBusyReported: a paste made while another is being typed is
// refused and reported as busy, so the page can say so.
func TestPasteBusyReported(t *testing.T) {
	m := &Machine{paste: make(chan []pasteStep, 1)}
	if _, busy := m.Paste("first"); busy {
		t.Fatal("first paste reported busy")
	}
	if _, busy := m.Paste("second"); !busy {
		t.Fatal("paste while one is queued not reported busy")
	}
	<-m.paste
	m.pasting.Store(true) // the machine goroutine is typing it
	if _, busy := m.Paste("third"); !busy {
		t.Fatal("paste while one is being typed not reported busy")
	}
	m.pasting.Store(false)
	if _, busy := m.Paste("fourth"); busy {
		t.Fatal("paste after the previous finished reported busy")
	}
}
