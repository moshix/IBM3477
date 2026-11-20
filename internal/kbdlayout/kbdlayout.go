// Package kbdlayout is the single description of the terminal keyboard
// used on both ends of the emulation: the emulated keyboard (browser key
// -> scan code, internal/emu) and the workstation controller (twinax key
// code from the terminal -> 5250 key action, internal/controller).
//
// The keyboard is the IBM 122-key keyboard of the 3477 (3180-2/3196
// layout). It sends IBM scan code set 3 to the terminal. The terminal
// firmware does NOT pass these codes on: it translates each key into the
// 5250 twinax keyboard code of that key (ToTwinax, recorded from the 3477
// firmware), which is what a workstation controller receives in the poll
// responses and decodes (IBM "5250 Information Display System to S/36,
// S/38 and AS/400 Product Attachment Information", 1989, figures 103 and
// 105). F1-F12 arrive as Cmd (6F) + 31..3C, F13-F24 as Cmd + Shift + the
// same codes; Shift is 57/56 make, D7/D6 break; Caps Lock 54/D4.
package kbdlayout

// Function is a 5250 keyboard function carried by a key (optionally
// with Shift held).
type Function struct {
	Name  string
	Code  uint8 // set-3 make code
	Shift bool  // the function is Shift + key
}

// Functions lists the 5250 functions and the 122-key keyboard key (set-3
// make code) that carries each. The left key block (set-3 05/06, 04/0C,
// 03/0B, 83/0A, 01/09) holds Attn, Clear, Help, Print, Roll Up and Setup.
// Each function must reach the controller as a distinct twinax code
// (Shift + key counts as distinct): see TestFunctionsDistinctOnTwinax.
var Functions = []Function{
	{"PF1", 0x07, false}, {"PF2", 0x0F, false}, {"PF3", 0x17, false},
	{"PF4", 0x1F, false}, {"PF5", 0x27, false}, {"PF6", 0x2F, false},
	{"PF7", 0x37, false}, {"PF8", 0x3F, false}, {"PF9", 0x47, false},
	{"PF10", 0x4F, false}, {"PF11", 0x56, false}, {"PF12", 0x5E, false},
	// F13-F24: the top key row of the 122-key keyboard
	{"PF13", 0x08, false}, {"PF14", 0x10, false}, {"PF15", 0x18, false},
	{"PF16", 0x20, false}, {"PF17", 0x28, false}, {"PF18", 0x30, false},
	{"PF19", 0x38, false}, {"PF20", 0x40, false}, {"PF21", 0x48, false},
	{"PF22", 0x50, false}, {"PF23", 0x57, false}, {"PF24", 0x5F, false},
	{"Attn", 0x05, false},       // left block, top left (twinax 7C)
	{"SysReq", 0x05, true},      // Shift + Attn
	{"Clear", 0x06, false},      // left block, top right (75)
	{"TestReq", 0x06, true},     // Shift + Clear
	{"Help", 0x04, false},       // left block (66)
	{"Print", 0x0C, false},      // left block (67)
	{"RollUp", 0x0B, false},     // left block (7D)
	{"RollDown", 0x6F, false},   // edit block, Page Up position (62)
	{"Reset", 0x11, false},      // left Ctrl position (7E)
	{"Enter", 0x58, false},      // right Ctrl position (68)
	{"FieldExit", 0x5A, false},  // large Enter key (2D)
	{"NewLine", 0x5A, true},     // Shift + Field Exit
	{"Tab", 0x0D, false},        // (20)
	{"BackTab", 0x0D, true},     // Shift + Tab
	{"Left", 0x61, false},       // (72)
	{"Right", 0x6A, false},      // (73)
	{"Up", 0x63, false},         // (71)
	{"Down", 0x60, false},       // (70)
	{"Backspace", 0x66, false},  // cursor left (3D)
	{"Home", 0x6E, false},       // (4C)
	{"Insert", 0x67, false},     // (4B)
	{"Dup", 0x67, true},         // Shift + Insert
	{"Delete", 0x64, false},     // (0C)
	{"EraseEOF", 0x65, false},   // (6C)
	{"ErInp", 0x6D, false},      // its own key; sends Shift + Erase EOF (57 6C)
	{"FieldPlus", 0x7C, false},  // keypad (4E)
	{"FieldMinus", 0x84, false}, // keypad (50)
	{"Setup", 0x01, false},      // terminal Setup (3477 run time: key 01 opens Online Setup)
}

// ToTwinax is the 3477 firmware's translation of set-3 make codes into
// the twinax key codes it sends to the controller (one key may send a
// sequence). Recorded by pressing every key on the emulated 3477
// (TestFirmwareKeyTranslation checks it); it agrees with IBM's 122-key
// scan code table. Keys absent here send nothing (Alt, Setup, the left
// block keys 03/09/0A, 62) or are handled inside the terminal.
var ToTwinax = map[uint8][]uint8{
	0x04: {0x66}, 0x05: {0x7C}, 0x06: {0x75}, 0x07: {0x6F, 0x31},
	0x08: {0x6F, 0x57, 0x31}, 0x0B: {0x7D}, 0x0C: {0x67}, 0x0D: {0x20},
	0x0E: {0x3E}, 0x0F: {0x6F, 0x32}, 0x10: {0x6F, 0x57, 0x32}, 0x11: {0x7E},
	0x12: {0x57}, 0x13: {0x0E}, 0x14: {0x54}, 0x15: {0x21}, 0x16: {0x31},
	0x17: {0x6F, 0x33}, 0x18: {0x6F, 0x57, 0x33}, 0x1A: {0x01}, 0x1B: {0x12},
	0x1C: {0x11}, 0x1D: {0x22}, 0x1E: {0x32}, 0x1F: {0x6F, 0x34},
	0x20: {0x6F, 0x57, 0x34}, 0x21: {0x03}, 0x22: {0x02}, 0x23: {0x13},
	0x24: {0x23}, 0x25: {0x34}, 0x26: {0x33}, 0x27: {0x6F, 0x35},
	0x28: {0x6F, 0x57, 0x35}, 0x29: {0x0F}, 0x2A: {0x04}, 0x2B: {0x14},
	0x2C: {0x25}, 0x2D: {0x24}, 0x2E: {0x35}, 0x2F: {0x6F, 0x36},
	0x30: {0x6F, 0x57, 0x36}, 0x31: {0x06}, 0x32: {0x05}, 0x33: {0x16},
	0x34: {0x15}, 0x35: {0x26}, 0x36: {0x36}, 0x37: {0x6F, 0x37},
	0x38: {0x6F, 0x57, 0x37}, 0x3A: {0x07}, 0x3B: {0x17}, 0x3C: {0x27},
	0x3D: {0x37}, 0x3E: {0x38}, 0x3F: {0x6F, 0x38}, 0x40: {0x6F, 0x57, 0x38},
	0x41: {0x08}, 0x42: {0x18}, 0x43: {0x28}, 0x44: {0x29}, 0x45: {0x3A},
	0x46: {0x39}, 0x47: {0x6F, 0x39}, 0x48: {0x6F, 0x57, 0x39}, 0x49: {0x09},
	0x4A: {0x0A}, 0x4B: {0x19}, 0x4C: {0x1A}, 0x4D: {0x2A}, 0x4E: {0x3B},
	0x4F: {0x6F, 0x3A}, 0x50: {0x6F, 0x57, 0x3A}, 0x51: {0x0C}, 0x52: {0x1B},
	0x53: {0x1C}, 0x54: {0x2B}, 0x55: {0x3C}, 0x56: {0x6F, 0x3B},
	0x57: {0x6F, 0x57, 0x3B}, 0x58: {0x68}, 0x59: {0x56}, 0x5A: {0x2D},
	0x5B: {0x2C}, 0x5C: {0x2D}, 0x5D: {0x61}, 0x5E: {0x6F, 0x3C},
	0x5F: {0x6F, 0x57, 0x3C}, 0x60: {0x70}, 0x61: {0x72}, 0x63: {0x71},
	0x64: {0x0C}, 0x65: {0x6C}, 0x66: {0x3D}, 0x67: {0x4B}, 0x69: {0x41},
	0x6A: {0x73}, 0x6B: {0x44}, 0x6C: {0x47}, 0x6D: {0x57, 0x6C},
	0x6E: {0x4C}, 0x6F: {0x62}, 0x70: {0x40}, 0x71: {0x4A}, 0x72: {0x42},
	0x73: {0x45}, 0x74: {0x46}, 0x75: {0x48}, 0x76: {0x1D}, 0x77: {0x1E},
	0x79: {0x4D}, 0x7A: {0x43}, 0x7B: {0x55}, 0x7C: {0x4E}, 0x7D: {0x49},
	0x7E: {0x4F}, 0x7F: {0x50}, 0x83: {0x78}, 0x84: {0x50},
}

// Twinax key codes with a meaning of their own on the line.
const (
	TwinaxCmd        = 0x6F // prefix: Cmd + 31..3C = PF1-PF12
	TwinaxShiftLeft  = 0x57
	TwinaxShiftRight = 0x56
	TwinaxCapsLock   = 0x54
	TwinaxRelease    = 0x80 // a break is the make code with bit 7 set
	TwinaxPFBase     = 0x30 // Cmd + (0x30+n) = PFn, n = 1..12
)

// Modifier make codes (set 3).
const (
	ShiftLeft  = 0x12
	ShiftRight = 0x59
)

// Chars maps set-3 make codes of character keys to their unshifted and
// shifted characters (US layout).
var Chars = map[uint8][2]rune{
	0x0E: {'`', '~'}, 0x16: {'1', '!'}, 0x1E: {'2', '@'}, 0x26: {'3', '#'},
	0x25: {'4', '$'}, 0x2E: {'5', '%'}, 0x36: {'6', '^'}, 0x3D: {'7', '&'},
	0x3E: {'8', '*'}, 0x46: {'9', '('}, 0x45: {'0', ')'}, 0x4E: {'-', '_'},
	0x55: {'=', '+'},
	0x15: {'q', 'Q'}, 0x1D: {'w', 'W'}, 0x24: {'e', 'E'}, 0x2D: {'r', 'R'},
	0x2C: {'t', 'T'}, 0x35: {'y', 'Y'}, 0x3C: {'u', 'U'}, 0x43: {'i', 'I'},
	0x44: {'o', 'O'}, 0x4D: {'p', 'P'}, 0x54: {'[', '{'}, 0x5B: {']', '}'},
	0x53: {'\\', '|'}, // the key left of the large Enter
	0x1C: {'a', 'A'}, 0x1B: {'s', 'S'}, 0x23: {'d', 'D'}, 0x2B: {'f', 'F'},
	0x34: {'g', 'G'}, 0x33: {'h', 'H'}, 0x3B: {'j', 'J'}, 0x42: {'k', 'K'},
	0x4B: {'l', 'L'}, 0x4C: {';', ':'}, 0x52: {'\'', '"'},
	0x13: {'<', '>'}, // the key left of Z
	0x1A: {'z', 'Z'}, 0x22: {'x', 'X'}, 0x21: {'c', 'C'}, 0x2A: {'v', 'V'},
	0x32: {'b', 'B'}, 0x31: {'n', 'N'}, 0x3A: {'m', 'M'}, 0x41: {',', '<'},
	0x49: {'.', '>'}, 0x4A: {'/', '?'},
	0x29: {' ', ' '},
	// numeric pad
	0x77: {'/', '/'}, 0x7E: {'*', '*'},
	0x6C: {'7', '7'}, 0x75: {'8', '8'}, 0x7D: {'9', '9'},
	0x6B: {'4', '4'}, 0x73: {'5', '5'}, 0x74: {'6', '6'},
	0x69: {'1', '1'}, 0x72: {'2', '2'}, 0x7A: {'3', '3'},
	0x70: {'0', '0'}, 0x71: {'.', '.'},
}

// Lookup returns the function named name.
func Lookup(name string) (Function, bool) {
	for _, f := range Functions {
		if f.Name == name {
			return f, true
		}
	}
	return Function{}, false
}

// twinaxKey is what one twinax key code means: a function, or a
// character pair (unshifted, shifted).
type twinaxKey struct {
	fn, shiftFn string
	chars       [2]rune
	isChar      bool
}

// twinaxKeys is built from Functions, Chars and ToTwinax: the meaning of
// every single-code twinax key.
var twinaxKeys = func() map[uint8]twinaxKey {
	m := map[uint8]twinaxKey{}
	for code, ch := range Chars {
		if seq := ToTwinax[code]; len(seq) == 1 {
			m[seq[0]] = twinaxKey{chars: ch, isChar: true}
		}
	}
	for _, f := range Functions {
		code, shift, ok := OnTwinax(f)
		if !ok {
			continue // PF keys (Cmd sequences) and keys that send nothing
		}
		k := m[code]
		if shift {
			k.shiftFn = f.Name
		} else {
			k.fn = f.Name
		}
		m[code] = k
	}
	return m
}()

// OnTwinax returns the twinax key code and Shift state in which function
// f reaches the controller: Shift + key, or a key the terminal sends as
// Shift make + code (ErInp: 57 6C). ok is false for PF keys (Cmd
// sequences) and keys that send nothing.
func OnTwinax(f Function) (code uint8, shift bool, ok bool) {
	seq := ToTwinax[f.Code]
	switch {
	case len(seq) == 1:
		return seq[0], f.Shift, true
	case len(seq) == 2 && seq[0] == TwinaxShiftLeft && !f.Shift:
		return seq[1], true, true
	}
	return 0, false, false
}

// DecodeTwinax returns the 5250 function name or character for a twinax
// key code (make code, without the Cmd prefix) with the given Shift
// state. ok is false for unknown codes. A shifted function falls back to
// the unshifted one; a key with both a character and a function (none
// today) prefers the function.
func DecodeTwinax(code uint8, shift bool) (fn string, ch rune, ok bool) {
	k, found := twinaxKeys[code]
	if !found {
		return "", 0, false
	}
	switch {
	case shift && k.shiftFn != "":
		return k.shiftFn, 0, true
	case k.fn != "":
		return k.fn, 0, true
	case k.isChar && shift:
		return "", k.chars[1], true
	case k.isChar:
		return "", k.chars[0], true
	}
	return "", 0, false
}
