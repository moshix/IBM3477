package emu

import (
	"strings"

	"ibm3486/internal/kbdlayout"
)

// Key events from the browser arrive as one of:
//   - a KeyboardEvent.code ("KeyA", "ShiftLeft", "F3", ...): the key at the
//     same position on the terminal's IBM 122-key keyboard is pressed;
//   - "fn:<Name>" ("fn:Reset", "fn:PF13", ...): a 5250 keyboard function
//     chosen in the browser's keyboard-mapping settings; the 122-key
//     keyboard key carrying that function is pressed.
//
// Codes are IBM scan code set 3 make codes; break is F0 + make.
//
// The layout (IBM enhanced 5250 keyboard) is shared with the workstation
// controller through internal/kbdlayout, so both ends agree on which key
// carries which 5250 function.

// set3 maps browser key positions to set-3 codes. Ctrl, Alt and Meta are
// deliberately absent: in the browser they only form shortcuts for the
// mapping (the key in the PC's left-Ctrl position is Reset on the 122-key
// keyboard, so passing Ctrl through would press Reset).
var set3 = map[string]uint8{
	"F1": 0x07, "F2": 0x0F, "F3": 0x17, "F4": 0x1F, "F5": 0x27, "F6": 0x2F,
	"F7": 0x37, "F8": 0x3F, "F9": 0x47, "F10": 0x4F, "F11": 0x56, "F12": 0x5E,
	"Backquote": 0x0E, "Digit1": 0x16, "Digit2": 0x1E, "Digit3": 0x26,
	"Digit4": 0x25, "Digit5": 0x2E, "Digit6": 0x36, "Digit7": 0x3D,
	"Digit8": 0x3E, "Digit9": 0x46, "Digit0": 0x45, "Minus": 0x4E,
	"Equal": 0x55, "Backspace": 0x66, "Tab": 0x0D,
	"KeyQ": 0x15, "KeyW": 0x1D, "KeyE": 0x24, "KeyR": 0x2D, "KeyT": 0x2C,
	"KeyY": 0x35, "KeyU": 0x3C, "KeyI": 0x43, "KeyO": 0x44, "KeyP": 0x4D,
	"BracketLeft": 0x54, "BracketRight": 0x5B, "Backslash": 0x53,
	"CapsLock": 0x14, "KeyA": 0x1C, "KeyS": 0x1B, "KeyD": 0x23, "KeyF": 0x2B,
	"KeyG": 0x34, "KeyH": 0x33, "KeyJ": 0x3B, "KeyK": 0x42, "KeyL": 0x4B,
	"Semicolon": 0x4C, "Quote": 0x52, "Enter": 0x5A,
	"ShiftLeft": 0x12, "IntlBackslash": 0x13, "KeyZ": 0x1A, "KeyX": 0x22, "KeyC": 0x21, "KeyV": 0x2A,
	"KeyB": 0x32, "KeyN": 0x31, "KeyM": 0x3A, "Comma": 0x41, "Period": 0x49,
	"Slash": 0x4A, "ShiftRight": 0x59, "Space": 0x29,
	"Insert": 0x67, "Delete": 0x64, "Home": 0x6E, "End": 0x65,
	"PageUp": 0x6F, "PageDown": 0x6D,
	"ArrowUp": 0x63, "ArrowDown": 0x60, "ArrowLeft": 0x61, "ArrowRight": 0x6A,
	"NumLock": 0x76, "NumpadDivide": 0x77, "NumpadMultiply": 0x7E,
	"NumpadSubtract": 0x84, "NumpadAdd": 0x7C, "NumpadEnter": 0x79,
	"Numpad7": 0x6C, "Numpad8": 0x75, "Numpad9": 0x7D, "Numpad4": 0x6B,
	"Numpad5": 0x73, "Numpad6": 0x74, "Numpad1": 0x69, "Numpad2": 0x72,
	"Numpad3": 0x7A, "Numpad0": 0x70, "NumpadDecimal": 0x71,
}

// ScanCodes returns the set-3 bytes for a key event, or nil for keys the
// emulated keyboard does not have (or 5250 functions not on the layout:
// Hex, FieldMark, Rule, Jump, Setup, Record, Play, CrSel). A function
// that is Shift + key (PF13-PF24, BackTab, ...) is sent as Shift make,
// key make on press and key break, Shift break on release.
func ScanCodes(code string, down bool) []uint8 {
	if name, isFn := strings.CutPrefix(code, "fn:"); isFn {
		f, ok := kbdlayout.Lookup(name)
		if !ok {
			return nil
		}
		switch {
		case down && f.Shift:
			return []uint8{kbdlayout.ShiftLeft, f.Code}
		case down:
			return []uint8{f.Code}
		case f.Shift:
			return []uint8{0xF0, f.Code, 0xF0, kbdlayout.ShiftLeft}
		default:
			return []uint8{0xF0, f.Code}
		}
	}
	sc, ok := set3[code]
	if !ok {
		return nil
	}
	if down {
		return []uint8{sc}
	}
	return []uint8{0xF0, sc}
}
