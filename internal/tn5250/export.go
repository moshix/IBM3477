package tn5250

// EBCDICToRune converts one EBCDIC byte to a character in the given
// codepage ("37", "273", ...), for tools that show terminal memory.
func EBCDICToRune(codepage string, b byte) rune {
	return newCharMap(codepage).toLocalRune(b)
}
