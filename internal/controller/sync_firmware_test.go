package controller

import (
	"math/rand"
	"testing"
)

// stationMismatches compares the station's screen with the host's
// presentation space: data bytes must appear as the cell character;
// attribute bytes and nulls show as blanks.
func stationMismatches(r *firmwareRig) []int {
	_, cols, _, cells := r.b.Video.Screen()
	d := r.h.scr.Data
	var bad []int
	for p, v := range d {
		row, col := p/80, p%80
		ch := cells[(row*cols+col)*3]
		want := v
		if v == 0 || (v >= 0x20 && v <= 0x3F) {
			if ch == 0x40 || ch == 0x00 {
				continue
			}
			want = 0x40
		}
		if ch != want {
			bad = append(bad, p)
		}
	}
	return bad
}

// TestFirmwareFastTypingKeepsScreen: a menu screen, then fast typing into
// the command line (one character every 1-6 ms, cursor following) while
// the indicators flip. Afterwards the station must show exactly the
// host's screen: no character may land in the wrong place.
func TestFirmwareFastTypingKeepsScreen(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		fastTyping(t, seed)
	}
}

func fastTyping(t *testing.T, seed int64) {
	r := newFirmwareRig(t)
	d := r.h.scr.Data
	text := "     3. General system tasks"
	for row := 2; row < 18; row++ {
		d[row*80] = 0x20
		for i, ch := range text {
			d[row*80+1+i] = asciiToEBCDIC(ch)
		}
	}
	in := 19*80 + 6
	d[in-1], d[in+60] = 0x24, 0x20 // underlined input field, cols 6-65
	r.step(500)
	rng := rand.New(rand.NewSource(seed))
	for round := 0; round < 4; round++ {
		for i := 0; i < 60; i++ {
			d[in+i] = asciiToEBCDIC(rune('a' + rng.Intn(26)))
			r.h.scr.CursorRow, r.h.scr.CursorCol = 19, 6+i+1
			r.h.scr.Insert = i%7 == 0
			if rounds := round >= 2; rounds && i%9 == 0 {
				// the host rewrites a menu line meanwhile
				row := 2 + rng.Intn(16)
				for c := 1; c < 29; c++ {
					d[row*80+c] = asciiToEBCDIC(rune('a' + rng.Intn(26)))
				}
			}
			r.step(1 + rng.Intn(6))
		}
		r.step(300)
		if bad := stationMismatches(r); len(bad) > 0 {
			_, cols, _, cells := r.b.Video.Screen()
			p := bad[0]
			t.Logf("queues pending %d; at %d station %02X host %02X; shadow %02X",
				len(r.c.queues), p, cells[(p/80*cols+p%80)*3], d[p], r.c.shadow[p])
			r.step(3000)
			t.Fatalf("seed %d round %d: %d cells differ (after 3 s more: %d)", seed, round, len(bad), len(stationMismatches(r)))
		}
		crow, ccol, _ := r.b.Video.Cursor()
		if crow != r.h.scr.CursorRow || ccol != r.h.scr.CursorCol {
			t.Fatalf("seed %d round %d: terminal cursor %d,%d, host %d,%d", seed, round, crow, ccol,
				r.h.scr.CursorRow, r.h.scr.CursorCol)
		}
	}
	r.step(500)
	if bad := stationMismatches(r); len(bad) > 0 {
		_, cols, _, cells := r.b.Video.Screen()
		for _, p := range bad[:min(len(bad), 10)] {
			row, col := p/80, p%80
			t.Errorf("row %d col %d: station %02X, host %02X", row, col, cells[(row*cols+col)*3], d[p])
		}
		t.Fatalf("%d cells differ", len(bad))
	}
}

// asciiToEBCDIC covers letters, digits, blank and '.' (CP 37).
func asciiToEBCDIC(r rune) uint8 {
	switch {
	case r >= 'a' && r <= 'i':
		return 0x81 + uint8(r-'a')
	case r >= 'j' && r <= 'r':
		return 0x91 + uint8(r-'j')
	case r >= 's' && r <= 'z':
		return 0xA2 + uint8(r-'s')
	case r >= 'A' && r <= 'I':
		return 0xC1 + uint8(r-'A')
	case r >= '0' && r <= '9':
		return 0xF0 + uint8(r-'0')
	case r == '.':
		return 0x4B
	}
	return 0x40
}
