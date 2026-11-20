// Copyright 2026 by moshix. All rights reserved.

package tn5250

// wtdContext is the Go port of lib5250's Tn5250WTDContext (wtd.c). It walks a
// display buffer and reconstructs the Write-To-Display command/order stream a
// host would have sent to produce it, run-length-encoding blank runs with RA
// orders. We only need the "no source" path (full reconstruction), which the
// session uses to answer Save Screen. Window/menu/scrollbar reconstruction is
// out of scope for v1, so those branches are omitted (our buffers never hold
// them).
type wtdContext struct {
	buffer *[]byte  // output stream, appended to
	dst    *dbuffer // buffer being reconstructed

	y, x int // current scan position

	raCount   int // run-length accumulator for RA optimisation
	raChar    byte
	clearUnit bool
}

func newWTDContext(buf *[]byte, dst *dbuffer) *wtdContext {
	return &wtdContext{buffer: buf, dst: dst}
}

func (c *wtdContext) setIC(y, x int) { c.y, c.x = y, x }

// convert reconstructs the whole buffer (tn5250_wtd_context_convert_nosrc).
func (c *wtdContext) convert() {
	c.putc(ESC)
	c.putc(CmdRestoreScreen)

	// We don't know the prior state, so clear the unit.
	c.putc(ESC)
	if c.dst.width() != 80 {
		c.putc(CmdClearUnitAlternate)
		c.putc(0x00)
	} else {
		c.putc(CmdClearUnit)
	}
	c.clearUnit = true

	c.putc(ESC)
	c.putc(CmdWriteToDisplay)
	c.putc(0x00) // CC1
	c.putc(0x00) // CC2

	if len(c.dst.headerData) != 0 {
		c.putc(OrderSOH)
		c.putc(byte(len(c.dst.headerData)))
		for _, b := range c.dst.headerData {
			c.putc(b)
		}
	}

	// Insert-cursor address, so a restore returns to the right field.
	c.putc(OrderIC)
	c.putc(byte(c.y))
	c.putc(byte(c.x))

	for c.y = 0; c.y < c.dst.height(); c.y++ {
		for c.x = 0; c.x < c.dst.width(); c.x++ {
			ch := c.dst.charAt(c.y, c.x)
			if f := c.peekField(); f != nil {
				c.writeField(f, ch)
			} else {
				c.raPutc(ch)
			}
		}
	}

	// Drop the trailing RA run without flushing. The original convert returned
	// here with the accumulator still pending (the final null run is never
	// emitted — the screen was cleared to null at the top, so a Restore already
	// has those cells blank). We must discard it explicitly now: otherwise the
	// GUI emit below would flush it, and at end-of-scan c.y/c.x are (height,width),
	// producing an out-of-range SBA that setBufferAddress rejects (skipping the
	// rest of the record and losing the GUI constructs).
	c.raCount = 0

	// Re-emit the enhanced (GUI) constructs so a Restore Screen replays and
	// re-parses them, mirroring gold wtd.c convert_nosrc which hit-tests the
	// window/menubar lists and re-emits write_cwsf / write_dsfsf. Each retained
	// WDSF byte-sequence is prefixed with an SBA to the position it was created
	// at (its stored 0-based row/col) so the replay's cursor matches. Reading the
	// live dbuffer lists means removed/replaced constructs are correctly dropped.
	for _, w := range c.dst.guiWindows {
		c.emitGUIConstruct(w.row, w.col, w.raw)
	}
	for _, sel := range c.dst.guiSelections {
		c.emitGUIConstruct(sel.row, sel.col, sel.raw)
	}
	for _, sb := range c.dst.guiScrollbars {
		c.emitGUIConstruct(sb.row, sb.col, sb.raw)
	}
}

// emitGUIConstruct writes an SBA to (row,col) (0-based, emitted 1-based) followed
// by a retained WDSF order byte-sequence.
func (c *wtdContext) emitGUIConstruct(row, col int, raw []byte) {
	if len(raw) == 0 {
		return
	}
	c.putc(OrderSBA)
	c.putc(byte(row + 1))
	c.putc(byte(col + 1))
	for _, b := range raw {
		c.putc(b)
	}
}

// peekField returns the field that _starts_ at the next screen position, or nil
// (tn5250_wtd_context_peek_field).
func (c *wtdContext) peekField() *field {
	nx, ny := c.x, c.y
	nx++
	if nx == c.dst.width() {
		ny++
		if ny == c.dst.height() {
			return nil
		}
		nx = 0
	}
	f := c.dst.fieldYX(ny, nx)
	if f == nil {
		return nil
	}
	if f.startRow != ny || f.startCol != nx {
		return nil
	}
	return f
}

func (c *wtdContext) putc(b byte) {
	c.raFlush()
	*c.buffer = append(*c.buffer, b)
}

func (c *wtdContext) raPutc(b byte) {
	if c.raChar != b {
		c.raFlush()
	}
	c.raChar = b
	c.raCount++
}

func (c *wtdContext) raFlush() {
	if c.raCount == 0 {
		return
	}
	// Decide between repeating the bytes and emitting an RA/SBA order.
	if c.raCount <= 4 && !(c.raCount == 3 && c.raChar == 0x00 && c.clearUnit) {
		for c.raCount > 0 {
			*c.buffer = append(*c.buffer, c.raChar)
			c.raCount--
		}
	} else if c.clearUnit && c.raChar == 0x00 {
		*c.buffer = append(*c.buffer, OrderSBA, byte(c.y+1), byte(c.x+1))
	} else {
		px, py := c.x, c.y
		if px == 0 {
			px = c.dst.width() - 1
			py--
		} else {
			px--
		}
		*c.buffer = append(*c.buffer, OrderRA, byte(py+1), byte(px+1), c.raChar)
	}
	c.raCount = 0
}

// writeField emits an SF order and the field's format (tn5250_wtd_context_write_field).
func (c *wtdContext) writeField(f *field, attr byte) {
	c.putc(OrderSF)

	if f.ffw != 0 {
		c.putc(byte(f.ffw >> 8))
		c.putc(byte(f.ffw & 0x00ff))
	}

	if f.continuous {
		c.putc(0x86)
		if f.continuedFirst {
			c.putc(0x01)
		}
		if f.continuedMiddle {
			c.putc(0x03)
		}
		if f.continuedLast {
			c.putc(0x02)
		}
	}
	if f.wordwrap {
		c.putc(0x86)
		c.putc(0x80)
	}
	if f.nextFieldProgressionID != 0 {
		c.putc(0x88)
		c.putc(byte(f.nextFieldProgressionID))
	}

	// Screen attribute (see wtd.c for why we sometimes prefer the field's own).
	if attr&0xe0 != 0x20 {
		c.putc(f.attribute)
	} else {
		c.putc(attr)
	}

	c.putc(byte(f.length >> 8))
	c.putc(byte(f.length & 0x00ff))
}
