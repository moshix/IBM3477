package controller

import (
	"testing"

	"ibm3486/internal/board"
)

// excStation is a station at a settable address that can be put into an
// exception: while one is pending it drops every command except Write
// Control Data with reset-exception, as the 3477 firmware does
// (C000:935A). rejectWDLC raises exception 6 for every Write Data.
type excStation struct {
	addr       uint8
	exc        uint8 // pending exception status bits (0 = none)
	rejectWDLC bool
	got        []message // commands accepted
	resets     int
}

func (f *excStation) Deliver(addr, cmd uint8, data []uint8) (board.Response, bool) {
	if addr != f.addr {
		return board.Response{}, false
	}
	switch cmd {
	case cmdPoll:
		return board.Response{Polled: true, Status: f.exc}, true
	case cmdAck:
		return board.Response{}, true
	}
	if f.exc != 0 {
		if cmd == cmdWCD && len(data) > 0 && data[0]&wcdResetException != 0 {
			f.exc = 0
			f.resets++
		} else {
			return board.Response{}, true // dropped
		}
	}
	if cmd == cmdWDLC && f.rejectWDLC {
		f.exc = 0x0C
		return board.Response{}, true
	}
	f.got = append(f.got, message{cmd, append([]uint8(nil), data...)})
	return board.Response{}, true
}

func (f *excStation) count(cmd uint8) int {
	n := 0
	for _, m := range f.got {
		if m.cmd == cmd {
			n++
		}
	}
	return n
}

// TestExceptionRecovered: an exception other than power-on (here 6,
// status 0x0C, as for an address beyond the station's screen) is reset
// and the screen repainted; before, the station stayed stuck dropping
// every write.
func TestExceptionRecovered(t *testing.T) {
	st := &excStation{}
	h := newHost()
	c := New(st, 0, 100, h)
	run(c, 20)
	if st.count(cmdWDLC) == 0 {
		t.Fatal("screen not painted")
	}
	for _, code := range []uint8{0x0C, 0x08, 0x02, 0x04, 0x0A} {
		st.exc = code
		st.got = nil
		run(c, 20)
		if st.exc != 0 {
			t.Fatalf("exception %02X not reset", code)
		}
		if st.count(cmdWDLC) == 0 || st.count(cmdClear) == 0 {
			t.Fatalf("exception %02X: screen not repainted: %+v", code, st.got)
		}
		// later host changes reach the station again
		h.scr.Data[0]++
		st.got = nil
		run(c, 5)
		if st.count(cmdWDLC) == 0 {
			t.Fatalf("exception %02X: later change not sent", code)
		}
	}
}

// TestRecurringExceptionBacksOff: a station that raises an exception for
// every write is reset with growing pauses, not in a tight loop.
func TestRecurringExceptionBacksOff(t *testing.T) {
	st := &excStation{rejectWDLC: true}
	c := New(st, 0, 100, newHost())
	run(c, 5000)
	if st.resets == 0 {
		t.Fatal("exception never reset")
	}
	// delays 1, 2, 4, ... 2048 polls: about a dozen resets in 5000 polls
	if st.resets > 20 {
		t.Fatalf("%d exception resets in 5000 polls", st.resets)
	}
	// once the station accepts the screen, the delay starts over
	st.rejectWDLC = false
	run(c, 2100)
	if st.exc != 0 {
		t.Fatal("still in exception")
	}
	if c.recoverDelay != 0 {
		t.Fatalf("recover delay %d after a clean repaint", c.recoverDelay)
	}
}

// TestScreenSizeSet: before clearing the station screen, the controller
// selects the host's screen size with Write Control Data [44 08] (27x132)
// or [44 00] (24x80).
func TestScreenSizeSet(t *testing.T) {
	st := &excStation{}
	h := newHost()
	h.scr.Rows, h.scr.Cols, h.scr.Data = 27, 132, make([]byte, 27*132)
	c := New(st, 0, 100, h)
	run(c, 10)
	wantSize := func(b uint8) {
		t.Helper()
		for i, m := range st.got {
			if m.cmd == cmdClear {
				t.Fatalf("clear before the screen size: %+v", st.got[:i+1])
			}
			if m.cmd == cmdWCD && len(m.data) == 2 && m.data[0]&wcdSecondByte != 0 {
				if m.data[1] != b || m.data[0]&wcdResetException == 0 {
					t.Fatalf("screen size WCD % X, want 44 %02X", m.data, b)
				}
				return
			}
		}
		t.Fatalf("no screen size WCD in %+v", st.got)
	}
	wantSize(0x08)
	st.got = nil
	h.scr.Rows, h.scr.Cols, h.scr.Data = 24, 80, make([]byte, 24*80)
	run(c, 10)
	wantSize(0x00)
}

// TestRescanAfterAddressChange: when the station stops answering (its
// address changed in the terminal's Setup), the controller searches the
// addresses again, finds it and repaints the screen there.
func TestRescanAfterAddressChange(t *testing.T) {
	st := &excStation{addr: 2}
	c := New(st, -1, 100, newHost())
	run(c, 20)
	if !c.Online || c.Address() != 2 {
		t.Fatalf("online=%v addr=%d, want station 2", c.Online, c.Address())
	}
	st.addr, st.got = 5, nil
	run(c, lostPolls+20)
	if !c.Online || c.Address() != 5 {
		t.Fatalf("online=%v addr=%d after the address change, want 5", c.Online, c.Address())
	}
	if st.count(cmdWDLC) == 0 {
		t.Fatal("screen not repainted at the new address")
	}
}

// TestShiftKeysTrackedSeparately: with both Shift keys held, releasing
// one keeps Shift down.
func TestShiftKeysTrackedSeparately(t *testing.T) {
	st := &fakeStation{}
	h := newHost()
	c := New(st, 0, 100, h)
	run(c, 10)
	// left Shift down, right Shift down, right Shift up, b, left up, b
	st.scans = []uint8{0x57, 0x56, 0xD6, 0x05, 0xD7, 0x05}
	run(c, 10)
	if string(h.typed) != "Bb" {
		t.Fatalf("typed %q, want Bb", string(h.typed))
	}
}

// lagStation is an excStation that, like the real firmware, processes a
// command queue after it was delivered: the next poll reports busy, and
// an exception the queue raises shows only on the poll after that.
type lagStation struct {
	excStation
	work    bool  // a queue was delivered and not processed yet
	raise   uint8 // exception the unprocessed queue will raise
	onReset func()
	// rejectCmd (non-zero) raises exception 6 for that command: with
	// cmdWCDInd it is the last queue of a repaint
	rejectCmd uint8
}

func (f *lagStation) Deliver(addr, cmd uint8, data []uint8) (board.Response, bool) {
	if addr != f.addr {
		return board.Response{}, false
	}
	switch cmd {
	case cmdPoll:
		if f.work {
			// busy now; the queue's outcome is visible next time
			f.work = false
			st := f.exc | stBusy
			f.exc |= f.raise
			f.raise = 0
			return board.Response{Polled: true, Status: st}, true
		}
		return board.Response{Polled: true, Status: f.exc}, true
	case cmdAck:
		return board.Response{}, true
	}
	f.work = true
	if f.exc != 0 {
		if cmd == cmdWCD && len(data) > 0 && data[0]&wcdResetException != 0 {
			f.exc = 0
			f.resets++
			if f.onReset != nil {
				f.onReset()
			}
		}
		return board.Response{}, true
	}
	if (cmd == cmdWDLC && f.rejectWDLC) || (f.rejectCmd != 0 && cmd == f.rejectCmd) {
		f.raise = 0x0C
		return board.Response{}, true
	}
	f.got = append(f.got, message{cmd, append([]uint8(nil), data...)})
	return board.Response{}, true
}

// TestRecurringExceptionBacksOffWhenLate: the back-off also holds when the
// station reports the exception only after a busy poll (as the firmware
// does). Before, the busy poll counted as a clean repaint and reset the
// delay, so the station was reset and repainted every few polls.
func TestRecurringExceptionBacksOffWhenLate(t *testing.T) {
	// the last queue of every repaint (0x45 indicators) is rejected
	st := &lagStation{rejectCmd: cmdWCDInd}
	h := newHost()
	h.scr.Connected = true // so the 0x45 indicators are sent
	c := New(st, 0, 100, h)
	run(c, 5000)
	if st.resets == 0 {
		t.Fatal("exception never reset")
	}
	if st.resets > 20 {
		t.Fatalf("%d exception resets in 5000 polls: back-off reset by busy polls", st.resets)
	}
	st.rejectCmd = 0
	run(c, 2100)
	if st.exc != 0 || c.recoverDelay != 0 {
		t.Fatalf("exc=%02X delay=%d after the station accepts the screen", st.exc, c.recoverDelay)
	}
}

// TestPowerOnDuringRecovery: a station that powers on again (exception 7)
// after the controller's reset went out is brought up from the start,
// instead of the controller waiting forever for a recovery that ended.
func TestPowerOnDuringRecovery(t *testing.T) {
	st := &lagStation{excStation: excStation{exc: stException}}
	h := newHost()
	c := New(st, 0, 100, h)
	repowered := false
	st.onReset = func() {
		if !repowered {
			repowered = true
			st.exc = stException // power cycle right after the first reset
		}
	}
	run(c, 200)
	if !repowered {
		t.Fatal("test station never reset")
	}
	if st.exc != 0 {
		t.Fatalf("station still in exception %02X", st.exc)
	}
	if st.count(cmdWDLC) == 0 {
		t.Fatal("screen never painted after the second power-on")
	}
}

// TestIndicatorsResentAfterResize: when the host screen changes size the
// station's status row moves, so the indicators are sent again even
// though their value did not change.
func TestIndicatorsResentAfterResize(t *testing.T) {
	st := &excStation{}
	h := newHost()
	c := New(st, 0, 100, h)
	run(c, 20)
	if st.count(cmdWDLCInd) == 0 {
		t.Fatal("indicators never sent")
	}
	st.got = nil
	h.scr.Rows, h.scr.Cols, h.scr.Data = 27, 132, make([]byte, 27*132)
	run(c, 20)
	if st.count(cmdWDLCInd) == 0 {
		t.Fatal("indicators not sent again after the screen size changed")
	}
	if st.count(cmdWCDInd) == 0 && wcdIndicatorByte(h.scr) != 0 {
		t.Fatal("0x45 indicators not sent again after the screen size changed")
	}
}
