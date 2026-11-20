package controller

import "testing"

// fillScreen gives the host a rows x cols screen with text at its
// corners and some rows in between.
func fillScreen(h *fakeHost, rows, cols int) {
	d := make([]byte, rows*cols)
	for i := range d {
		d[i] = 0x40
	}
	put := func(r, c int, s string) {
		for i, ch := range s {
			d[r*cols+c+i] = asciiToEBCDIC(ch)
		}
	}
	put(0, 0, "TOPLEFT")
	put(0, cols-8, "TOPRIGHT")
	for r := 1; r < rows-1; r += 3 {
		put(r, r%(cols-10), "ROW")
	}
	put(rows-1, 0, "BOTTOMLEFT")
	put(rows-1, cols-11, "BOTTOMRIGHT")
	h.scr.Rows, h.scr.Cols, h.scr.Data = rows, cols, d
	h.scr.CursorRow, h.scr.CursorCol, h.scr.ErrorRow = 1, 1, rows-1
}

func checkStation(t *testing.T, r *firmwareRig, rows, cols int) {
	t.Helper()
	srows, scols, _, _ := r.b.Video.Screen()
	if srows != rows+1 || scols != cols {
		t.Fatalf("station screen %dx%d (+ status row), want %dx%d", srows-1, scols, rows, cols)
	}
	if st, ok := r.b.Twinax.Deliver(r.c.Address(), cmdPoll, nil); !ok || st.Status&stException != 0 {
		t.Fatalf("station status %02X (ok %v)", st.Status, ok)
	}
	if bad := stationMismatches(r); len(bad) > 0 {
		_, _, _, cells := r.b.Video.Screen()
		for _, p := range bad[:min(len(bad), 10)] {
			row, col := p/cols, p%cols
			t.Errorf("row %d col %d: station %02X, host %02X", row, col,
				cells[(row*cols+col)*3], r.h.scr.Data[p])
		}
		t.Fatalf("%d of %d cells differ", len(bad), rows*cols)
	}
}

// TestFirmware27x132: a 27x132 host screen is shown by the real 3477
// firmware: the controller switches the station to 27x132 with Write
// Control Data [44 08] (before, it sent 132-column addresses to a
// station in 24x80 mode, which raised exception 6 and then dropped every
// write). Switching back to 24x80 works too.
func TestFirmware27x132(t *testing.T) {
	r := newFirmwareRig(t)
	fillScreen(r.h, 27, 132)
	r.step(3000)
	checkStation(t, r, 27, 132)
	fillScreen(r.h, 24, 80)
	r.step(3000)
	checkStation(t, r, 24, 80)
}

// TestFirmwareExceptionRecovered: an exception raised by the real
// firmware (a write beyond the 24x80 screen) is reset by the controller
// and later host changes reach the screen again.
func TestFirmwareExceptionRecovered(t *testing.T) {
	r := newFirmwareRig(t)
	fillScreen(r.h, 24, 80)
	r.step(1500)
	checkStation(t, r, 24, 80)
	a := r.c.Address()
	r.b.Twinax.Deliver(a, cmdLAC, addr2(27*132-10))
	r.b.Twinax.Deliver(a, cmdWDLC, []uint8{3, 0xC1, 0xC2, 0xC3})
	r.b.Twinax.Deliver(a, cmdEOQ, nil)
	exc := uint8(0)
	for i := 0; i < 50 && exc == 0; i++ {
		r.b.Run(59818500 / 8 / 1000)
		st, _ := r.b.Twinax.Deliver(a, cmdPoll, nil)
		exc = st.Status & stException
	}
	if exc == 0 {
		t.Skip("the firmware raised no exception for the bad address")
	}
	t.Logf("exception status %02X", exc)
	put := "CHANGED"
	for i, ch := range put {
		r.h.scr.Data[5*80+10+i] = asciiToEBCDIC(ch)
	}
	r.step(1500)
	checkStation(t, r, 24, 80)
}
