package emu

import (
	"reflect"
	"testing"
)

// TestKeyGroups: characters become set-3 key presses and releases, with
// Shift for upper case, New Line for line breaks; untypeable characters
// are skipped.
func TestKeyGroups(t *testing.T) {
	got := keyGroups("aB\n€")
	want := [][]uint8{
		{0x1C, 0xF0, 0x1C},                   // a
		{0x12, 0x32, 0xF0, 0x32, 0xF0, 0x12}, // Shift + b
		{0x12, 0x5A, 0xF0, 0x5A, 0xF0, 0x12}, // New Line = Shift + Field Exit
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyGroups = % X\nwant       % X", got, want)
	}
	// digits come from the main row, not the numeric pad
	if g := keyGroups("1"); len(g) != 1 || g[0][0] != 0x16 {
		t.Fatalf("'1' typed as % X, want main-row key 16", g)
	}
}

// TestScreenTextHidesPasswords: a cell in a non-display field (attribute
// 0x27) is copied as a blank, ordinary text as itself.
func TestScreenTextHidesPasswords(t *testing.T) {
	// two cells: 'A' (C1) normal 0x20, 'B' (C2) non-display 0x27
	cells := []byte{0xC1, 0x20, 0x00, 0xC2, 0x27, 0x00}
	if got := screenText(cells, 2, "37"); len(got) != 1 || got[0] != "A " {
		t.Fatalf("screenText = %q, want [\"A \"]", got)
	}
	// host password field: the firmware stores 5250 attribute 0x27 as
	// cell attribute 00 with bit 7 of the third byte set
	cells = []byte{0xC1, 0x00, 0x00, 0x91, 0x00, 0x80, 0x86, 0x00, 0x80}
	if got := screenText(cells, 3, "37"); len(got) != 1 || got[0] != "A  " {
		t.Fatalf("screenText = %q, want [\"A  \"]", got)
	}
}

// TestPasteStepsDirectChars: characters the US keyboard has no key for
// become direct characters, in order; control characters are dropped.
func TestPasteStepsDirectChars(t *testing.T) {
	st := pasteSteps("aü\x07ß")
	if len(st) != 3 || st[0].keys == nil || st[1].char != 'ü' || st[2].char != 'ß' {
		t.Fatalf("pasteSteps = %+v", st)
	}
}
