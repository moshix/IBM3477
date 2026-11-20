package controller

import (
	"testing"

	"ibm3486/internal/board"
	"ibm3486/internal/tn5250"
)

// fakeStation behaves like a terminal at address 0: it reports power-on
// (exception 7) until Write Control Data resets the exception, hands out
// queued scan codes one per poll, and records every command it receives.
type fakeStation struct {
	exception bool
	scans     []uint8
	got       []message
}

func (f *fakeStation) Deliver(addr, cmd uint8, data []uint8) (board.Response, bool) {
	if addr != 0 {
		return board.Response{}, false
	}
	switch cmd {
	case cmdPoll:
		r := board.Response{Polled: true}
		if f.exception {
			r.Status |= stException
		}
		if len(f.scans) > 0 {
			r.Scan = f.scans[0]
			f.scans = f.scans[1:]
		}
		return r, true
	case cmdAck:
		return board.Response{}, true
	case cmdWCD:
		if len(data) > 0 && data[0]&wcdResetException != 0 {
			f.exception = false
		}
	}
	f.got = append(f.got, message{cmd, append([]uint8(nil), data...)})
	return board.Response{}, true
}

// fakeHost is a 2x20 screen that records keystrokes.
type fakeHost struct {
	scr   tn5250.ScreenState
	typed []rune
	keys  []string
}

func (h *fakeHost) Screen() tn5250.ScreenState { return h.scr }
func (h *fakeHost) TypeChar(r rune)            { h.typed = append(h.typed, r) }
func (h *fakeHost) Key(name string)            { h.keys = append(h.keys, name) }

func newHost() *fakeHost {
	h := &fakeHost{scr: tn5250.ScreenState{Rows: 2, Cols: 20, Data: make([]byte, 40)}}
	// "SIGN ON" (EBCDIC) at row 1, col 2; cursor at row 1, col 10
	copy(h.scr.Data[22:], []byte{0xE2, 0xC9, 0xC7, 0xD5, 0x40, 0xD6, 0xD5})
	h.scr.CursorRow, h.scr.CursorCol = 1, 10
	return h
}

func run(c *Controller, polls int) {
	for i := 0; i < polls; i++ {
		c.Tick(c.pollEvery)
	}
}

func TestBringUpAndScreenMirror(t *testing.T) {
	st := &fakeStation{exception: true}
	h := newHost()
	c := New(st, 0, 100, h)
	run(c, 20)
	if !c.Online {
		t.Fatal("station not online")
	}
	if len(st.got) < 4 || st.got[0].cmd != cmdSetMode || st.got[2].cmd != cmdWCD {
		t.Fatalf("bring-up sequence wrong: %+v", st.got)
	}
	// the screen text arrives as LAC 22 + WDLC "SIGN ON", then the cursor
	var sawText, sawCursor bool
	for i, m := range st.got {
		if m.cmd == cmdWDLC && len(m.data) == 8 && m.data[0] == 7 && m.data[1] == 0xE2 {
			prev := st.got[i-1]
			sawText = prev.cmd == cmdLAC && prev.data[0] == 0 && prev.data[1] == 22
		}
		if m.cmd == cmdLCR && m.data[1] == 30 {
			sawCursor = true
		}
	}
	if !sawText || !sawCursor {
		t.Fatalf("text=%v cursor=%v in %+v", sawText, sawCursor, st.got)
	}
	// no further traffic once the station is up to date
	before := len(st.got)
	run(c, 5)
	if len(st.got) != before {
		t.Fatalf("idle screen still sends %d commands", len(st.got)-before)
	}
	// a host change is sent as just the changed bytes
	h.scr.Data[0] = 0xC1
	run(c, 3)
	if n := len(st.got) - before; n == 0 {
		t.Fatal("screen change not mirrored")
	}
}

func TestKeystrokesReachHost(t *testing.T) {
	st := &fakeStation{}
	h := newHost()
	c := New(st, 0, 100, h)
	// twinax key codes as the 3477 sends them (kbdlayout.ToTwinax):
	// a, Shift+b, Shift+F1 (PF13), Shift release, Reset, F13 (Cmd Shift 1,
	// PF13), F1 (PF1: the terminal releases the Shift of the F13 sequence
	// before the next key), Caps Lock on, c (C), Right arrow, Shift+Tab
	// (BackTab)
	run(c, 10) // first repaint: keys typed before it are dropped
	st.scans = []uint8{0x11, 0x57, 0x05, 0x6F, 0x31, 0xD7, 0x7E,
		0x6F, 0x57, 0x31, 0xD7, 0x6F, 0x31, 0x54, 0xD4, 0x03, 0x73,
		0x57, 0x20, 0xD7}
	run(c, 25)
	if string(h.typed) != "aBC" {
		t.Fatalf("typed %q, want aBC", string(h.typed))
	}
	want := []string{"PF13", "Reset", "PF13", "PF1", "Right", "BackTab"}
	if len(h.keys) != len(want) {
		t.Fatalf("keys %v, want %v", h.keys, want)
	}
	for i := range want {
		if h.keys[i] != want[i] {
			t.Fatalf("keys %v, want %v", h.keys, want)
		}
	}
}

func TestNoStation(t *testing.T) {
	c := New(&fakeStation{}, 3, 100, newHost()) // nobody at address 3
	run(c, 3)
	if c.Online {
		t.Fatal("online without a station")
	}
}

// TestFindsStationAddress: with addr -1 the controller polls 0-6 and
// settles on the station that answers.
func TestFindsStationAddress(t *testing.T) {
	st := &fakeStation{}
	c := New(st, -1, 100, newHost())
	c.addr = 4 // start elsewhere: wraps 4,5,6,0
	run(c, 6)
	if !c.Online || c.Address() != 0 {
		t.Fatalf("online=%v addr=%d, want station 0", c.Online, c.Address())
	}
}

// indicatorsSent returns the data bytes of every 0x51 command received.
func (f *fakeStation) indicatorsSent() []uint8 {
	var v []uint8
	for _, m := range f.got {
		if m.cmd == cmdWDLCInd && len(m.data) == 1 {
			v = append(v, m.data[0])
		}
	}
	return v
}

// TestIndicators: the indicator byte follows the host state and is sent
// once after bring-up and then only when it changes.
func TestIndicators(t *testing.T) {
	st := &fakeStation{exception: true}
	h := newHost()
	h.scr.Connected = true
	c := New(st, 0, 100, h)
	run(c, 20)
	if got := st.indicatorsSent(); len(got) != 1 || got[0] != indSystemAvailable {
		t.Fatalf("after bring-up: indicators % X, want [80]", got)
	}
	run(c, 5)
	if n := len(st.indicatorsSent()); n != 1 {
		t.Fatalf("unchanged indicators resent (%d sends)", n)
	}
	steps := []struct {
		name string
		set  func(*tn5250.ScreenState)
		want uint8
	}{
		{"host lock", func(s *tn5250.ScreenState) { s.Inhibited, s.HostLocked = true, true }, 0x82},
		{"unlock + message waiting", func(s *tn5250.ScreenState) {
			s.Inhibited, s.HostLocked, s.MessageWait = false, false, true
		}, 0xC0},
		{"insert", func(s *tn5250.ScreenState) { s.Insert = true }, 0xC8},
		{"all off", func(s *tn5250.ScreenState) {
			s.Insert, s.MessageWait, s.Connected = false, false, false
		}, 0x00},
	}
	for _, step := range steps {
		step.set(&h.scr)
		run(c, 3)
		got := st.indicatorsSent()
		if got[len(got)-1] != step.want {
			t.Fatalf("%s: last indicators %02X, want %02X", step.name, got[len(got)-1], step.want)
		}
	}
	// a new power-on (exception 7) resends the indicators after bring-up
	before := len(st.indicatorsSent())
	st.exception = true
	run(c, 20)
	if n := len(st.indicatorsSent()); n != before+1 {
		t.Fatalf("indicators not resent after power-on: %d -> %d", before, n)
	}
}

// TestOperatorErrorCode: an operator error lights Input Inhibited and
// shows the error code in reverse image at the start of the error line;
// Reset restores the host's row.
func TestOperatorErrorCode(t *testing.T) {
	st := &fakeStation{exception: true}
	h := newHost()
	h.scr.Connected = true
	h.scr.ErrorRow = 1
	c := New(st, 0, 100, h)
	run(c, 20)
	mark := len(st.got)
	h.scr.Inhibited, h.scr.ErrorCode = true, 0x0005
	run(c, 5)
	want := []uint8{6, attrReverse, 0xF0, 0xF0, 0xF0, 0xF5, attrNormal}
	var sawCode bool
	for i, m := range st.got[mark:] {
		if m.cmd == cmdWDLC && string(m.data) == string(want) {
			prev := st.got[mark+i-1]
			sawCode = prev.cmd == cmdLAC && prev.data[0] == 0 && prev.data[1] == 20
		}
	}
	if !sawCode {
		t.Fatalf("error code not written at row 1 col 0: %+v", st.got[mark:])
	}
	if got := st.indicatorsSent(); got[len(got)-1] != indSystemAvailable|indInputInhibited {
		t.Fatalf("indicators % X, want last 82", got)
	}
	// Reset: the row comes back (blanks there in the host screen)
	mark = len(st.got)
	h.scr.Inhibited, h.scr.ErrorCode = false, 0
	run(c, 5)
	var restored bool
	for _, m := range st.got[mark:] {
		if m.cmd == cmdWDLC && len(m.data) == 7 && m.data[0] == 6 && m.data[1] == 0 {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("error line not restored: %+v", st.got[mark:])
	}
	if got := st.indicatorsSent(); got[len(got)-1] != indSystemAvailable {
		t.Fatalf("indicators % X, want last 80", got)
	}
}

// TestStationImageHexCode: error codes are four hex digits (SRC 0x0010
// shows as 0010, 0x001A as 001A).
func TestStationImageHexCode(t *testing.T) {
	s := tn5250.ScreenState{Rows: 2, Cols: 10, Data: make([]byte, 20),
		Inhibited: true, ErrorCode: 0x001A, ErrorRow: 1}
	img := stationImage(s)
	if got := img[10:16]; string(got) != string([]byte{attrReverse, 0xF0, 0xF0, 0xF1, 0xC1, attrNormal}) {
		t.Fatalf("error line % X", got)
	}
	if s.Data[10] != 0 {
		t.Fatal("stationImage modified the host screen")
	}
	s.ErrorCode = 0 // host lock, no code: screen unchanged
	if img := stationImage(s); &img[0] != &s.Data[0] {
		t.Fatal("host lock should not overlay the error line")
	}
}

// TestWCDIndicatorAndShutdown: while connected the 0x45 indicator byte
// carries 0x10 (the "»" at col 12 on a real 3477); Shutdown clears both
// indicator bytes.
func TestWCDIndicatorAndShutdown(t *testing.T) {
	st := &fakeStation{}
	h := newHost()
	h.scr.Connected = true
	c := New(st, 0, 100, h)
	run(c, 30)
	var saw bool
	for _, m := range st.got {
		if m.cmd == cmdWCDInd && len(m.data) == 1 && m.data[0] == wcdIndSession {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no 0x45 [10] sent: %+v", st.got)
	}
	before := len(st.got)
	c.Shutdown()
	cleared := map[uint8]bool{}
	for _, m := range st.got[before:] {
		if (m.cmd == cmdWDLCInd || m.cmd == cmdWCDInd) && m.data[0] == 0 {
			cleared[m.cmd] = true
		}
	}
	if !cleared[cmdWDLCInd] || !cleared[cmdWCDInd] {
		t.Fatalf("Shutdown did not clear the indicators: %+v", st.got[before:])
	}
}

// TestTypeaheadBeforeRepaintDropped: keys the terminal queued before the
// host screen was painted are not typed into the host.
func TestTypeaheadBeforeRepaintDropped(t *testing.T) {
	st := &fakeStation{}
	h := newHost()
	c := New(st, 0, 100, h)
	st.scans = []uint8{0x11, 0x12} // "as" waiting in the typeahead queue
	run(c, 2)
	if len(h.typed) != 0 {
		t.Fatalf("typeahead typed into the host: %q", string(h.typed))
	}
}
