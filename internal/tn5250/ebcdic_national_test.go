// Copyright 2026 by moshix. All rights reserved.

package tn5250

import "testing"

// These tests cover the national single-byte EBCDIC code pages added on top of
// the original CP037/CP500 support. The reference values are taken directly from
// lib5250's transmaps.h (ibmNNN_to_iso_8859_1 tables), so they double as a guard
// against a mis-transcribed table.

// roundTrip asserts the full 256-value local -> remote -> local identity on the
// raw translation tables for the named Latin-1 code page. (The two EBCDIC
// display overrides in local(), 0x00 and 0x1C, are intentionally lossy and are
// verified separately in TestLocalSpecialCases; they are not part of the table
// bijection tested here.)
func roundTrip(t *testing.T, name string) {
	t.Helper()
	cmap := newCharMap(name)
	if cmap.name != name {
		t.Fatalf("newCharMap(%q) returned map named %q", name, cmap.name)
	}
	for i := 0; i < 256; i++ {
		b := byte(i)
		ebcdic := cmap.toRemote[b]
		back := cmap.toLocal[ebcdic]
		if back != b {
			t.Errorf("CP%s round-trip broken: local 0x%02X -> remote 0x%02X -> local 0x%02X",
				name, b, ebcdic, back)
		}
	}
}

func TestNationalRoundTrip(t *testing.T) {
	for _, name := range []string{"273", "285", "297", "277", "278", "280", "284", "871"} {
		roundTrip(t, name)
	}
}

// TestNationalSpotChecks verifies a few national glyph code points against the
// values in transmaps.h, and confirms they actually differ from plain CP037 (so
// the map is doing real work, not silently falling back to the default).
func TestNationalSpotChecks(t *testing.T) {
	cp037 := newCharMap("37")

	// EBCDIC byte -> expected local Unicode rune, per transmaps.h.
	cases := []struct {
		page   string
		ebcdic byte
		want   rune
	}{
		// CP273 (German): Ä at 0x4A, § at 0x7C, Ü at 0x5A, [ at 0x63.
		{"273", 0x4A, 'Ä'},
		{"273", 0x7C, '§'},
		{"273", 0x5A, 'Ü'},
		// CP285 (UK): $ at 0x4A, £ at 0x5B, [ at 0xB1.
		{"285", 0x4A, '$'},
		{"285", 0x5B, '£'},
		{"285", 0xB1, '['},
		// CP297 (French): ° at 0x4A, à at 0x7C, { at 0x51, £ at 0x7B.
		{"297", 0x4A, '°'},
		{"297", 0x7C, 'à'},
		{"297", 0x51, '{'},
		{"297", 0x7B, '£'},
	}

	for _, c := range cases {
		cmap := newCharMap(c.page)
		if got := cmap.toLocalRune(c.ebcdic); got != c.want {
			t.Errorf("CP%s: EBCDIC 0x%02X -> %q (U+%04X), want %q (U+%04X)",
				c.page, c.ebcdic, got, got, c.want, c.want)
		}
		// The whole point of these code pages is that the national glyphs live
		// where CP037 has something else.
		if got := cmap.toLocalRune(c.ebcdic); got == cp037.toLocalRune(c.ebcdic) {
			t.Errorf("CP%s: EBCDIC 0x%02X matches CP037 (%q); expected a national difference",
				c.page, c.ebcdic, got)
		}
	}
}

// TestCP870Latin2Bridge checks the CP870 special case: its local alphabet is
// ISO-8859-2, so toLocalRune must reach real Central-European Unicode code
// points (above U+00FF) rather than mis-reading the Latin-2 byte as Latin-1.
func TestCP870Latin2Bridge(t *testing.T) {
	cmap := newCharMap("870")
	if cmap.localToUnicode == nil || cmap.unicodeToLocal == nil {
		t.Fatal("CP870 map is missing its Latin-2 <-> Unicode bridge")
	}

	// Values verified against transmaps.h (ibm870_to_iso_8859_2 + the utility.c
	// "870" workaround) and Python's iso-8859-2 codec.
	cases := []struct {
		ebcdic byte
		want   rune
	}{
		{0x47, 'č'}, // U+010D
		{0x52, 'ę'}, // U+0119
		{0x44, 'Ă'}, // U+0102
	}
	for _, c := range cases {
		if got := cmap.toLocalRune(c.ebcdic); got != c.want {
			t.Errorf("CP870: EBCDIC 0x%02X -> %q (U+%04X), want %q (U+%04X)",
				c.ebcdic, got, got, c.want, c.want)
		}
		if c.want <= 0xFF {
			t.Fatalf("test bug: %q is not a Latin-2-specific code point", c.want)
		}
	}

	// The bridge must be internally consistent: rune -> local -> unicode returns
	// the same rune for every ISO-8859-2 byte.
	for b := 0; b < 256; b++ {
		r := iso88592ToUnicode[b]
		lb, ok := cmap.unicodeToLocal[r]
		if !ok || iso88592ToUnicode[lb] != r {
			t.Errorf("CP870 bridge not a bijection at local byte %d (rune U+%04X)", b, r)
		}
	}
}

// TestLocalSpecialCases guards the two EBCDIC display overrides shared by every
// map, so the round-trip helper's exclusion of them is justified.
func TestLocalSpecialCases(t *testing.T) {
	cmap := newCharMap("273")
	if got := cmap.local(0x00); got != ' ' {
		t.Errorf("EBCDIC 0x00 should display as blank, got %q", got)
	}
	if got := cmap.local(0x1C); got != '*' {
		t.Errorf("EBCDIC 0x1C (DUP) should display as '*', got %q", got)
	}
}
