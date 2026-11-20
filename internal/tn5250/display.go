// Copyright 2026 by moshix. All rights reserved.

package tn5250

import "strings"

// display is the Go port of lib5250's Tn5250Display (display.c / display.h). It
// owns the chain of display buffers (for save/restore screen), the character
// map, the OIA indicators, and the keyboard state. The session drives it while
// processing the 5250 data stream; the client reads from it to build snapshots.
//
// The C original also owns a curses terminal and a keystroke queue. A browser
// terminal has neither: keystrokes arrive as whole-field edits via SendAID, and
// rendering is a Snapshot. So those pieces are dropped and their side effects
// (beep, indicator changes) are captured as flags the snapshot reads.

// OIA indicator bits (display.h).
const (
	indInhibit        = 0x0001
	indMessageWaiting = 0x0002
	indXSystem        = 0x0004
	indXClock         = 0x0008
	indInsert         = 0x0010
	indFER            = 0x0020
	indMacro          = 0x0040
)

type display struct {
	buffers *dbuffer // head of the circular list == the active buffer
	cmap    *charMap

	indicators int
	keystate   int
	keySRC     int

	pendingInsert bool

	msgLine      []byte // current operator-error message row contents
	savedMsgLine []byte // the row we overwrote, restored when inhibit clears
	msgLen       int
	errorText    string // decoded message text, surfaced in the Snapshot

	alarm       bool // a beep happened this frame
	alarms      int  // beeps so far (the twinax controller sounds each one)
	signKeyHack bool // '+'/'-' act as field-exit/field-minus in numeric fields
}

func newDisplay(w, h int, codepage string) *display {
	d := &display{
		cmap:        newCharMap(codepage),
		keystate:    KeyStateUnlocked,
		keySRC:      KBDSrcNone,
		signKeyHack: true,
	}
	d.buffers = newDBuffer(w, h)
	d.buffers.next = d.buffers
	d.buffers.prev = d.buffers
	return d
}

// ── thin accessors mirroring the display.h macros ───────────────────────

func (d *display) dbuf() *dbuffer            { return d.buffers }
func (d *display) width() int                { return d.buffers.w }
func (d *display) height() int               { return d.buffers.h }
func (d *display) cursorX() int              { return d.buffers.cx }
func (d *display) cursorY() int              { return d.buffers.cy }
func (d *display) setCursor(y, x int)        { d.buffers.cursorSet(y, x) }
func (d *display) charAt(y, x int) byte      { return d.buffers.charAt(y, x) }
func (d *display) addch(c byte)              { d.buffers.addch(c) }
func (d *display) roll(top, bot, lines int)  { d.buffers.roll(top, bot, lines) }
func (d *display) setIC(y, x int)            { d.buffers.setIC(y, x) }
func (d *display) fieldData(f *field) []byte { return d.buffers.fieldData(f) }

func (d *display) setHeaderData(data []byte) { d.buffers.setHeaderData(data) }

func (d *display) inhibited() bool { return d.indicators&indInhibit != 0 }
func (d *display) inhibit()        { d.indicatorSet(indInhibit) }
func (d *display) uninhibit()      { d.indicatorClear(indInhibit) }

func (d *display) beep() { d.alarm = true; d.alarms++ }

func (d *display) mdt() bool { return d.buffers.masterMDT }

// ── indicators (display.c) ──────────────────────────────────────────────

func (d *display) indicatorSet(inds int) { d.indicators |= inds }

func (d *display) indicatorClear(inds int) {
	d.indicators &^= inds
	// Restore the message line when the inhibit indicator is cleared.
	if inds&indInhibit != 0 && d.savedMsgLine != nil {
		l := d.buffers.msgLine()
		copy(d.buffers.data[l*d.width():], d.savedMsgLine)
		d.savedMsgLine = nil
		d.msgLine = nil
		d.errorText = ""
	}
}

// ── field lookup / navigation (display.c) ───────────────────────────────

func (d *display) fieldAt(y, x int) *field { return d.buffers.fieldYX(y, x) }

func (d *display) currentField() *field {
	return d.fieldAt(d.cursorY(), d.cursorX())
}

// nextField returns the next non-bypass field after the cursor, wrapping.
func (d *display) nextField() *field {
	y, x := d.cursorY(), d.cursorX()
	iter := d.fieldAt(y, x)
	if iter == nil {
		for iter == nil {
			iter = d.fieldAt(y, x)
			if iter == nil {
				x++
				if x == d.buffers.w {
					x = 0
					y++
					if y == d.buffers.h {
						y = 0
					}
				}
				if y == d.cursorY() && x == d.cursorX() {
					return nil
				}
			}
		}
	} else {
		iter = iter.next
	}
	next := iter
	for next.isBypass() {
		next = next.next
		if next == iter && next.isBypass() {
			return nil
		}
	}
	return next
}

// prevField returns the first preceding non-bypass field, wrapping.
func (d *display) prevField() *field {
	y, x := d.cursorY(), d.cursorX()
	iter := d.fieldAt(y, x)
	if iter == nil {
		for iter == nil {
			iter = d.fieldAt(y, x)
			if iter == nil {
				if x == 0 {
					x = d.buffers.w - 1
					if y == 0 {
						y = d.buffers.h - 1
					} else {
						y--
					}
				} else {
					x--
				}
				if y == d.cursorY() && x == d.cursorX() {
					return nil
				}
			}
		}
	} else {
		iter = iter.prev
	}
	prev := iter
	for prev.isBypass() {
		prev = prev.prev
		if prev == iter && prev.isBypass() {
			return nil
		}
	}
	return prev
}

// setCursorHome moves to the IC target, else the first non-bypass field, else
// 0,0 (tn5250_display_set_cursor_home).
func (d *display) setCursorHome() {
	if d.pendingInsert {
		d.buffers.gotoIC()
		return
	}
	y, x := 0, 0
	if iter := d.buffers.fieldList; iter != nil {
		for {
			if !iter.isBypass() {
				y, x = iter.startRow, iter.startCol
				break
			}
			iter = iter.next
			if iter == d.buffers.fieldList {
				break
			}
		}
	}
	d.setCursor(y, x)
}

// homeTarget returns the Home / Insert-Cursor target WITHOUT moving the cursor.
// It mirrors setCursorHome's TARGET selection so the snapshot can carry the
// position the browser's Home key should jump to: the host IC target if
// pendingInsert is set, else the first non-bypass field, else 0,0.
func (d *display) homeTarget() (row, col int) {
	if d.pendingInsert {
		return d.buffers.tcy, d.buffers.tcx
	}
	if iter := d.buffers.fieldList; iter != nil {
		for {
			if !iter.isBypass() {
				return iter.startRow, iter.startCol
			}
			iter = iter.next
			if iter == d.buffers.fieldList {
				break
			}
		}
	}
	return 0, 0
}

func (d *display) setCursorField(f *field) {
	if f == nil {
		d.setCursorHome()
		return
	}
	d.buffers.cursorSet(f.startRow, f.startCol)
}

func (d *display) setCursorNextField() {
	d.setCursorField(d.nextField())
}

func (d *display) setPendingInsert(y, x int) {
	d.pendingInsert = true
	d.buffers.setIC(y, x)
}

// ── clear operations (display.c) ────────────────────────────────────────

func (d *display) clearUnit() {
	d.buffers.setSize(24, 80)
	d.indicatorSet(indXSystem)
	d.keystate = KeyStateLocked
	d.indicatorClear(indInsert | indInhibit | indFER)
	d.pendingInsert = false
	d.buffers.setIC(0, 0)
	d.savedMsgLine = nil
	d.msgLine = nil
	d.errorText = ""
}

func (d *display) clearUnitAlternate() {
	d.buffers.setSize(27, 132)
	d.indicatorSet(indXSystem)
	d.keystate = KeyStateLocked
	d.indicatorClear(indInsert | indInhibit | indFER)
	d.pendingInsert = false
	d.buffers.setIC(0, 0)
	d.savedMsgLine = nil
	d.msgLine = nil
	d.errorText = ""
}

func (d *display) clearFormatTable() {
	d.buffers.clearTable()
	d.setCursor(0, 0)
	d.indicatorSet(indXSystem)
	d.keystate = KeyStateLocked
	d.indicatorClear(indInsert)
}

// ── message line (display.c) ────────────────────────────────────────────

func (d *display) saveMsgLine() {
	l := d.buffers.msgLine()
	d.savedMsgLine = make([]byte, d.width())
	copy(d.savedMsgLine, d.buffers.data[l*d.width():])
}

func (d *display) setMsgLine(msg []byte) {
	d.msgLine = make([]byte, d.width())
	copy(d.msgLine, msg)
	d.msgLen = len(msg)
	l := d.buffers.msgLine()
	// Clamp the on-screen copy to the display width. d.msgLine is only width
	// bytes, so a WEC message longer than the screen would make d.msgLine[:d.msgLen]
	// slice past its capacity and panic. tn5250 silently overruns the row; we
	// truncate it instead (see cursesterm.c / dbuffer bounds).
	n := d.msgLen
	if n > d.width() {
		n = d.width()
	}
	copy(d.buffers.data[l*d.width():], d.msgLine[:n])
	// Decode the message for the snapshot's ErrorText.
	b := make([]rune, 0, d.msgLen)
	for i := 0; i < d.msgLen; i++ {
		b = append(b, d.cmap.toLocalRune(msg[i]))
	}
	d.errorText = string(b)
}

// eraseRegion blanks a rectangular region (tn5250_display_erase_region).
func (d *display) eraseRegion(startrow, startcol, endrow, endcol, leftedge, rightedge int) {
	blank := d.cmap.remote(' ')
	w := d.buffers.w
	if startrow == endrow {
		for j := startcol - 1; j < endcol; j++ {
			d.buffers.data[(startrow-1)*w+j] = blank
		}
		return
	}
	for i := startrow - 1; i < endrow; i++ {
		switch {
		case i == startrow-1:
			for j := startcol - 1; j < rightedge; j++ {
				d.buffers.data[i*w+j] = blank
			}
		case i == endrow-1:
			for j := leftedge - 1; j < endcol; j++ {
				d.buffers.data[i*w+j] = blank
			}
		default:
			for j := leftedge - 1; j < rightedge; j++ {
				d.buffers.data[i*w+j] = blank
			}
		}
	}
}

// ── field adjust on exit (display.c) ────────────────────────────────────

// shiftRight right-justifies a field, left-filling with fill
// (tn5250_display_shift_right).
func (d *display) shiftRight(f *field, fill byte) {
	ptr := d.fieldData(f)
	end := f.length - 1
	f.setMDT(d.buffers)
	if f.isSignedNum() {
		end-- // never move into the sign position
	}
	n := 0
	for n <= end && (ptr[n] == 0 || ptr[n] == 0x40) {
		ptr[n] = fill
		n++
	}
	if n > end {
		return
	}
	for ptr[end] == 0 || ptr[end] == 0x40 {
		for n = end; n > 0; n-- {
			ptr[n] = ptr[n-1]
		}
		ptr[0] = fill
	}
}

// fieldAdjust applies the field's mandatory-fill/right-adjust rule
// (tn5250_display_field_adjust).
func (d *display) fieldAdjust(f *field) {
	mandFill := f.mandFillType()
	if f.fieldType() == FieldSignedNum {
		mandFill = FieldRightBlank
	}
	switch mandFill {
	case FieldNoAdjust, FieldMandatoryFill:
		// nothing
	case FieldRightZero:
		d.shiftRight(f, d.cmap.remote('0'))
	case FieldRightBlank:
		d.shiftRight(f, d.cmap.remote(' '))
	}
	f.setMDT(d.buffers)
}

// ── save/restore screen stack (display.c) ───────────────────────────────

// pushDBuffer copies the active buffer and makes the copy active, returning the
// old buffer as the restore handle (tn5250_display_push_dbuffer).
func (d *display) pushDBuffer() *dbuffer {
	nb := d.buffers.copy()
	// Insert nb ahead of the current head and make it the head.
	nb.next = d.buffers
	nb.prev = d.buffers.prev
	nb.next.prev = nb
	nb.prev.next = nb
	old := d.buffers
	d.buffers = nb
	return old
}

// makeWTDData reconstructs a Write-To-Display stream for the active buffer,
// used to answer Save Screen (tn5250_display_make_wtd_data).
func (d *display) makeWTDData(buf *[]byte) {
	ctx := newWTDContext(buf, d.buffers)
	ctx.setIC(d.cursorY()+1, d.cursorX()+1)
	ctx.convert()
}

// ── word wrap (display.c) ───────────────────────────────────────────────

// wordwrap places EBCDIC text into a word-wrap field, wrapping words across the
// field's continued word-wrap group. It is used by the WRITE-DATA structured
// field (session.c write_data_structured_field -> tn5250_display_wordwrap).
//
// This is a focused port: it fills each field of the group (fieldlen columns per
// line) word by word, right-padding unused cells with the word-wrap space (0x00)
// and clearing any trailing group fields, then sets each written field's MDT. The
// elaborate cursor-repositioning tn5250's wordwrap also performs is out of scope
// on this path (the Write-To-Display end logic positions the cursor); a word
// longer than the field width is hard-split so nothing is lost.
func (d *display) wordwrap(text []byte, f *field) {
	fieldlen := f.length
	if fieldlen <= 0 {
		return
	}

	// Collect the consecutive word-wrap fields starting at f. In a well-formed
	// stream the field after the group's last field is not word-wrap; the
	// circular-list guard stops us if that invariant does not hold.
	var group []*field
	for iter := f; iter != nil && iter.wordwrap; iter = iter.next {
		group = append(group, iter)
		if iter.next == f {
			break
		}
	}

	// Decode the payload to local text; the word-wrap space (0x00) reads as blank.
	var sb strings.Builder
	for _, b := range text {
		if b == 0x00 {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteRune(d.cmap.toLocalRune(b))
	}
	words := strings.Fields(sb.String())

	// Greedily pack words into lines no wider than fieldlen.
	var lines []string
	cur := ""
	for _, w := range words {
		// Hard-split a word wider than the field.
		for len([]rune(w)) > fieldlen {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			rw := []rune(w)
			lines = append(lines, string(rw[:fieldlen]))
			w = string(rw[fieldlen:])
		}
		switch {
		case cur == "":
			cur = w
		case len([]rune(cur))+1+len([]rune(w)) <= fieldlen:
			cur = cur + " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}

	// Write each line into successive group fields; clear any remaining ones.
	for i, fld := range group {
		data := d.fieldData(fld)
		for j := range data {
			data[j] = 0x00
		}
		if i < len(lines) {
			for j, r := range []rune(lines[i]) {
				if j >= len(data) {
					break
				}
				data[j] = d.cmap.toRemoteRune(r)
			}
		}
		fld.setMDT(d.buffers)
	}
}
