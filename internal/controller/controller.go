// Package controller emulates the twinax workstation controller (the
// role of a 5294/5494 or an AS/400 workstation controller) between a
// TN5250 host session and one emulated InfoWindow display station.
//
// On twinax the controller owns the 5250 logic: it keeps the host's
// presentation space and format table (here: a tn5250.Client), mirrors
// the screen into the terminal with station commands, polls the terminal
// for keystrokes (raw scan codes) and turns them into field editing and
// AID keys. Station command encodings follow IBM's 5250 station protocol
// as used by inmbolmie's 5250_usb_converter; see TWINAX.md.
package controller

import (
	"fmt"
	"unicode"

	"ibm3486/internal/board"
	"ibm3486/internal/kbdlayout"
	"ibm3486/internal/tn5250"
)

// Station is the twinax station (the emulated terminal's twinax engine).
type Station interface {
	Deliver(addr, cmd uint8, data []uint8) (board.Response, bool)
}

// Host is the 5250 session the controller serves.
type Host interface {
	Screen() tn5250.ScreenState
	TypeChar(r rune)
	Key(name string)
}

// Station commands (low 5 bits = command, high bits = modifiers).
const (
	cmdWCD     = 0x05 // Write Control Data: [control byte]
	cmdLRC     = 0x07 // Load Reference Counter: [addr hi, addr lo]
	cmdPoll    = 0x10
	cmdWDLC    = 0x11 // Write Data and Load Cursor: [count, data...]
	cmdWDLCInd = 0x51 // Write Data and Load Cursor with indicators: [indicators]
	cmdWCDInd  = 0x45 // Write Control Data with indicators: [indicators]
	cmdClear   = 0x12 // Clear from address counter to reference counter
	cmdSetMode = 0x13 // Set Mode: [mode], 0 = zero fill
	cmdLAC     = 0x15 // Load Address Counter: [addr hi, addr lo]
	cmdLCR     = 0x17 // Load Cursor Register: [addr hi, addr lo]
	cmdAck     = 0x30 // acknowledge the last poll response
	cmdEOQ     = 0x62 // end of command queue
)

// Poll response status bits.
const (
	stBusy      = 0x80 // a command queue is still being processed
	stException = 0x0E // exception status field; 0x0E = 7 = power on
)

// Write Control Data control byte bits.
const (
	wcdResetException = 0x04
	// wcdAlarm in the Write Control Data byte sounds the alarm: the
	// firmware keeps the byte at [0x1207] and on bit 0 starts a 50-tick
	// (60 Hz, ~0.8 s) tone (C000:93EF-93F6).
	wcdAlarm = 0x01
)

// Indicator byte of Write Data and Load Cursor with indicators (0x51).
// The 3477 firmware (C000:99E9 -> C000:91D9) reads exactly one data byte
// and rebuilds every indicator it controls from it, so the byte always
// carries the full set. Each bit selects a symbol-font glyph in the
// status row (row 25): see the notes (oia/notes.md) for the ROM evidence.
const (
	indSystemAvailable = 0x80 // col 3: session box (glyph 02)
	indMessageWaiting  = 0x40 // col 44: solid block (glyph 01)
	indKeyboardShift   = 0x20 // col 36: shift arrow (glyph 06); not driven yet
	indInsertMode      = 0x08 // col 41: caret (glyph 05)
	indInputInhibited  = 0x02 // col 8: X (glyph 03)

	// wcdIndSession is the 0x45 indicator bit that shows "»" at col 12
	wcdIndSession = 0x10
)

// Attribute bytes used for the operator error code on the error line.
const (
	attrNormal  = 0x20
	attrReverse = 0x21
)

// maxChunk is the most data bytes per Write Data command: a queue is
// LAC(3) + WDLC(2+n) + EOQ(1) and the station buffer holds 16 bytes.
const maxChunk = 10

type message struct {
	cmd  uint8
	data []uint8
}

// Controller drives one display station.
type Controller struct {
	st   Station
	addr uint8
	scan bool // searching for the station: poll addresses 0-6 in turn
	host Host

	queues    [][]message // command queues waiting to be sent
	pollEvery int         // CPU cycles between polls
	since     int

	// shadow is what the terminal is known to display (nil = unknown,
	// repaint everything); cursor is its cursor address (-1 = unknown).
	shadow       []byte
	rows, cols   int
	cursor       int
	initializing bool
	shift        bool // Shift held (twinax 57/56 make .. D7/D6 break)
	caps         bool // Caps Lock on (toggled by twinax 54)
	cmd          bool // the Cmd prefix (6F) of a PF key was received
	painted      bool // the first full repaint of the host screen is done
	// indicators is the last indicator byte sent with 0x51 (-1 = unknown,
	// send it on the next sync).
	indicators int
	// wcdIndicators is the last 0x45 indicator byte sent (-1 = unknown)
	wcdIndicators int
	// alarms is the host's alarm count already sounded (-1 = not yet read)
	alarms int

	// Online reports whether the station answered the last poll.
	Online bool
}

// New returns a controller for the station at addr (0-6), polling every
// pollEvery CPU cycles. With addr < 0 it searches addresses 0-6, as a
// controller does for a station whose address (set in the terminal's
// setup) is not known, and stays on the first that answers.
func New(st Station, addr int, pollEvery int, host Host) *Controller {
	c := &Controller{st: st, host: host, pollEvery: pollEvery, cursor: -1, indicators: -1, wcdIndicators: -1, alarms: -1}
	if addr < 0 {
		c.scan = true
	} else {
		c.addr = uint8(addr)
	}
	return c
}

// Address returns the station address being served.
func (c *Controller) Address() uint8 { return c.addr }

// Tick advances controller time by n CPU cycles, polling when due.
func (c *Controller) Tick(n int) {
	c.since += n
	if c.since < c.pollEvery {
		return
	}
	c.since = 0
	c.poll()
}

// poll runs one poll cycle: poll, acknowledge, handle power-on and
// keystrokes, then send the next command queue if the station is idle.
func (c *Controller) poll() {
	r, ok := c.st.Deliver(c.addr, cmdPoll, nil)
	if !ok || !r.Polled {
		c.Online = false
		if c.scan {
			c.addr = (c.addr + 1) % 7
		}
		return
	}
	c.Online = true
	c.scan = false
	c.st.Deliver(c.addr, cmdAck, nil)

	if r.Status&stException == stException {
		if !c.initializing {
			c.startInit()
		}
	} else {
		c.initializing = false
	}
	if r.Scan != 0 && c.host != nil {
		c.key(r.Scan)
	}
	if len(c.queues) == 0 && c.shadow != nil {
		c.painted = true // the host screen is on the station
	}
	if r.Status&stBusy != 0 {
		return
	}
	if len(c.queues) == 0 && !c.initializing {
		c.sync()
	}
	if len(c.queues) > 0 {
		q := c.queues[0]
		c.queues = c.queues[1:]
		for _, m := range q {
			c.st.Deliver(c.addr, m.cmd, m.data)
		}
	}
}

// startInit queues the station bring-up after power on (exception 7):
// Set Mode (zero fill), Write Control Data with reset-exception, and a
// clear of the whole screen; the screen is then repainted.
func (c *Controller) startInit() {
	c.initializing = true
	c.queues = [][]message{
		{{cmdSetMode, []uint8{0}}, {cmdEOQ, nil}},
		{{cmdWCD, []uint8{wcdResetException}}, {cmdEOQ, nil}},
	}
	c.shadow = nil
	c.cursor = -1
	c.indicators = -1
	c.wcdIndicators = -1
}

// addr2 encodes a screen position as the station's 2-byte address
// (row*cols + col, big-endian).
func addr2(pos int) []uint8 { return []uint8{uint8(pos >> 8), uint8(pos)} }

// sync compares the host screen with what the station shows and queues
// the station commands that bring it up to date.
func (c *Controller) sync() {
	if c.host == nil {
		return
	}
	s := c.host.Screen()
	n := s.Rows * s.Cols
	want := stationImage(s)
	if c.shadow == nil || s.Rows != c.rows || s.Cols != c.cols {
		// unknown or resized: clear the station screen, then repaint
		c.rows, c.cols = s.Rows, s.Cols
		c.shadow = make([]byte, n)
		c.queues = append(c.queues, []message{
			{cmdLAC, addr2(0)}, {cmdLRC, addr2(n - 1)}, {cmdClear, nil}, {cmdEOQ, nil},
		})
		c.cursor = -1
	}
	wrote := false
	for row := 0; row < s.Rows; row++ {
		base := row * s.Cols
		for col := 0; col < s.Cols; {
			p := base + col
			if want[p] == c.shadow[p] {
				col++
				continue
			}
			// a run of changed bytes, at most maxChunk long, within the row
			end := col
			for end < s.Cols && end-col < maxChunk && want[base+end] != c.shadow[base+end] {
				end++
			}
			chunk := append([]uint8{uint8(end - col)}, want[p:base+end]...)
			c.queues = append(c.queues, []message{
				{cmdLAC, addr2(p)}, {cmdWDLC, chunk}, {cmdEOQ, nil},
			})
			copy(c.shadow[p:base+end], want[p:base+end])
			wrote = true
			col = end
		}
	}
	cur := s.CursorRow*s.Cols + s.CursorCol
	if wrote || cur != c.cursor {
		c.queues = append(c.queues, []message{{cmdLCR, addr2(cur)}, {cmdEOQ, nil}})
		c.cursor = cur
	}
	// Indicators go last, so a keyboard unlock follows the screen it
	// belongs to; they are sent only when they change.
	if ind := indicatorByte(s); ind != c.indicators {
		c.queues = append(c.queues, []message{{cmdWDLCInd, []uint8{uint8(ind)}}, {cmdEOQ, nil}})
		c.indicators = ind
	}
	// a new alarm (host or operator error): Write Control Data with the
	// alarm bit; the other bits are those sent at bring-up
	if c.alarms < 0 {
		c.alarms = s.Alarms // attach: earlier alarms are history
	} else if s.Alarms != c.alarms {
		c.alarms = s.Alarms
		c.queues = append(c.queues, []message{{cmdWCD, []uint8{wcdResetException | wcdAlarm}}, {cmdEOQ, nil}})
	}
	if wind := wcdIndicatorByte(s); wind != c.wcdIndicators {
		c.queues = append(c.queues, []message{{cmdWCDInd, []uint8{uint8(wind)}}, {cmdEOQ, nil}})
		c.wcdIndicators = wind
	}
}

// wcdIndicatorByte is the indicator byte of Write Control Data with
// indicators (0x45, C000:95C4). Photos of a real 3477 attached to an
// AS/400 show its "»" at status-row column 12, which is bit 0x10, while
// a session is attached; the other bits (Caps Lock "A", "+", ...) are
// not driven.
func wcdIndicatorByte(s tn5250.ScreenState) int {
	if s.Connected {
		return wcdIndSession
	}
	return 0
}

// Shutdown clears the indicators the controller set, before it is
// detached (otherwise the terminal clears them only after its ~2 s
// polling timeout). Called from the machine goroutine.
func (c *Controller) Shutdown() {
	if !c.Online {
		return
	}
	// one queue: a second queue sent before the station is idle again
	// would be lost
	c.st.Deliver(c.addr, cmdWDLCInd, []uint8{0})
	c.st.Deliver(c.addr, cmdWCDInd, []uint8{0})
	c.st.Deliver(c.addr, cmdEOQ, nil)
}

// indicatorByte maps the host session state to the 0x51 indicator byte.
// Host lock (X SYSTEM) and operator errors (X II) both light Input
// Inhibited: the 3477 has a single X symbol for it, and the texts SYSTEM
// and II do not exist in its ROM.
func indicatorByte(s tn5250.ScreenState) int {
	ind := 0
	if s.Connected {
		ind |= indSystemAvailable
	}
	if s.MessageWait {
		ind |= indMessageWaiting
	}
	if s.Insert {
		ind |= indInsertMode
	}
	if s.Inhibited {
		ind |= indInputInhibited
	}
	return ind
}

// operatorError reports whether the keyboard is locked by an operator
// error (a keyboard SRC) rather than by the host.
func operatorError(s tn5250.ScreenState) bool {
	return s.Inhibited && s.ErrorCode != 0
}

// TypeDirect types a character into the host session without a key: for
// pasted characters the terminal's keyboard has no key for (ä, é, ...).
// Ignored before the host screen is on the station, like typed keys.
// Called from the machine goroutine.
func (c *Controller) TypeDirect(r rune) {
	if c.host != nil && c.painted {
		c.host.TypeChar(r)
	}
}

// InputError reports whether the keyboard is locked by an operator error
// (a rejected keystroke), which ends a paste.
func (c *Controller) InputError() bool {
	return c.host != nil && operatorError(c.host.Screen())
}

// stationImage returns what the station should display: the host's
// presentation space, plus the operator error code while the keyboard is
// locked by an operator error. A twinax station has no way to show the
// code itself (row 25 is not addressable by the controller), so the
// controller writes it, as 5250 controllers do, in reverse image at the
// start of the error line: [reverse attr][4 hex digits][normal attr].
// The host's row comes back through the screen diff when Reset clears
// the error.
func stationImage(s tn5250.ScreenState) []byte {
	if !operatorError(s) || s.Cols < 6 || s.ErrorRow < 0 || s.ErrorRow >= s.Rows {
		return s.Data
	}
	img := append([]byte(nil), s.Data...)
	p := s.ErrorRow * s.Cols
	img[p] = attrReverse
	code := s.ErrorCode & 0xFFFF
	for i := 0; i < 4; i++ {
		// hex digit i (most significant first) as EBCDIC 0-9 / A-F
		d := code >> uint(12-4*i) & 0xF
		if d < 10 {
			img[p+1+i] = 0xF0 + uint8(d)
		} else {
			img[p+1+i] = 0xC1 + uint8(d-10)
		}
	}
	img[p+5] = attrNormal
	return img
}

// key interprets one twinax key code from the station. The 3477 sends
// the 5250 keyboard codes of its 122-key keyboard (kbdlayout.ToTwinax):
// Shift and Caps Lock as make/break codes, PF keys as the Cmd prefix (6F)
// followed by 31..3C, all other keys as one make code. The terminal
// manages the controller's Shift state itself: for F13-F24 it sends
// Cmd, Shift make, 31..3C; for Shift+F1 it sends Shift break first; it
// sends the matching break before the next key.
func (c *Controller) key(scan uint8) {
	release := scan&kbdlayout.TwinaxRelease != 0
	code := scan &^ kbdlayout.TwinaxRelease
	switch code {
	case kbdlayout.TwinaxShiftLeft, kbdlayout.TwinaxShiftRight:
		c.shift = !release
		return
	case kbdlayout.TwinaxCapsLock:
		if !release {
			c.caps = !c.caps
		}
		return
	}
	if release {
		return
	}
	if !c.painted {
		// Keys typed before this host session reached the screen (the
		// terminal holds up to 7 in its typeahead queue while no
		// controller polls) are dropped, not typed into the host.
		c.cmd = false
		return
	}
	if code == kbdlayout.TwinaxCmd {
		c.cmd = true
		return
	}
	if c.cmd {
		c.cmd = false
		if n := int(code) - kbdlayout.TwinaxPFBase; n >= 1 && n <= 12 {
			if c.shift {
				n += 12
			}
			c.host.Key(fmt.Sprintf("PF%d", n))
		}
		return
	}
	fn, ch, ok := kbdlayout.DecodeTwinax(code, c.shift)
	switch {
	case !ok:
	case fn != "":
		c.host.Key(fn)
	default:
		// Caps Lock shifts letters only, like a typewriter shift lock
		if c.caps && unicode.IsLower(ch) {
			ch = unicode.ToUpper(ch)
		}
		c.host.TypeChar(ch)
	}
}
