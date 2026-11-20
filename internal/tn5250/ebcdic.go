// Copyright 2026 by moshix. All rights reserved.

package tn5250

// EBCDIC translation, ported from the character-map machinery in lib5250's
// utility.c and the generated tables in transmaps.h. A charMap holds two 256
// byte lookups: toRemote (local -> EBCDIC) and toLocal (EBCDIC -> local). The
// "local" alphabet is ISO-8859-1 (Latin-1), so a local byte maps one-to-one to
// a Unicode code point in U+0000..U+00FF — this is what makes the maps
// round-trippable and lets us bridge EBCDIC to UTF-8 for the browser.
//
// Supported code pages: "37" (CP037, US/Canada — the default), "500" (CP500,
// International), "37-bracket"/"bracket" (a CP037 variant that swaps the
// square-bracket code points), and the national single-byte EBCDIC pages
// "273" (German), "277" (Danish/Norwegian), "278" (Swedish/Finnish),
// "280" (Italian), "284" (Spanish), "285" (UK), "297" (French),
// "871" (Icelandic) and "870" (Latin-2 / Central Europe). All but 870 are
// Latin-1 based; 870 is Latin-2 based and carries a Unicode bridge (see below).

// charMap is the Go analogue of lib5250's Tn5250CharMap.
//
// For nearly every supported code page the "local" alphabet is ISO-8859-1
// (Latin-1), so a local byte equals its Unicode code point and localToUnicode /
// unicodeToLocal stay nil (the identity path). CP870 is the exception: its
// local alphabet is ISO-8859-2 (Latin-2), so it supplies the two bridge tables
// to translate the Latin-2 local bytes to/from real Unicode for the browser.
type charMap struct {
	name     string
	toRemote [256]byte // local -> EBCDIC
	toLocal  [256]byte // EBCDIC -> local

	// localToUnicode maps a local byte to its Unicode rune when the local
	// alphabet is not Latin-1 (nil = identity, i.e. rune(localByte)).
	localToUnicode *[256]rune
	// unicodeToLocal is the inverse used on the keyboard path (nil = identity).
	unicodeToLocal map[rune]byte
}

// newCharMap creates a translation map for the named code page. Unknown or
// empty names fall back to CP037, mirroring the display's default of "37".
func newCharMap(name string) *charMap {
	switch name {
	case "", "37", "cp037", "037":
		return &charMap{name: "37", toRemote: cp037ToRemote, toLocal: cp037ToLocal}
	case "500", "cp500":
		return &charMap{name: "500", toRemote: cp500ToRemote, toLocal: cp500ToLocal}
	case "37-bracket", "bracket", "37b":
		return newBracketMap()
	case "273", "cp273":
		return &charMap{name: "273", toRemote: cp273ToRemote, toLocal: cp273ToLocal}
	case "277", "cp277":
		return &charMap{name: "277", toRemote: cp277ToRemote, toLocal: cp277ToLocal}
	case "278", "cp278":
		return &charMap{name: "278", toRemote: cp278ToRemote, toLocal: cp278ToLocal}
	case "280", "cp280":
		return &charMap{name: "280", toRemote: cp280ToRemote, toLocal: cp280ToLocal}
	case "284", "cp284":
		return &charMap{name: "284", toRemote: cp284ToRemote, toLocal: cp284ToLocal}
	case "285", "cp285":
		return &charMap{name: "285", toRemote: cp285ToRemote, toLocal: cp285ToLocal}
	case "297", "cp297":
		return &charMap{name: "297", toRemote: cp297ToRemote, toLocal: cp297ToLocal}
	case "871", "cp871":
		return &charMap{name: "871", toRemote: cp871ToRemote, toLocal: cp871ToLocal}
	case "870", "cp870":
		// CP870's local alphabet is ISO-8859-2 (Latin-2), so it needs the
		// Latin-2 <-> Unicode bridge to reach the browser correctly.
		return newLatin2Map("870", cp870ToRemote, cp870ToLocal)
	default:
		// Faithful to the C: an unknown map name would return NULL there, but
		// for a browser terminal a safe default is friendlier than a failure.
		return &charMap{name: "37", toRemote: cp037ToRemote, toLocal: cp037ToLocal}
	}
}

// newBracketMap builds the CP037 "bracket" variant. Hosts using the bracket
// code page place '[' at EBCDIC 0x4A and ']' at 0x5A (where plain CP037 keeps
// cent-sign / exclamation). We start from CP037 and swap the two pairs: '[' /
// ']' move to 0x4A / 0x5A, and '¢' / '!' move to the code points plain CP037
// uses for the brackets (0xBA / 0xBB). A pure swap keeps the map a bijection,
// so every byte still round-trips and no two characters share a code point.
func newBracketMap() *charMap {
	m := &charMap{name: "37-bracket", toRemote: cp037ToRemote, toLocal: cp037ToLocal}
	swap := func(a, b byte) { // exchange the characters at EBCDIC a and b
		la, lb := m.toLocal[a], m.toLocal[b]
		m.toLocal[a], m.toLocal[b] = lb, la
		m.toRemote[la], m.toRemote[lb] = b, a
	}
	swap(0x4A, 0xBA) // '¢' <-> '['
	swap(0x5A, 0xBB) // '!' <-> ']'
	return m
}

// newLatin2Map builds a char map whose local alphabet is ISO-8859-2 (Latin-2)
// rather than Latin-1. The EBCDIC<->local byte tables come straight from
// transmaps.h, and the localToUnicode/unicodeToLocal bridge converts those
// Latin-2 local bytes to and from real Unicode so the browser shows the correct
// Central-European glyphs. ISO-8859-2 is a bijection over 0..255, so inverting
// iso88592ToUnicode yields a complete reverse map.
func newLatin2Map(name string, toRemote, toLocal [256]byte) *charMap {
	rev := make(map[rune]byte, 256)
	for b := 0; b < 256; b++ {
		rev[iso88592ToUnicode[b]] = byte(b)
	}
	return &charMap{
		name:           name,
		toRemote:       toRemote,
		toLocal:        toLocal,
		localToUnicode: &iso88592ToUnicode,
		unicodeToLocal: rev,
	}
}

// remote translates a local byte to its EBCDIC counterpart.
func (m *charMap) remote(local byte) byte { return m.toRemote[local] }

// local translates an EBCDIC byte to its local Latin-1 byte. Two values are
// special-cased exactly as tn5250_char_map_to_local does: the DUP order (0x1C)
// prints as '*' and NUL (0x00) prints as a blank.
func (m *charMap) local(ebcdic byte) byte {
	switch ebcdic {
	case 0x1C:
		return '*' // DUP fill; shown as an (over-struck) asterisk
	case 0x00:
		return ' '
	default:
		return m.toLocal[ebcdic]
	}
}

// toRemoteRune converts a Unicode rune to EBCDIC. For Latin-1 maps the rune's
// code point is the local byte directly; for a bridged map (CP870/Latin-2) it is
// looked up in unicodeToLocal. Runes with no single-byte representation in the
// map become a blank (0x40 via the ' ' entry).
func (m *charMap) toRemoteRune(r rune) byte {
	// The browser fills a DUP field with the U+001C sentinel; it always
	// transmits as the 5250 DUP byte (EBCDIC 0x1C), independent of code page.
	if r == 0x1C {
		return 0x1C
	}
	if m.unicodeToLocal != nil {
		if lb, ok := m.unicodeToLocal[r]; ok {
			return m.toRemote[lb]
		}
		return m.toRemote[' ']
	}
	if r >= 0 && r <= 0xFF {
		return m.toRemote[byte(r)]
	}
	return m.toRemote[' ']
}

// toLocalRune converts an EBCDIC byte to a Unicode rune. For Latin-1 maps the
// local byte is itself the code point; for a bridged map (CP870/Latin-2) the
// local byte is run through localToUnicode to reach the real Unicode rune.
func (m *charMap) toLocalRune(ebcdic byte) rune {
	lb := m.local(ebcdic)
	if m.localToUnicode != nil {
		return m.localToUnicode[lb]
	}
	return rune(lb)
}

// isAttribute reports whether an EBCDIC byte is a 5250 field-attribute byte
// (top three bits 001), matching tn5250_char_map_attribute_p.
func isAttribute(b byte) bool { return (b & 0xE0) == 0x20 }
