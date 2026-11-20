// Copyright 2026 by moshix. All rights reserved.

package tn5250

// session_orders.go ports the Write-To-Display order handlers from session.c:
// SF (start of field), SOH (start of header), SBA, RA, EA, IC, MC, TD, WEA. The
// GUI structured-field orders (WDSF sub-types) are consumed but not acted on,
// per the v1 scope.

// startOfField parses an SF order and creates/updates a field
// (tn5250_session_start_of_field).
func (s *session) startOfField() {
	var ffw1, ffw2 byte
	inputField := false

	resequence := 0
	var magstripe, lightpen, magAndLight, lightAndAttn, forwardEdge bool
	var ideoOnly, ideoDataType, ideoEither, ideoOpen bool
	transparency := 0
	continuous := false
	contFirst, contMiddle, contLast := false, false, false
	progressionID := 0
	var highlightEntryAttr, pointerAID byte
	selfCheckMod11, selfCheckMod10 := false, false

	cur := s.rec.getByte()

	if cur&0xe0 != 0x20 {
		// Input field: lock keyboard, read FFW then any FCWs.
		s.display.indicatorSet(indXSystem)
		if s.display.keystate == KeyStateUnlocked {
			s.display.keystate = KeyStateLocked
		}
		inputField = true
		ffw1 = cur
		ffw2 = s.rec.getByte()
		cur = s.rec.getByte()

		continuous, contFirst, contMiddle, contLast = false, false, false, false
		progressionID = 0
		for cur&0xe0 != 0x20 {
			// Stop if the record ended before the mandatory attribute byte —
			// getByte() returns 0 past the end, which would otherwise spin here
			// forever on a malformed/truncated SF order.
			if s.rec.isChainEnd() {
				break
			}
			fcw1 := cur
			fcw2 := s.rec.getByte()
			fcw := (uint16(fcw1) << 8) | uint16(fcw2)
			cur = s.rec.getByte()

			switch {
			case fcw1 == 0x80:
				resequence = int(fcw2)
			case fcw == 0x8101:
				magstripe = true
			case fcw == 0x8102:
				lightpen = true
			case fcw == 0x8103:
				magAndLight = true // magnetic stripe + light pen
			case fcw == 0x8106:
				lightAndAttn = true // light pen + attention
			case fcw == 0x8200:
				ideoOnly = true // ideographic (DBCS) only
			case fcw == 0x8220:
				ideoDataType = true // ideographic data type
			case fcw == 0x8240:
				ideoEither = true // ideographic either
			case fcw == 0x8280 || fcw == 0x82C0:
				ideoOpen = true // ideographic open
			case fcw == 0x8501:
				forwardEdge = true // forward-edge trigger
			case fcw1 == 0x84:
				transparency = int(fcw2)
			case fcw == 0x8601:
				continuous, contFirst = true, true
			case fcw == 0x8603:
				continuous, contMiddle = true, true
			case fcw == 0x8602:
				continuous, contLast = true, true
			case fcw == 0x8680:
				s.wordwrapState = true
			case fcw1 == 0x88:
				progressionID = int(fcw2)
			case fcw1 == 0x89:
				highlightEntryAttr = fcw2
			case fcw1 == 0x8A:
				pointerAID = fcw2
			case fcw == 0xB140:
				selfCheckMod11 = true
			case fcw == 0xB1A0:
				selfCheckMod10 = true
			}
			if fcw == 0x8602 && s.wordwrapState {
				s.wordwrapState = false
			}
		}
	} else {
		inputField = false
	}

	// cur now holds the screen attribute byte.
	attr := cur
	s.display.addch(cur)

	length1 := s.rec.getByte()
	length2 := s.rec.getByte()
	length := (int(length1) << 8) | int(length2)

	width := s.display.width()
	height := s.display.height()
	startx := s.display.cursorX() + 1
	starty := s.display.cursorY() + 1

	if (starty-1)*width+startx-1+length > width*height {
		s.sendError(nrInvalidRowColAddr)
		return
	}

	var f *field
	if inputField {
		x := s.display.cursorX()
		y := s.display.cursorY()

		if existing := s.display.fieldAt(y, x); existing != nil {
			// Modify a field we already have at this exact start position.
			if existing.startCol == x && existing.startRow == y {
				existing.ffw = (uint16(ffw1) << 8) | uint16(ffw2)
				existing.attribute = attr
			}
			f = existing
		} else {
			f = newField(width)
			f.ffw = (uint16(ffw1) << 8) | uint16(ffw2)
			f.resequence = resequence
			f.magstripe = magstripe
			f.lightpen = lightpen
			f.transparency = transparency
			f.continuous = continuous
			f.continuedFirst = contFirst
			f.continuedMiddle = contMiddle
			f.continuedLast = contLast
			f.wordwrap = s.wordwrapState
			f.nextFieldProgressionID = progressionID
			f.highlightEntryAttr = highlightEntryAttr
			f.pointerAID = pointerAID
			f.selfCheckMod11 = selfCheckMod11
			f.selfCheckMod10 = selfCheckMod10
			f.magAndLight = magAndLight
			f.lightAndAttn = lightAndAttn
			f.forwardEdge = forwardEdge
			f.ideographicOnly = ideoOnly
			f.ideographicDataType = ideoDataType
			f.ideographicEither = ideoEither
			f.ideographicOpen = ideoOpen
			f.attribute = attr
			f.length = length
			f.startRow = y
			f.startCol = x
			s.display.dbuf().addField(f)
		}

		// Write a trailing attribute (0x20) just past the field, then restore
		// the cursor to the field start — mirrors the C exactly.
		endrow := f.endRow()
		endcol := f.endCol()
		if endcol == width-1 {
			endcol = 0
			if endrow == height-1 {
				endrow = 0
			} else {
				endrow++
			}
		} else {
			endcol++
		}
		s.display.setCursor(endrow, endcol)
		s.display.addch(0x20)
		s.display.setCursor(y, x)
	}
}

// startOfHeader clears the format table and stores the SOH header
// (tn5250_session_start_of_header).
func (s *session) startOfHeader() {
	s.display.dbuf().clearTable()
	s.display.pendingInsert = false
	s.display.indicatorSet(indXSystem)
	if s.display.keystate == KeyStateUnlocked {
		s.display.keystate = KeyStateLocked
	}

	n := int(s.rec.getByte())
	if n < 0 || n > 7 {
		s.sendError(nrInvalidSOHLength)
		return
	}
	var hdr []byte
	for i := 0; i < n; i++ {
		hdr = append(hdr, s.rec.getByte())
	}
	s.display.setHeaderData(hdr)
}

// setBufferAddress positions the cursor (tn5250_session_set_buffer_address).
func (s *session) setBufferAddress() {
	y := int(s.rec.getByte())
	x := int(s.rec.getByte())
	width := s.display.width()
	height := s.display.height()
	if y == 0 || y > height || x == 0 || x > width {
		s.sendError(nrInvalidRowColAddr)
		return
	}
	s.display.setCursor(y-1, x-1)
}

// writeExtendedAttribute validates a WEA order but does not act on it
// (tn5250_session_write_extended_attribute).
func (s *session) writeExtendedAttribute() {
	attrtype := s.rec.getByte()
	_ = s.rec.getByte() // attr
	if attrtype != 0x01 && attrtype != 0x03 && attrtype != 0x05 {
		s.sendError(nrInvalidExtAttrType)
	}
}

// writeDisplayStructuredField dispatches a WDSF order (tn5250_session_write_
// display_structured_field). The GUI constructs Create Window, Define Selection
// Field and Define Scroll Bar Field (and their REMOVE counterparts) are parsed
// into overlay models on the dbuffer; the remaining recognised types are
// consumed. See gui.go for the parsers.
func (s *session) writeDisplayStructuredField() {
	length := (int(s.rec.getByte()) << 8) | int(s.rec.getByte())
	class := s.rec.getByte()
	if class != 0xd9 {
		s.sendError(nrInvalidSFClassType)
		return
	}
	typ := s.rec.getByte()
	origLen := length // the length field as sent, replayed verbatim by Save Screen
	length -= 4
	if length < 0 {
		length = 0
	}

	// Record the byte just past this SF's payload. After parsing we snap the
	// record cursor to exactly there, so the byte stream stays synchronized even
	// if a GUI sub-parser reads a little too much or too little on a malformed
	// record — the ordinary (non-GUI) order stream that follows is never
	// disturbed.
	payloadStart := s.rec.curPos
	end := payloadStart + length
	if end > len(s.rec.data) {
		end = len(s.rec.data)
	}

	// Capture the exact WDSF order byte-sequence (order, length, class, type,
	// payload) so a GUI construct can be re-emitted by Save Screen and re-parsed
	// on Restore (see wtd.go / guiWindow.raw). Only the GUI-construct types below
	// actually keep it; the rest ignore the copy.
	raw := make([]byte, 0, 5+(end-payloadStart))
	raw = append(raw, OrderWDSF, byte(origLen>>8), byte(origLen), 0xd9, byte(typ))
	raw = append(raw, s.rec.data[payloadStart:end]...)

	switch typ {
	case WDSFCreateWindow:
		s.parseCreateWindow(length, raw)
	case WDSFDefineSelectionField:
		s.parseSelectionField(length, raw)
	case WDSFDefineScrollBarField:
		s.parseScrollbar(length, raw)
	case WDSFRemGUIWindow:
		s.removeGUIWindow()
	case WDSFRemGUISelField:
		s.removeGUISelection()
	case WDSFRemGUIScrollBarField:
		s.removeGUIScrollbar()
	case WDSFRemAllGUIConstructs:
		s.removeAllGUI()
	case WDSFWriteData:
		s.writeDataStructuredField(length)
	case WDSFUnrestWinCursMove, WDSFProgrammableMouseBut,
		WDSFDrawEraseGridLines, WDSFClearGridLineBuffer:
		// Recognised but not rendered (unrestricted cursor movement, programmable
		// mouse buttons, grid lines) — consumed below.
	default:
		s.sendError(nrInvalidSFClassType)
		return
	}

	// Re-sync to the first byte after this structured field's payload.
	s.rec.curPos = end
}

// writeDataStructuredField ports tn5250_session_write_data_structured_field
// (session.c:3575). A WRITE-DATA WDSF (0x54) carries a flag byte followed by
// text. When flag-byte-1 bit 0x80 is set and the current field is a word-wrap
// field, the text is word-wrapped into that field (tn5250_display_wordwrap);
// otherwise it is consumed with no effect. length is the SF payload length; the
// caller re-syncs the record cursor past it afterwards, so reads are bounded to
// length.
func (s *session) writeDataStructuredField(length int) {
	field := s.display.currentField()
	g := func() byte {
		if length <= 0 {
			return 0
		}
		length--
		return s.rec.getByte()
	}

	flagbyte1 := g()
	// The remaining payload is the data text (EBCDIC).
	data := make([]byte, 0, length)
	for length > 0 {
		data = append(data, g())
	}

	if flagbyte1&0x80 != 0 && field != nil && field.wordwrap {
		s.display.wordwrap(data, field)
	}
}

// transparentData copies raw bytes to the display (tn5250_session_transparent_data).
func (s *session) transparentData() {
	l1 := s.rec.getByte()
	l2 := s.rec.getByte()
	tdLen := (int(l1) << 8) | int(l2)

	width := s.display.width()
	height := s.display.height()
	curx := s.display.cursorX()
	cury := s.display.cursorY()
	// cury/curx are 0-based here (session.c subtracted 1 from the 0-based
	// row, letting TD near the bottom wrap to row 0 instead of failing).
	if cury*width+curx+tdLen > width*height {
		s.sendError(nrInvalidRowColAddr)
		return
	}
	for tdLen > 0 {
		s.display.addch(s.rec.getByte())
		tdLen--
	}
}

// moveCursor validates an MC order (tn5250_session_move_cursor); like the C it
// only range-checks and does not move the cursor.
func (s *session) moveCursor() {
	// Match the gold exactly (session.c:2024): the raw byte is decremented as an
	// UNSIGNED char (0 -> 255) FIRST, then range-checked. Go byte arithmetic wraps
	// the same way, so getByte()-1 reproduces the C's off-by-one intent: MC to
	// row 1 (byte 1 -> y 0) is an invalid-address error, and MC to row==height is
	// accepted. MC only validates — it never moves the cursor.
	y := int(s.rec.getByte() - 1)
	x := int(s.rec.getByte() - 1)
	width := s.display.width()
	height := s.display.height()
	if y == 0 || y > height || x == 0 || x > width {
		s.sendError(nrInvalidRowColAddr)
	}
}

// insertCursor sets the pending insert cursor (tn5250_session_insert_cursor).
func (s *session) insertCursor() {
	y := int(s.rec.getByte())
	x := int(s.rec.getByte())
	width := s.display.width()
	height := s.display.height()
	if y == 0 || y > height || x == 0 || x > width {
		s.sendError(nrInvalidRowColAddr)
		return
	}
	s.display.setPendingInsert(y-1, x-1)
}

// eraseToAddress blanks from the cursor to an address (tn5250_session_erase_to_address).
func (s *session) eraseToAddress() {
	curx := s.display.cursorX() + 1
	cury := s.display.cursorY() + 1
	y := int(s.rec.getByte())
	x := int(s.rec.getByte())
	length := int(s.rec.getByte())
	width := s.display.width()
	height := s.display.height()
	start := (cury-1)*width + curx
	end := (y-1)*width + x
	if end < start || length < 2 || length > 5 {
		s.sendError(nrInvalidRowColAddr)
		return
	}
	length--
	var attribute int
	for length > 0 {
		attribute = int(s.rec.getByte())
		length--
	}
	// Bound the target row/col to the display BEFORE erasing. tn5250 silently
	// overruns the buffer for an out-of-range EA target; eraseRegion here would
	// index d.data past its end and panic. Clamp y to [1,height] and x to
	// [1,width], mirroring how SBA/IC bound their targets.
	if y < 1 {
		y = 1
	} else if y > height {
		y = height
	}
	if x < 1 {
		x = 1
	} else if x > width {
		x = width
	}
	if attribute == 0xFF {
		s.display.eraseRegion(cury, curx, y, x, 1, width)
	}
	// The current address after EA is the one following the ending address
	// (as after RA), wrapping to the next row and from the last position to
	// the first. y/x are 1-based, so the next address is 0-based (y-1, x);
	// session.c passed the 1-based row as 0-based, one row too low.
	ny, nx := y-1, x
	if nx >= width {
		nx = 0
		ny++
	}
	if ny >= height {
		ny = 0
	}
	s.display.setCursor(ny, nx)
}

// repeatToAddress fills from the cursor to an address with a byte
// (tn5250_session_repeat_to_address).
func (s *session) repeatToAddress() {
	r := s.rec.getByte()
	c := s.rec.getByte()
	ch := s.rec.getByte()

	y := s.display.cursorY() + 1
	x := s.display.cursorX() + 1
	width := s.display.width()
	height := s.display.height()
	start := (y-1)*width + x
	end := (int(r)-1)*width + int(c)

	if r == 0 || int(r) > height || c == 0 || int(c) > width || end < start {
		s.sendError(nrInvalidRowColAddr)
		return
	}

	for {
		yy := s.display.cursorY()
		xx := s.display.cursorX()
		s.display.addch(ch)
		if yy == int(r)-1 && xx == int(c)-1 {
			break
		}
	}
}
