package emu

import (
	"sort"

	"ibm3486/internal/kbdlayout"
	"ibm3486/internal/tn5250"
)

// Pasting types text through the emulated keyboard, one key at a time,
// so the terminal and the 5250 controller apply their normal rules
// (field types, auto-advance) exactly as for real typing.

// pasteKeyGap is the time between pasted keys, in CPU cycles: 10 ms per
// character (100 per second). The real firmware keeps up even at 3 ms,
// but a Shift+letter needs about 8 ms to reach the controller (6 bytes
// on the 1 ms/byte keyboard line, then the firmware and the next poll).
// The gap must cover that, so a character typed directly (ä, é) never
// overtakes the key before it, and a paste stops right at the character
// a field rejects. TestFirmwarePasteASCII and TestMachinePaste check it.
const pasteKeyGap = cpuHz / 100

// charKeys maps a character to its key: set-3 make code and Shift.
var charKeys = func() map[rune]struct {
	code  uint8
	shift bool
} {
	m := map[rune]struct {
		code  uint8
		shift bool
	}{}
	// in code order, so a character two keys can type (< on its own key
	// and on Shift+,) always uses the same one
	codes := make([]int, 0, len(kbdlayout.Chars))
	for code := range kbdlayout.Chars {
		codes = append(codes, int(code))
	}
	sort.Ints(codes)
	for _, ci := range codes {
		code := uint8(ci)
		ch := kbdlayout.Chars[code]
		// main keyboard first: keep the first code found for a character
		// unless the new one is not a numeric-pad key (0x69-0x7E)
		for i, r := range ch {
			old, seen := m[r]
			pad := code >= 0x69 && code <= 0x7E
			if !seen || (old.code >= 0x69 && old.code <= 0x7E && !pad) {
				m[r] = struct {
					code  uint8
					shift bool
				}{code, i == 1 && ch[0] != ch[1]}
			}
		}
	}
	return m
}()

// pasteStep is one step of a paste: a key press or release group as
// set-3 bytes (keys), or a character the US keyboard has no key for
// (char, e.g. ä ö ü ß é), which goes to the host session's field editor
// directly, in order with the typed keys.
type pasteStep struct {
	keys []uint8
	char rune
}

// keyGroups returns only the keystrokes of a paste (characters without
// a key are left out).
func keyGroups(text string) [][]uint8 {
	var out [][]uint8
	for _, st := range pasteSteps(text) {
		if st.keys != nil {
			out = append(out, st.keys)
		}
	}
	return out
}

// pasteSteps converts text to paste steps: one step per key, press and
// release together (the keyboard line spaces the bytes 1 ms apart, so
// the terminal still sees make, then break). Line breaks become New
// Line, tabs Tab; control characters are skipped.
func pasteSteps(text string) []pasteStep {
	var out []pasteStep
	press := func(code uint8, shift bool) {
		if shift {
			out = append(out, pasteStep{keys: []uint8{kbdlayout.ShiftLeft, code, 0xF0, code, 0xF0, kbdlayout.ShiftLeft}})
			return
		}
		out = append(out, pasteStep{keys: []uint8{code, 0xF0, code}})
	}
	for _, r := range text {
		switch r {
		case '\r':
			continue
		case '\n':
			if f, ok := kbdlayout.Lookup("NewLine"); ok {
				press(f.Code, f.Shift)
			}
			continue
		case '\t':
			if f, ok := kbdlayout.Lookup("Tab"); ok {
				press(f.Code, f.Shift)
			}
			continue
		}
		if k, ok := charKeys[r]; ok {
			press(k.code, k.shift)
		} else if r >= 0x20 && r != 0x7F {
			out = append(out, pasteStep{char: r})
		}
	}
	return out
}

// MaxPasteChars is the most characters one paste types; the rest of a
// longer text is dropped. At 100 characters a second that is well over a
// minute of typing, and it keeps a client from queueing unbounded work.
const MaxPasteChars = 8192

// Paste queues text to be typed (safe to call from any goroutine). Only
// one paste is typed at a time: text pasted while another paste is still
// being typed is dropped and reported as busy, and everything after
// MaxPasteChars characters is dropped and reported as truncated.
func (m *Machine) Paste(text string) (truncated, busy bool) {
	if m.pasting.Load() {
		return false, true
	}
	text, truncated = truncateRunes(text, MaxPasteChars)
	select {
	case m.paste <- pasteSteps(text):
	default: // a paste is already waiting to be typed
		return false, true
	}
	return truncated, false
}

// pasteStallLimit is how long (CPU cycles) a pasted key waits for the
// keyboard buffer to drain before the paste is given up: 5 s.
const pasteStallLimit = 5 * cpuHz

// pasteKeyboardReady reports whether the next pasted key may be pressed:
// only once the keys pressed before it have been taken by the terminal,
// so a paste never overflows the keyboard's 16-byte buffer (whose
// overflow drops keys) while the firmware is busy. If the buffer does
// not drain for pasteStallLimit, the paste is given up. Called from the
// machine goroutine.
func (m *Machine) pasteKeyboardReady() bool {
	if !m.b.IO.Keyboard().Pending() {
		m.stallSince = 0
		return true
	}
	now := m.b.CPU.Cycles
	if m.stallSince == 0 {
		m.stallSince = now
	} else if now-m.stallSince > pasteStallLimit {
		m.pending = nil // the terminal stopped taking keys
		m.pasting.Store(false)
		m.stallSince = 0
	}
	return false
}

// truncateRunes cuts s after n characters.
func truncateRunes(s string, n int) (string, bool) {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos], true
		}
		i++
	}
	return s, false
}

// SetCodepage sets the EBCDIC codepage used to decode the screen text
// for copying (e.g. "37", "273"; "" = 37); call before Start.
func (m *Machine) SetCodepage(cp string) {
	if cp == "" {
		cp = "37"
	}
	m.decode = tn5250.EBCDICTable(cp)
}

// screenText decodes the cells into text rows. Non-display cells are
// left blank so passwords are never copied: the firmware marks a 5250
// non-display field (0x27/2F/37/3F) with bit 7 of the third cell byte
// (cell attribute 00/10), and its own screens use attribute 0x27.
// cellNonDisplay is the third-cell-byte bit of a non-display field.
const cellNonDisplay = 0x80

func screenText(cells []byte, cols int, decode *[256]rune) []string {
	if cols == 0 {
		return nil
	}
	rows := len(cells) / (cols * 3)
	out := make([]string, rows)
	line := make([]rune, cols)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := (r*cols + c) * 3
			ch, attr, extra := cells[i], cells[i+1], cells[i+2]
			hidden := extra&cellNonDisplay != 0 || (attr&0x20 != 0 && attr&0x07 == 0x07)
			ru := ' '
			if !hidden && ch != 0x00 && ch != 0x40 {
				ru = decode[ch]
				if ru < 0x20 {
					ru = ' '
				}
			}
			line[c] = ru
		}
		out[r] = string(line)
	}
	return out
}
