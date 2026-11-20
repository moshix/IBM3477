package controller

import (
	"bytes"
	"os"
	"testing"

	"ibm3486/internal/board"
	"ibm3486/internal/kbdlayout"
)

// scanRecorder passes messages to the station and keeps the key codes
// that came back in poll responses.
type scanRecorder struct {
	st   Station
	scan []uint8
}

func (r *scanRecorder) Deliver(addr, cmd uint8, data []uint8) (board.Response, bool) {
	resp, ok := r.st.Deliver(addr, cmd, data)
	if ok && resp.Polled && resp.Scan != 0 {
		r.scan = append(r.scan, resp.Scan)
	}
	return resp, ok
}

// firmwareRig boots the real 3477 firmware with a controller attached.
type firmwareRig struct {
	b    *board.Board
	c    *Controller
	h    *fakeHost
	rec  *scanRecorder
	step func(ms int)
}

func newFirmwareRig(t *testing.T) *firmwareRig {
	t.Helper()
	const romDir = "../.."
	if _, err := os.Stat(romDir + "/ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROM not present")
	}
	if testing.Short() {
		t.Skip("boots the firmware")
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
	rec := &scanRecorder{st: b.Twinax}
	c := New(rec, -1, ms, h)
	r := &firmwareRig{b: b, c: c, h: h, rec: rec}
	r.step = func(n int) {
		for i := 0; i < n; i++ {
			b.Run(ms)
			c.Tick(ms)
		}
	}
	r.step(2000)
	if !c.Online {
		t.Fatal("station not online")
	}
	return r
}

// tap presses and releases a set-3 key, with Shift held if shift.
func (r *firmwareRig) tap(code uint8, shift bool) {
	kb := r.b.IO.Keyboard()
	if shift {
		kb.Key(kbdlayout.ShiftLeft)
		r.step(30)
	}
	kb.Key(code)
	r.step(60)
	kb.Key(0xF0, code)
	r.step(60)
	if shift {
		kb.Key(0xF0, kbdlayout.ShiftLeft)
		r.step(30)
	}
}

// TestFirmwareKeyTranslation: every key in kbdlayout.ToTwinax reaches the
// controller as exactly that sequence, once (no repeats, no second copy
// on release).
func TestFirmwareKeyTranslation(t *testing.T) {
	r := newFirmwareRig(t)
	for code, want := range kbdlayout.ToTwinax {
		// a key that made the terminal send Shift make (F13, ErInp) is
		// followed by Shift break before the next key: let that go first
		r.tap(0x29, false) // Space
		r.rec.scan = nil
		r.tap(code, false)
		got := r.rec.scan
		switch code {
		case kbdlayout.ShiftLeft, kbdlayout.ShiftRight, 0x14:
			// make/break keys: make, then make|0x80
			want = []uint8{want[0], want[0] | kbdlayout.TwinaxRelease}
		}
		if !bytes.Equal(got, want) {
			t.Errorf("set-3 %02X: twinax % X, want % X", code, got, want)
		}
	}
}

// TestFirmwareKeysReachHost: every 5250 function and character typed on
// the emulated keyboard arrives at the host as that function/character.
func TestFirmwareKeysReachHost(t *testing.T) {
	r := newFirmwareRig(t)
	for _, f := range kbdlayout.Functions {
		if f.Name == "Setup" {
			continue // opens the terminal's own Setup
		}
		r.h.keys, r.h.typed = nil, nil
		r.tap(f.Code, f.Shift)
		want := f.Name
		if len(r.h.keys) != 1 || r.h.keys[0] != want || len(r.h.typed) != 0 {
			t.Errorf("%s: host got keys %v typed %q", f.Name, r.h.keys, string(r.h.typed))
		}
	}
	for code, ch := range kbdlayout.Chars {
		for i, shift := range []bool{false, true} {
			r.h.keys, r.h.typed = nil, nil
			r.tap(code, shift)
			if string(r.h.typed) != string(ch[i]) || len(r.h.keys) != 0 {
				t.Errorf("key %02X shift=%v: typed %q keys %v, want %q",
					code, shift, string(r.h.typed), r.h.keys, string(ch[i]))
			}
		}
	}
}

// TestFirmwareNonDisplayField: the firmware stores a 5250 non-display
// field (attribute 0x27, password) as cell attribute 00 with bit 7 of the
// third cell byte set, and characters later written into the field keep
// it. The browser renderer and the copy text rely on that bit.
func TestFirmwareNonDisplayField(t *testing.T) {
	r := newFirmwareRig(t)
	d := r.h.scr.Data
	row := 5 * 80
	d[row+10], d[row+21] = 0x27, 0x20 // non-display field, cols 11-20
	r.step(300)
	d[row+11], d[row+12] = 0x91, 0x86 // "jf" typed into it
	r.step(300)
	_, cols, _, cells := r.b.Video.Screen()
	for c := 11; c <= 12; c++ {
		i := (5*cols + c) * 3
		if cells[i+2]&0x80 == 0 {
			t.Errorf("col %d: cell % X, want third byte bit 7 (non-display)", c, cells[i:i+3])
		}
	}
	if i := (5*cols + 22) * 3; cells[i+2]&0x80 != 0 {
		t.Errorf("col 22 after the field is non-display: % X", cells[i:i+3])
	}
}
