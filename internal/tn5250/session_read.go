// Copyright 2026 by moshix. All rights reserved.

package tn5250

import "fmt"

// session_read.go ports the inbound side of session.c: building and sending the
// field data record for a read/AID (send_fields / send_field), the AID-key
// dispatch (handle_aidkey), and the 5250 Query reply (write_structured_field /
// query_reply).

// sendFields builds and transmits the inbound record for the current read
// opcode (tn5250_session_send_fields). After sending it locks the keyboard and
// sets X SYSTEM, matching the C.
func (s *session) sendFields(aidcode int) {
	x := s.display.cursorX()
	y := s.display.cursorY()
	db := s.display.dbuf()

	// Leading row+1, col+1, aid.
	buf := []byte{byte(y + 1), byte(x + 1), byte(aidcode)}

	switch s.readOpcode {
	case CmdReadInputFields:
		if db.masterMDT && db.sendDataForAIDKey(aidcode) {
			s.forEachField(db, func(f *field) { s.sendField(&buf, f) })
		}
	case CmdReadImmediate:
		if db.masterMDT {
			s.forEachField(db, func(f *field) { s.sendField(&buf, f) })
		}
	case CmdReadMDTFields, CmdReadMDTFieldsAlt, CmdReadImmediateAlt:
		if db.sendDataForAIDKey(aidcode) {
			s.forEachField(db, func(f *field) {
				if f.mdt() {
					s.sendField(&buf, f)
				}
			})
		}
	default:
		// Not in a read; nothing to send.
	}

	s.readOpcode = 0
	s.display.indicatorSet(indXSystem)
	s.display.keystate = KeyStateLocked
	s.display.indicatorClear(indInsert)

	_ = s.st.sendPacket(flowDisplay, recHNone, opPutGet, buf)
}

// forEachField walks the circular field list in screen order.
func (s *session) forEachField(db *dbuffer, fn func(*field)) {
	f := db.fieldList
	if f == nil {
		return
	}
	for {
		fn(f)
		f = f.next
		if f == db.fieldList {
			return
		}
	}
}

// sendField appends one field's data to the inbound buffer in the layout the
// current read opcode requires (tn5250_session_send_field).
func (s *session) sendField(buf *[]byte, f *field) {
	size := f.length
	data := s.display.fieldData(f)
	cmap := s.display.cmap

	// Continued (word-wrap) fields: the FIRST subfield carries the whole field;
	// middle/last subfields are folded into it and not sent on their own. We
	// reconstruct the full field by concatenating every continuous subfield from
	// the first through the last, in order (tn5250_session_send_field,
	// session.c:487-520).
	if f.continuous {
		if !f.continuedFirst {
			return
		}
		size = f.length
		for iter := f.next; iter != nil && iter.continuous && iter != f; iter = iter.next {
			size += iter.length
			if iter.continuedLast {
				break
			}
		}
		data = make([]byte, 0, size)
		for iter := f; iter != nil && iter.continuous; iter = iter.next {
			data = append(data, s.display.fieldData(iter)...)
			if iter.continuedLast || iter.next == f {
				break
			}
		}
	}

	switch s.readOpcode {
	case CmdReadInputFields, CmdReadImmediate:
		if f.isSignedNum() {
			for n := 0; n < size-1; n++ {
				*buf = append(*buf, blankIfNull(data[n]))
			}
			c := data[size-2]
			if cmap.local(data[size-1]) == '-' {
				*buf = append(*buf, 0xd0|(0x0f&c))
			} else {
				*buf = append(*buf, c)
			}
		} else {
			for n := 0; n < size; n++ {
				*buf = append(*buf, blankIfNull(data[n]))
			}
		}

	case CmdReadMDTFields, CmdReadMDTFieldsAlt, CmdReadImmediateAlt:
		*buf = append(*buf, OrderSBA, byte(f.startRow+1), byte(f.startCol+1))

		c := data[size-1]
		if f.isSignedNum() {
			size--
			if size > 0 {
				c = data[size-1]
			} else {
				c = 0
			}
			if size > 1 && data[size] == cmap.remote('-') &&
				isDigit(int(cmap.local(c))) {
				c = 0xd0 | (0x0f & c)
			}
		}

		// Strip trailing NULs.
		for size > 0 && data[size-1] == 0 {
			size--
			if size > 0 {
				c = data[size-1]
			} else {
				c = 0
			}
		}

		// For Read MDT Fields, leading/embedded NULs become blanks; the
		// 'Alternate' commands leave them as-is.
		for n := 0; n < size-1; n++ {
			if s.readOpcode != CmdReadMDTFields {
				*buf = append(*buf, data[n])
			} else {
				*buf = append(*buf, blankIfNull(data[n]))
			}
		}
		if size > 0 {
			if s.readOpcode != CmdReadMDTFields {
				*buf = append(*buf, c)
			} else {
				*buf = append(*buf, blankIfNull(c))
			}
		}
	}
}

func blankIfNull(b byte) byte {
	if b == 0 {
		return 0x40
	}
	return b
}

// handleAIDKey dispatches an AID key (tn5250_session_handle_aidkey). Returns
// once the inbound record has been queued.
func (s *session) handleAIDKey(key int) {
	switch key {
	case AIDPrint, AIDRecBackspace:
		buf := []byte{
			byte(s.display.cursorY() + 1),
			byte(s.display.cursorX() + 1),
			byte(key),
		}
		_ = s.st.sendPacket(flowDisplay, recHNone, opNoOp, buf)

	case aidSysReq:
		s.display.indicatorSet(indXSystem)
		if s.display.keystate == KeyStateUnlocked {
			s.display.keystate = KeyStateLocked
		}
		_ = s.st.sendPacket(flowDisplay, recHSRQ, opNoOp, nil)
		s.display.indicatorClear(indXSystem)
		if s.display.keystate == KeyStateLocked {
			s.display.keystate = KeyStateUnlocked
		}

	case aidTestReq:
		_ = s.st.sendPacket(flowDisplay, recHTRQ, opNoOp, nil)

	case aidAttn:
		// K_ATTENTION (display.c ~1468): Attn is a recovery key. tn5250 does
		// tn5250_display_uninhibit(This); This->keystate = UNLOCKED; BEFORE
		// dispatching the ATTN AID, so a pending host inhibit ("X II") is cleared
		// and the message line the host overwrote is restored (uninhibit ->
		// indicatorClear restores savedMsgLine), then it unconditionally sends.
		s.display.uninhibit()
		s.display.keystate = KeyStateUnlocked
		s.display.indicatorSet(indXSystem)
		if s.display.keystate == KeyStateUnlocked {
			s.display.keystate = KeyStateLocked
		}
		_ = s.st.sendPacket(flowDisplay, recHATN, opNoOp, nil)
		s.display.indicatorClear(indXSystem)
		if s.display.keystate == KeyStateLocked {
			s.display.keystate = KeyStateUnlocked
		}

	case AIDHelp:
		if s.display.keystate == KeyStatePreHelp {
			src := []byte{byte(s.display.keySRC >> 8), byte(s.display.keySRC)}
			_ = s.st.sendPacket(flowDisplay, recHHLP, opNoOp, src)
			s.display.keystate = KeyStatePostHelp
			return
		}
		s.sendFields(key)

	default:
		s.sendFields(key)
	}
}

// writeStructuredField validates a WSF command and answers a Query
// (tn5250_session_write_structured_field).
func (s *session) writeStructuredField() {
	var temp [5]byte
	for i := range temp {
		temp[i] = s.rec.getByte()
	}
	if temp[2] != 0xD9 {
		s.sendError(nrInvalidSFClassType)
		return
	}
	switch temp[3] {
	case WSFDefineAuditWindowTable, WSFDefineCommandKeyFunc, WSFReadTextScreen,
		WSFDefinePendingOperations, WSFDefineTextScreenFormat, WSFDefineScaleTime,
		WSFWriteTextScreen, WSFDefineSpecialChars, WSFPendingData,
		WSFDefineOperatorErrorMsgs, WSFDefinePitchTable, WSFDefineFakeDPCmdKeyFunc,
		WSFPassThrough, WSF5250Query, WSF5250QueryStationState:
		// recognised
	default:
		s.sendError(nrInvalidSFClassType)
		return
	}
	s.queryReply()
}

// queryReply builds and sends the 5250 Query reply advertising our station
// capabilities (tn5250_session_query_reply). We advertise the enhanced 5250 GUI
// interface so the host drives windows, selection fields and scroll bars through
// the WDSF constructs we now render (see gui.go), instead of falling back to
// plain-character emulation.
func (s *session) queryReply() {
	var temp [67]byte
	cmap := s.display.cmap

	temp[0] = 0x00 // cursor row/col
	temp[1] = 0x00
	temp[2] = 0x88 // inbound WSF aid
	temp[3] = 0x00 // length hi
	temp[4] = 0x40 // length lo (enhanced: 0x40 => 67-byte reply)
	temp[5] = 0xD9 // command class
	temp[6] = 0x70 // query
	temp[7] = 0x80 // flag byte

	temp[8] = 0x06 // controller hardware class
	temp[9] = 0x00
	temp[10] = 0x01 // controller code level (V1R1.0)
	temp[11] = 0x01
	temp[12] = 0x00
	// bytes 13..28 reserved zero

	temp[29] = 0x01 // display emulation

	// Device type/model, from TERM, as EBCDIC digits.
	devType, devModel := parseDeviceTypeModel(s.termType)
	copy(temp[30:], fmt.Sprintf("%04d", devType))
	temp[34] = 0
	copy(temp[35:], fmt.Sprintf("%02d", devModel))
	for i := 30; i <= 36; i++ {
		temp[i] = cmap.remote(temp[i])
	}

	temp[37] = 0x02 // standard keyboard id
	temp[38] = 0x00
	temp[39] = 0x00

	temp[40] = 0x00 // display serial number
	temp[41] = 0x61
	temp[42] = 0x50
	temp[43] = 0x00

	temp[44] = 0xff // max input fields
	temp[45] = 0xff

	temp[46] = 0x00
	temp[47] = 0x00
	temp[48] = 0x00

	temp[49] = 0x23 // controller/display capability
	temp[50] = 0x31
	temp[51] = 0x00
	temp[52] = 0x00
	// Enhanced features, matching the C's enhanced query_reply:
	//   byte 53 bit 6 (0x02): Enhanced 5250 FCW & WDSFs — tells the host it may
	//                         send Create Window / Define Selection Field / Define
	//                         Scroll Bar Field (and the REMOVE constructs).
	//   byte 54 bit 0 (0x80): enhanced user-interface support level 2.
	// This is a single coarse "enhanced 5250" switch (there are no per-construct
	// bits). It is safe here because writeDisplayStructuredField now parses AND
	// consumes every GUI WDSF, keeping the byte stream synchronized, and the
	// browser draws the visible ones (window border, selection choices, scroll
	// bar); the non-visual ones (mouse buttons, grid lines) are simply consumed.
	temp[53] = 0x02
	temp[54] = 0x80
	// bytes 55..66 zero

	_ = s.st.sendPacket(flowDisplay, recHNone, opNoOp, temp[:int(temp[4])+3])
}

// parseDeviceTypeModel splits a TERM like "IBM-3179-2" into type 3179, model 2.
func parseDeviceTypeModel(term string) (devType, devModel int) {
	// Skip to the character after the first '-'.
	i := 0
	for i < len(term) && term[i] != '-' {
		i++
	}
	if i < len(term) {
		i++
	}
	rest := term[i:]
	devType = atoiPrefix(rest)
	devModel = 1
	for j := 0; j < len(rest); j++ {
		if rest[j] == '-' {
			devModel = atoiPrefix(rest[j+1:])
			break
		}
	}
	return devType, devModel
}

// atoiPrefix parses the leading run of decimal digits, like C's atoi.
func atoiPrefix(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
