package tn5250

// EBCDICToRune converts one EBCDIC byte to a character in the given
// codepage ("37", "273", ...), for tools that show terminal memory.
func EBCDICToRune(codepage string, b byte) rune {
	return newCharMap(codepage).toLocalRune(b)
}

// EBCDICTable returns the decoding of every EBCDIC byte in the given
// codepage, as EBCDICToRune would return it. Building it once lets a
// caller decode whole screens without a codepage lookup per character.
func EBCDICTable(codepage string) *[256]rune {
	m := newCharMap(codepage)
	t := new([256]rune)
	for i := range t {
		t[i] = m.toLocalRune(byte(i))
	}
	return t
}
