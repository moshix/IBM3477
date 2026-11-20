// Copyright 2026 by moshix. All rights reserved.

package tn5250

// dbuffer is the Go port of lib5250's Tn5250DBuffer (dbuffer.c / dbuffer.h): the
// w×h grid of EBCDIC bytes that holds the current screen (attribute bytes are
// stored inline), the circular field list, cursor position (cx,cy), the pending
// insert-cursor target (tcx,tcy), the master MDT flag, and the format-table
// header saved from the SOH order. Multiple dbuffers are chained for
// save/restore-screen; that chaining is managed by the display.
type dbuffer struct {
	next, prev *dbuffer

	w, h     int
	cx, cy   int    // cursor position
	tcx, tcy int    // insert-cursor target, for goto_ic / set_ic
	data     []byte // w*h EBCDIC bytes, row-major

	fieldList       *field
	fieldCount      int
	entryFieldCount int
	masterMDT       bool

	headerData []byte // saved SOH header bytes (byte 3 = operator error line)

	// Enhanced (GUI) constructs overlaid on this buffer: 5250 windows, selection
	// fields and scroll bars (see gui.go). They are additive — the character grid
	// and field list are untouched — and are cleared by a full display clear.
	guiWindows    []*guiWindow
	guiSelections []*guiSelectionField
	guiScrollbars []*guiScrollbar
}

// newDBuffer allocates a cleared width×height buffer (tn5250_dbuffer_new).
func newDBuffer(width, height int) *dbuffer {
	d := &dbuffer{w: width, h: height}
	d.data = make([]byte, width*height)
	d.clear()
	return d
}

// copy deep-copies the buffer and its field list (tn5250_dbuffer_copy), used by
// save-screen.
func (d *dbuffer) copy() *dbuffer {
	n := &dbuffer{w: d.w, h: d.h, cx: d.cx, cy: d.cy, tcx: d.tcx, tcy: d.tcy}
	n.data = make([]byte, len(d.data))
	copy(n.data, d.data)
	// Copy the field list preserving order.
	if d.fieldList != nil {
		iter := d.fieldList
		for {
			nf := *iter
			nf.next, nf.prev = nil, nil
			cp := &nf
			n.fieldList = fieldListAdd(n.fieldList, cp)
			iter = iter.next
			if iter == d.fieldList {
				break
			}
		}
	}
	n.fieldCount = d.fieldCount
	n.entryFieldCount = d.entryFieldCount
	if d.headerData != nil {
		n.headerData = append([]byte(nil), d.headerData...)
	}
	n.guiWindows, n.guiSelections, n.guiScrollbars = d.copyGUI()
	return n
}

func (d *dbuffer) width() int  { return d.w }
func (d *dbuffer) height() int { return d.h }

// setSize reallocates and clears the buffer (tn5250_dbuffer_set_size), used by
// Clear Unit (24×80) and Clear Unit Alternate (27×132).
func (d *dbuffer) setSize(rows, cols int) {
	d.w = cols
	d.h = rows
	d.data = make([]byte, rows*cols)
	d.clear()
}

func (d *dbuffer) cursorSet(y, x int) { d.cy, d.cx = y, x }

// clear zeroes the buffer, homes the cursor, and clears the format table.
func (d *dbuffer) clear() {
	for i := range d.data {
		d.data[i] = 0
	}
	d.cx, d.cy = 0, 0
	d.clearTable()
	// A full clear (new buffer / Clear Unit) also wipes every GUI overlay. Note
	// clearTable() deliberately does NOT — windows survive a format-table clear.
	d.clearGUI()
}

// clearTable drops the field list and header (tn5250_dbuffer_clear_table).
func (d *dbuffer) clearTable() {
	d.fieldList = nil
	d.fieldCount = 0
	d.entryFieldCount = 0
	d.masterMDT = false
	d.headerData = nil
}

// addField appends a field, assigning ids exactly as tn5250_dbuffer_add_field.
func (d *dbuffer) addField(f *field) {
	f.id = d.fieldCount
	d.fieldCount++
	d.fieldList = fieldListAdd(d.fieldList, f)
	if !f.continuedMiddle && !f.continuedLast {
		d.entryFieldCount++
	}
	f.entryID = d.entryFieldCount
}

// fieldYX returns the field covering (y,x), or nil.
func (d *dbuffer) fieldYX(y, x int) *field {
	if d.fieldList == nil {
		return nil
	}
	iter := d.fieldList
	for {
		if iter.hitTest(y, x) {
			return iter
		}
		iter = iter.next
		if iter == d.fieldList {
			return nil
		}
	}
}

// firstNonBypass returns the first non-bypass field, or nil.
func (d *dbuffer) firstNonBypass() *field {
	if d.fieldList == nil {
		return nil
	}
	iter := d.fieldList
	for {
		if !iter.isBypass() {
			return iter
		}
		iter = iter.next
		if iter == d.fieldList {
			return nil
		}
	}
}

// fieldData returns the slice of the display buffer holding the field's data.
func (d *dbuffer) fieldData(f *field) []byte {
	start := f.startRow*d.w + f.startCol
	return d.data[start : start+f.length]
}

// ── cursor movement (dbuffer.c) ─────────────────────────────────────────

// right advances the cursor n positions with wrap, as tn5250_dbuffer_right.
func (d *dbuffer) right(n int) {
	d.cx += n
	d.cy += d.cx / d.w
	d.cx = d.cx % d.w
	d.cy = d.cy % d.h
}

func (d *dbuffer) left() {
	d.cx--
	if d.cx == -1 {
		d.cx = d.w - 1
		d.cy--
		if d.cy == -1 {
			d.cy = d.h - 1
		}
	}
}

func (d *dbuffer) gotoIC() { d.cy, d.cx = d.tcy, d.tcx }

// addch writes an EBCDIC byte at the cursor and advances (tn5250_dbuffer_addch).
func (d *dbuffer) addch(c byte) {
	d.data[d.cy*d.w+d.cx] = c
	d.right(1)
}

// ins inserts byte c at the cursor, shifting the field right by shiftcount
// positions (tn5250_dbuffer_ins, non-continued path).
func (d *dbuffer) ins(c byte, shiftcount int) {
	x, y := d.cx, d.cy
	for i := 0; i <= shiftcount; i++ {
		c2 := d.data[y*d.w+x]
		d.data[y*d.w+x] = c
		c = c2
		x++
		if x == d.w {
			x = 0
			y++
		}
	}
	d.right(1)
}

// del deletes the char at the cursor, shifting the field left (tn5250_dbuffer_del,
// non-continued path).
func (d *dbuffer) del(shiftcount int) {
	x, y := d.cx, d.cy
	for i := 0; i < shiftcount; i++ {
		fwdx := x + 1
		fwdy := y
		if fwdx == d.w {
			fwdx = 0
			fwdy++
		}
		d.data[y*d.w+x] = d.data[fwdy*d.w+fwdx]
		x, y = fwdx, fwdy
	}
	d.data[y*d.w+x] = 0x00
}

func (d *dbuffer) setIC(y, x int) { d.tcy, d.tcx = y, x }

func (d *dbuffer) charAt(y, x int) byte { return d.data[y*d.w+x] }

// roll scrolls rows [top,bot] by lines (negative = up), per tn5250_dbuffer_roll.
func (d *dbuffer) roll(top, bot, lines int) {
	if lines == 0 {
		return
	}
	// Clamp the roll window to the buffer. A malformed Roll with top/bot outside
	// [0,h-1] (session.roll passes raw record bytes 0..255) would index d.data
	// out of range and panic. The C silently overruns; we clamp so a bad roll
	// becomes a no-op or partial roll instead of a crash. With top,bot in
	// [0,h-1], the guarded destination index n+lines also stays in range.
	if top < 0 {
		top = 0
	} else if top > d.h-1 {
		top = d.h - 1
	}
	if bot < 0 {
		bot = 0
	} else if bot > d.h-1 {
		bot = d.h - 1
	}
	if top > bot {
		return
	}
	if lines < 0 {
		for n := top; n <= bot; n++ {
			if n+lines >= top {
				for c := 0; c < d.w; c++ {
					d.data[(n+lines)*d.w+c] = d.data[n*d.w+c]
				}
			}
		}
	} else {
		for n := bot; n >= top; n-- {
			if n+lines <= bot {
				for c := 0; c < d.w; c++ {
					d.data[(n+lines)*d.w+c] = d.data[n*d.w+c]
				}
			}
		}
	}
}

// setHeaderData stores the SOH header bytes (tn5250_dbuffer_set_header_data).
func (d *dbuffer) setHeaderData(data []byte) {
	if data == nil {
		d.headerData = nil
		return
	}
	d.headerData = append([]byte(nil), data...)
}

// msgLine picks the operator-error line: header byte 3 (1-based) if present,
// else the last row (tn5250_dbuffer_msg_line).
func (d *dbuffer) msgLine() int {
	l := 1000
	if d.headerData != nil && len(d.headerData) >= 4 {
		l = int(d.headerData[3]) - 1
	}
	if l > d.h-1 || l < 0 {
		l = d.h - 1
	}
	return l
}

// sendDataForAIDKey consults the format-table header's command-key mask to
// decide whether field data should accompany this AID key
// (tn5250_dbuffer_send_data_for_aid_key). Returns true when there is no mask.
func (d *dbuffer) sendDataForAIDKey(k int) bool {
	if d.headerData == nil || len(d.headerData) <= 6 {
		return true
	}
	var b, bit int
	switch k {
	case AIDPF1:
		b, bit = 6, 7
	case AIDPF2:
		b, bit = 6, 6
	case AIDPF3:
		b, bit = 6, 5
	case AIDPF4:
		b, bit = 6, 4
	case AIDPF5:
		b, bit = 6, 3
	case AIDPF6:
		b, bit = 6, 2
	case AIDPF7:
		b, bit = 6, 1
	case AIDPF8:
		b, bit = 6, 0
	case AIDPF9:
		b, bit = 5, 7
	case AIDPF10:
		b, bit = 5, 6
	case AIDPF11:
		b, bit = 5, 5
	case AIDPF12:
		b, bit = 5, 4
	case AIDPF13:
		b, bit = 5, 3
	case AIDPF14:
		b, bit = 5, 2
	case AIDPF15:
		b, bit = 5, 1
	case AIDPF16:
		b, bit = 5, 0
	case AIDPF17:
		b, bit = 4, 7
	case AIDPF18:
		b, bit = 4, 6
	case AIDPF19:
		b, bit = 4, 5
	case AIDPF20:
		b, bit = 4, 4
	case AIDPF21:
		b, bit = 4, 3
	case AIDPF22:
		b, bit = 4, 2
	case AIDPF23:
		b, bit = 4, 1
	case AIDPF24:
		b, bit = 4, 0
	default:
		return true
	}
	return d.headerData[b]&(0x80>>uint(bit)) == 0
}
