package board

import "math/bits"

// Keyboard models the terminal's IBM keyboard on the far side of the
// serial keyboard cable. It speaks the AT/PS2 protocol the firmware uses
// (F000:0BF2 receive, F000:0CC3 send) with IBM scan code set 3: commands
// are acknowledged with FA, a reset answers FA, AA and the ID, and keys
// send make codes, plus F0+code on release only for keys set to
// make/break.
//
// Set 3 lets the host choose per key whether it sends a release code and
// whether it repeats. The 3477 firmware sends at start-up:
//
//	FF               reset
//	F9               all keys make-only (no release code, no repeat)
//	FB 61 6A 60 63   the four arrow keys typematic (repeat, no release)
//	F3 2C            typematic rate/delay
//	FC 59 12 14 39 19  Shift, Caps Lock (Ctrl) and Alt make/break
//	F4               enable
//	EE ...           echo, periodically (keep-alive)
type Keyboard struct {
	out     []uint8 // bytes queued for the terminal
	last    uint8   // last byte sent (for FE resend)
	needArg uint8   // command waiting for its argument byte (ED, F3, F0)
	keyList uint8   // FB/FC/FD collecting key codes (0 = none)
	enabled bool
	// mode holds the set-3 key type of each make code
	mode [0x100]uint8
	held [0x100]bool // keys currently down (to drop non-typematic repeats)
	// Log records every byte the terminal sent (for reverse engineering)
	Log []uint8
}

// Set-3 key types (bit flags).
const (
	keyBreak     = 1 // sends F0+code on release
	keyTypematic = 2 // repeats while held
)

// keyboardID is the 2-byte keyboard ID. The 3477 POST (F000:343D-3491)
// accepts an ID whose bytes are 101xxxxx or 011xxxxx (the second may also
// be 9F) with equal top two bits, and the firmware decodes it at
// F000:3FAE: first byte 10xxxxxx = keyboard type 2, 01xxxxxx = type 1;
// second byte & 1F == 14 selects a variant. BF BF (type 2, no variant)
// is used; which IBM keyboard it names is PROVISIONAL.
var keyboardID = []uint8{0xBF, 0xBF}

// powerOn queues what an IBM terminal keyboard sends by itself after
// power-on or reset: AA (self test passed) and its ID (POST F000:32AA
// waits for AA, then F000:3425 reads the ID without asking for it).
func (k *Keyboard) powerOn() {
	k.setDefaults()
	k.send(0xAA)
	k.send(keyboardID...)
}

// setDefaults sets the set-3 default key types (power-on, F6): every key
// typematic and make-only, except Ctrl, Shift, Alt and Caps Lock, which
// are make/break (IBM scan code set 3 defaults). The 3477 relies on this:
// after F6 it only re-types a few keys (ROM table D000:28BE), so a data
// key must not send a break, or the firmware forwards the release to the
// controller as a second keystroke.
func (k *Keyboard) setDefaults() {
	k.setAll(keyTypematic)
	for _, c := range []uint8{0x11, 0x12, 0x14, 0x19, 0x39, 0x58, 0x59} {
		k.mode[c] = keyBreak
	}
}

func (k *Keyboard) setAll(m uint8) {
	for i := range k.mode {
		k.mode[i] = m
	}
}

func (k *Keyboard) send(b ...uint8) { k.out = append(k.out, b...) }

// reply queues the answer to a command from the terminal ahead of key
// codes still waiting, as a real keyboard answers a command first. The
// 3477 sends an Echo (EE) about every 5 s; with a key code in front of
// the EE reply it took the keyboard for broken and reset it (FF and all
// key modes again), losing the key. A break already begun (F0 sent) is
// finished first: its key code must follow the F0.
func (k *Keyboard) reply(b ...uint8) {
	at := 0
	if k.last == 0xF0 && len(k.out) > 0 {
		at = 1
	}
	out := make([]uint8, 0, len(k.out)+len(b))
	out = append(out, k.out[:at]...)
	out = append(out, b...)
	k.out = append(out, k.out[at:]...)
}

// Command handles one byte written by the terminal.
func (k *Keyboard) Command(c uint8) {
	if len(k.Log) < 256 {
		k.Log = append(k.Log, c)
	}
	if k.needArg != 0 {
		// Argument of ED (LEDs), F3 (typematic rate), F0 (scan set)
		if k.needArg == 0xF0 && c == 0 {
			k.reply(0xFA, 0x03) // query scan set: set 3
		} else {
			k.reply(0xFA)
		}
		k.needArg = 0
		return
	}
	// FA/FB/FC/FD are followed by any number of key codes, ended by the next
	// command byte (ED-FF)
	if k.keyList != 0 && c < 0xED {
		switch k.keyList {
		case 0xFB:
			k.mode[c] = keyTypematic
		case 0xFC:
			k.mode[c] = keyBreak
		case 0xFD:
			k.mode[c] = 0
		case 0xFA:
			// Typematic only, no break: IBM's 122-key table (5250
			// Attachment Information, fig. 105) lists Left/Right (twinax
			// 72/73) as "T", and the firmware forwards a release of these
			// keys to the controller as the same code, a second keystroke.
			k.mode[c] = keyTypematic
		}
		k.reply(0xFA)
		return
	}
	k.keyList = 0
	switch c {
	case 0xFF: // reset: ack, then self test passed and the ID
		k.out = k.out[:0]
		k.enabled = true
		k.reply(0xFA)
		k.powerOn()
	case 0xFE: // resend
		k.reply(k.last)
	case 0xEE: // echo
		k.reply(0xEE)
	case 0xF2: // read ID
		k.reply(0xFA)
		k.reply(keyboardID...)
	case 0xED, 0xF3, 0xF0:
		k.needArg = c
		k.reply(0xFA)
	case 0xFB, 0xFC, 0xFD:
		k.keyList = c
		k.reply(0xFA)
	case 0xF4: // enable
		k.enabled = true
		k.reply(0xFA)
	case 0xF5: // disable
		k.enabled = false
		k.reply(0xFA)
	case 0xF6: // defaults
		k.setDefaults()
		k.reply(0xFA)
	case 0xF7: // all typematic
		k.setAll(keyTypematic)
		k.reply(0xFA)
	case 0xF8: // all make/break
		k.setAll(keyBreak)
		k.reply(0xFA)
	case 0xF9: // all make only
		k.setAll(0)
		k.reply(0xFA)
	case 0xFA:
		// The 3477 sends FA followed by key codes (ROM D000:28BE:
		// "FA 61 6A", Left/Right), so on its keyboard FA starts a
		// per-key list like FB/FC/FD. FA with no list would mean "all
		// keys" (set 3); the firmware never does that.
		k.keyList = c
		k.reply(0xFA)
	default:
		k.reply(0xFA)
	}
}

// Key queues a key event given as set-3 bytes: a make code (repeated
// while held for auto-repeat) or F0 + code for the release. The key's
// set-3 type decides what actually goes to the terminal: releases only for
// make/break keys, repeats only for typematic keys.
func (k *Keyboard) Key(codes ...uint8) {
	for i := 0; i < len(codes); i++ {
		c := codes[i]
		if c == 0xF0 && i+1 < len(codes) {
			i++
			code := codes[i]
			k.held[code] = false
			if k.mode[code]&keyBreak != 0 {
				k.send(0xF0, code)
			}
			continue
		}
		if k.held[c] && k.mode[c]&keyTypematic == 0 {
			continue // held key that does not repeat
		}
		k.held[c] = true
		k.send(c)
	}
}

// next pops the next byte for the terminal, if any.
func (k *Keyboard) next() (uint8, bool) {
	if len(k.out) == 0 {
		return 0, false
	}
	b := k.out[0]
	k.out = k.out[1:]
	k.last = b
	return b, true
}

// kbdPort is the gate array's keyboard interface (0x20012-0x20014).
//
//	0x20012 W: byte to send to the keyboard   R: last byte received
//	0x20013 W: 2 = line released (enabled), 3 = line inhibited
//	        R: bit0 = received parity bit (odd parity), bit1 = byte
//	           pending/line busy, bit2 = receive complete
//	0x20014 W: acknowledge - clears the pending byte and the interrupt
//
// A finished transmission or reception raises the keyboard interrupt
// (INT 0Eh, enabled by 0x20007 bit 0).
type kbdPort struct {
	kbd      Keyboard
	enabled  bool  // control = 2
	rx       uint8 // received byte latch
	pending  bool  // byte latched / transfer done, not yet acknowledged
	irq      bool
	txByte   uint8
	txQueued bool
	busy     int // cycles left in the current serial transfer
	txActive bool
	// wrap counts clock pulses in loopback (wrap) mode: POST test 8
	// (F000:3116) writes a byte, then pulses control 1/3 eleven times
	// (start, 8 data, parity, stop) and expects the byte back in the
	// receive register with its parity and an interrupt.
	wrap int
}

// wrapFrameClocks is one keyboard frame: start, 8 data, parity, stop.
const wrapFrameClocks = 11

// byteCycles is one keyboard serial frame (11 bits at about 10 kHz).
const byteCycles = cpuHz / 1000

func (p *kbdPort) status() uint8 {
	var s uint8
	if bits.OnesCount8(p.rx)&1 == 0 {
		s |= 0x01 // odd parity: parity bit set when data has even ones
	}
	if p.pending || p.busy > 0 {
		s |= 0x02
	}
	if p.pending {
		s |= 0x04
	}
	return s
}

func (p *kbdPort) writeData(v uint8) {
	p.txByte = v
	p.txQueued = true
	p.wrap = 0
}

// writeControl: 2 releases the line (normal operation), 3 inhibits it,
// and 1 is a clock pulse of the loopback test.
func (p *kbdPort) writeControl(v uint8) {
	p.enabled = v == 2
	if v != 1 {
		return
	}
	p.wrap++
	if p.wrap == wrapFrameClocks {
		// the frame shifted out arrives back in the receive register
		p.rx = p.txByte
		p.txQueued = false
		p.pending = true
		p.irq = true
		p.wrap = 0
	}
}

func (p *kbdPort) ack() {
	p.pending = false
	p.irq = false
}

// tick advances the serial line by n CPU cycles.
func (p *kbdPort) tick(n int) {
	if p.busy > 0 {
		p.busy -= n
		if p.busy > 0 {
			return
		}
		p.busy = 0
		if p.txActive {
			// transmission to the keyboard finished
			p.txActive = false
			p.kbd.Command(p.txByte)
			p.pending = true
			p.irq = true
		} else if b, ok := p.kbd.next(); ok {
			p.rx = b
			p.pending = true
			p.irq = true
		}
		return
	}
	if !p.enabled || p.pending {
		return // line inhibited or previous byte not acknowledged
	}
	switch {
	case p.txQueued:
		p.txQueued = false
		p.txActive = true
		p.busy = byteCycles
	case len(p.kbd.out) > 0:
		p.busy = byteCycles
	}
}
