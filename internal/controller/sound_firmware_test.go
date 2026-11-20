package controller

import "testing"

// TestFirmwareKeyClick: each key press makes the firmware click the
// speaker (register 0x20011), once.
func TestFirmwareKeyClick(t *testing.T) {
	r := newFirmwareRig(t)
	before := r.b.IO.Sound().Clicks
	r.tap(0x1C, false) // a
	r.tap(0x32, false) // b
	if got := r.b.IO.Sound().Clicks - before; got != 2 {
		t.Fatalf("%d clicks for 2 keys, want 2", got)
	}
}

// TestFirmwareHostAlarm: a host alarm reaches the terminal as Write
// Control Data with the alarm bit; the firmware sounds the tone for 50
// ticks of its 60 Hz timer (~0.83 s) and then stops it.
func TestFirmwareHostAlarm(t *testing.T) {
	r := newFirmwareRig(t)
	if r.b.IO.Sound().ToneOn {
		t.Fatal("tone on before the alarm")
	}
	r.h.scr.Alarms++
	on, off := -1, -1
	for ms := 0; ms < 1500; ms++ {
		r.step(1)
		tone := r.b.IO.Sound().ToneOn
		if tone && on < 0 {
			on = ms
		}
		if !tone && on >= 0 && off < 0 {
			off = ms
		}
	}
	if on < 0 {
		t.Fatal("the alarm never sounded")
	}
	if d := off - on; off < 0 || d < 750 || d > 900 {
		t.Fatalf("tone lasted %d ms (on at %d, off at %d), want ~833", d, on, off)
	}
	if v := r.b.IO.Sound().Volume; v == 0 {
		t.Error("alarm volume 0")
	}
}
