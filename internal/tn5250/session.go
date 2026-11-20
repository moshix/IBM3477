// Copyright 2026 by moshix. All rights reserved.

package tn5250

// session is the Go port of lib5250's Tn5250Session (session.c): it parses the
// 5250 command/order stream into display-buffer mutations and builds the inbound
// records sent back when the operator presses an AID key. It is driven by the
// client's read loop (processRecord) and by SendAID (handleAIDKey).

// ── record header flow types / flags / opcodes (record.h) ───────────────
const (
	flowDisplay = 0x00
	flowStartup = 0x90

	recHNone = 0x00
	recHERR  = 0x80
	recHATN  = 0x40
	recHSRQ  = 0x04
	recHTRQ  = 0x02
	recHHLP  = 0x01

	opNoOp         = 0
	opInvite       = 1
	opOutputOnly   = 2
	opPutGet       = 3
	opSaveScr      = 4
	opRestoreScr   = 5
	opReadImmed    = 6
	opReadScr      = 8
	opCancelInvite = 10
	opMessageOn    = 11
	opMessageOff   = 12
)

// ── WTD command-control byte 2 flags (session.h) ────────────────────────
const (
	ctlICUlock    = 0x40
	ctlClrBlink   = 0x20
	ctlSetBlink   = 0x10
	ctlUnlock     = 0x08
	ctlAlarm      = 0x04
	ctlMessageOff = 0x02
	ctlMessageOn  = 0x01
)

// ── negative response codes (session.h) ─────────────────────────────────
const (
	nrInvalidCommand      = 0x10030101
	nrInvalidClearUnitAlt = 0x10030105
	nrInvalidSOHLength    = 0x1005012B
	nrInvalidRowColAddr   = 0x10050122
	nrInvalidExtAttrType  = 0x1005012C
	nrInvalidSFClassType  = 0x10050111
)

// ── pseudo AID codes used locally (session.h) ───────────────────────────
const (
	aidSysReq  = -1
	aidAttn    = -2
	aidTestReq = -3
)

type session struct {
	display    *display
	st         *stream
	rec        *record
	readOpcode int
	termType   string

	// start_of_field keeps word-wrap state across SF orders (a static in the C).
	wordwrapState bool
}

func newSession(d *display, st *stream, termType string) *session {
	return &session{display: d, st: st, termType: termType}
}

// ── error / negative response ───────────────────────────────────────────

func (s *session) sendError(errorcode uint32) {
	data := []byte{
		byte(errorcode >> 24), byte(errorcode >> 16),
		byte(errorcode >> 8), byte(errorcode),
	}
	_ = s.st.sendPacket(flowDisplay, recHERR, opNoOp, data)
	s.rec.skipToEnd()
}

// ── top-level record handling (tn5250_session_handle_receive, one record) ──

func (s *session) processRecord(data []byte) {
	s.rec = newRecord(data)
	switch s.rec.opcode() {
	case opPutGet, opInvite:
		s.invite()
	case opOutputOnly:
		s.outputOnly()
	case opCancelInvite:
		s.cancelInvite()
	case opMessageOn:
		s.display.indicatorSet(indMessageWaiting)
		s.display.beep()
	case opMessageOff:
		s.display.indicatorClear(indMessageWaiting)
	case opNoOp, opSaveScr, opRestoreScr, opReadImmed, opReadScr:
		// nothing
	default:
		// Unknown opcode: ignore rather than crash.
	}
	if !s.rec.isChainEnd() {
		s.processStream()
	}
}

func (s *session) invite() {
	s.display.indicatorClear(indXClock)
}

func (s *session) cancelInvite() {
	s.display.indicatorSet(indXClock)
	_ = s.st.sendPacket(flowDisplay, recHNone, opCancelInvite, nil)
}

func (s *session) outputOnly() {
	// Empirically a no-op for display terminals; consume the sys-request params.
	if s.rec.sysRequest() {
		_ = s.rec.getByte()
		_ = s.rec.getByte()
	}
}

// ── command dispatch (tn5250_session_process_stream) ────────────────────

func (s *session) processStream() {
	for !s.rec.isChainEnd() {
		if s.rec.getByte() != ESC {
			// Not an escape: ignore the rest of the record, as the C does.
			return
		}
		cmd := s.rec.getByte()
		switch cmd {
		case CmdClearUnit:
			s.clearUnit()
		case CmdClearUnitAlternate:
			s.clearUnitAlternate()
		case CmdClearFormatTable:
			s.clearFormatTable()
		case CmdWriteToDisplay:
			s.writeToDisplay()
		case CmdWriteErrorCode, CmdWriteErrorCodeWindow:
			s.writeErrorCode(int(cmd))
		case CmdReadInputFields, CmdReadMDTFields, CmdReadMDTFieldsAlt:
			s.readCmd(int(cmd))
		case CmdReadScreenImmediate:
			s.readScreenImmediate()
		case CmdReadScreenExtended, CmdReadScreenPrint, CmdReadScreenPrintExtended,
			CmdReadScreenPrintGrid, CmdReadScreenPrintExtGrid:
			// ignored (out of scope)
		case CmdReadImmediate:
			s.readImmediate()
		case CmdReadImmediateAlt:
			// ignored
		case CmdSaveScreen:
			s.saveScreen()
		case CmdSavePartialScreen:
			s.savePartialScreen()
		case CmdRestoreScreen, CmdRestorePartialScreen:
			// Ignored: the following data is a valid Write-To-Display.
		case CmdRoll:
			s.roll()
		case CmdWriteStructuredField:
			s.writeStructuredField()
		case 0x0a:
			// ignore
		default:
			s.sendError(nrInvalidCommand)
		}
	}
}

// ── clear commands ──────────────────────────────────────────────────────

func (s *session) clearUnit() {
	s.display.clearUnit()
	s.readOpcode = 0
}

func (s *session) clearUnitAlternate() {
	c := s.rec.getByte()
	if c != 0x00 && c != 0x80 {
		s.sendError(nrInvalidClearUnitAlt)
		return
	}
	s.display.clearUnitAlternate()
	s.readOpcode = 0
}

func (s *session) clearFormatTable() {
	s.display.clearFormatTable()
	s.readOpcode = 0
}

// ── Write To Display (tn5250_session_write_to_display) ──────────────────

func (s *session) writeToDisplay() {
	oldX := s.display.cursorX()
	oldY := s.display.cursorY()
	// end_x/end_y remain 0xff throughout, exactly as in the C; the IC/MC orders
	// set the pending insert cursor separately.
	endX, endY := 0xff, 0xff

	cc1 := s.rec.getByte()
	cc2 := s.rec.getByte()
	s.handleCC1(cc1)

	done := false
	for !done {
		if s.rec.isChainEnd() {
			done = true
			continue
		}
		order := s.rec.getByte()
		switch order {
		case OrderWEA:
			s.writeExtendedAttribute()
		case OrderTD:
			s.transparentData()
		case OrderWDSF:
			s.writeDisplayStructuredField()
		case OrderMC:
			s.moveCursor()
		case OrderIC:
			s.insertCursor()
		case OrderEA:
			s.eraseToAddress()
		case OrderRA:
			s.repeatToAddress()
		case OrderSBA:
			s.setBufferAddress()
		case OrderSF:
			s.startOfField()
		case OrderSOH:
			s.startOfHeader()
		case ESC:
			done = true
			s.rec.ungetByte()
		default:
			if isPrintable(order) {
				s.display.addch(order)
			}
			// unknown non-printable orders are ignored (the C asserts)
		}
	}

	willBeUnlocked := cc2&ctlUnlock != 0
	curOpcode := int(s.rec.opcode())
	switch {
	case endY != 0xff && endX != 0xff && cc2&ctlICUlock == 0:
		s.display.setCursor(endY, endX)
	case (willBeUnlocked && cc2&ctlICUlock == 0) || curOpcode == opRestoreScr:
		s.display.setCursorHome()
	default:
		s.display.setCursor(oldY, oldX)
	}

	s.handleCC2(cc2)
}

// handleCC1 applies the first command-control byte (tn5250_session_handle_cc1).
func (s *session) handleCC1(cc1 byte) {
	lockKB := true
	resetNonBypassMDT := false
	resetAllMDT := false
	nullNonBypassMDT := false
	nullNonBypass := false

	switch cc1 & 0xE0 {
	case 0x00:
		lockKB = false
	case 0x40:
		resetNonBypassMDT = true
	case 0x60:
		resetAllMDT = true
	case 0x80:
		nullNonBypassMDT = true
	case 0xA0:
		resetNonBypassMDT = true
		nullNonBypass = true
	case 0xC0:
		resetNonBypassMDT = true
		nullNonBypassMDT = true
	case 0xE0:
		resetAllMDT = true
		nullNonBypass = true
	}

	if lockKB {
		s.display.indicatorSet(indXSystem)
		s.display.keystate = KeyStateLocked
	}

	db := s.display.dbuf()
	if iter := db.fieldList; iter != nil {
		for {
			if !iter.isBypass() {
				if (nullNonBypassMDT && iter.mdt()) || nullNonBypass {
					data := s.display.fieldData(iter)
					for i := range data {
						data[i] = 0
					}
				}
			}
			if resetAllMDT || (resetNonBypassMDT && !iter.isBypass()) {
				iter.clearMDT()
			}
			iter = iter.next
			if iter == db.fieldList {
				break
			}
		}
	}
}

// handleCC2 applies the second command-control byte (tn5250_session_handle_cc2).
func (s *session) handleCC2(cc2 byte) {
	if cc2&ctlMessageOn != 0 {
		s.display.indicatorSet(indMessageWaiting)
	}
	if cc2&ctlMessageOff != 0 && cc2&ctlMessageOn == 0 {
		s.display.indicatorClear(indMessageWaiting)
	}
	if cc2&ctlAlarm != 0 {
		s.display.beep()
	}
	if cc2&ctlUnlock != 0 {
		s.display.indicatorClear(indXSystem)
		if s.display.keystate == KeyStateLocked {
			s.display.keystate = KeyStateUnlocked
		}
	}
}

// ── Write Error Code (tn5250_session_write_error_code) ──────────────────

func (s *session) writeErrorCode(readop int) {
	if readop == CmdWriteErrorCodeWindow {
		_ = s.rec.getByte() // startwin
		_ = s.rec.getByte() // endwin
	}

	endX := s.display.cursorX()
	endY := s.display.cursorY()

	s.display.saveMsgLine()
	s.display.setCursor(s.display.dbuf().msgLine(), 0)

	var tempmsg []byte
	haveIC := false
	for {
		if s.rec.isChainEnd() {
			break
		}
		c := s.rec.getByte()
		if c == ESC {
			s.rec.ungetByte()
			break
		}
		if c == OrderIC {
			haveIC = true
			endY = int(s.rec.getByte()) - 1
			endX = int(s.rec.getByte()) - 1
			continue
		}
		// Every remaining byte is printable message text.
		tempmsg = append(tempmsg, c)
	}
	_ = haveIC

	s.display.setMsgLine(tempmsg)
	// Bound the (possibly WEC-embedded IC) cursor target. A gold IC of 0,0 sets
	// endY/endX to -1 (get_byte()-1), which would panic Go's addch on the next
	// order; the C leaves the cursor at -1,-1 and reads past the buffer under
	// NDEBUG. Clamp so the cursor is always a valid position.
	if endY < 0 {
		endY = 0
	} else if endY > s.display.height()-1 {
		endY = s.display.height() - 1
	}
	if endX < 0 {
		endX = 0
	} else if endX > s.display.width()-1 {
		endX = s.display.width() - 1
	}
	s.display.setCursor(endY, endX)
	s.display.keystate = KeyStatePostHelp
	s.display.inhibit()
}

// ── read commands ───────────────────────────────────────────────────────

func (s *session) readCmd(readop int) {
	// A pending operator error - Write Error Code's message on the error line
	// with input inhibited - persists until the operator presses Reset, as on
	// a real 5250, even when the host follows the WEC with a read (the usual
	// "ESC 21 msg ESC 52 ..." sequence). session.c lost it: the read's CC1
	// sets the keystate to LOCKED, and the read then un-inhibits every
	// LOCKED keyboard, restoring the saved message line. Remember the error
	// state and keep it; without one the read unlocks as before.
	pendingErr := s.display.inhibited()
	errKeystate := s.display.keystate

	cc1 := s.rec.getByte()
	s.handleCC1(cc1)
	cc2 := s.rec.getByte()
	s.handleCC2(cc2)

	s.display.indicatorClear(indXSystem | indXClock)
	switch {
	case pendingErr:
		// Still inhibited; Reset clears it. The read itself releases any
		// host lock (Reset is refused while LOCKED, so keeping LOCKED here
		// would leave the operator no way out).
		if errKeystate == KeyStateLocked {
			errKeystate = KeyStateUnlocked
		}
		s.display.keystate = errKeystate
	case s.display.keystate == KeyStateLocked:
		// A read only uninhibits if we were in a normal locked state.
		s.display.uninhibit()
		s.display.keystate = KeyStateUnlocked
	}
	s.readOpcode = readop
}

func (s *session) readImmediate() {
	old := s.readOpcode
	s.readOpcode = CmdReadImmediate
	s.sendFields(0)
	s.readOpcode = old
}

func (s *session) readScreenImmediate() {
	w := s.display.width()
	h := s.display.height()
	buf := make([]byte, 0, w*h)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			buf = append(buf, s.display.charAt(row, col))
		}
	}
	_ = s.st.sendPacket(flowDisplay, recHNone, opNoOp, buf)
}

// ── Save Screen (tn5250_session_save_screen) ────────────────────────────

func (s *session) saveScreen() {
	var buf []byte
	s.display.makeWTDData(&buf)
	if s.readOpcode != 0 {
		buf = append(buf, ESC, byte(s.readOpcode), 0x00, 0x00)
	}
	_ = s.st.sendPacket(flowDisplay, recHNone, opSaveScr, buf)
}

func (s *session) savePartialScreen() {
	_ = s.rec.getByte() // flagbyte
	_ = s.rec.getByte() // toprow
	_ = s.rec.getByte() // leftcol
	_ = s.rec.getByte() // windepth
	_ = s.rec.getByte() // winwidth
	var buf []byte
	s.display.makeWTDData(&buf)
	if s.readOpcode != 0 {
		buf = append(buf, ESC, byte(s.readOpcode), 0x00, 0x00)
	}
	_ = s.st.sendPacket(flowDisplay, recHNone, opSaveScr, buf)
}

// ── Roll (tn5250_session_roll) ──────────────────────────────────────────

func (s *session) roll() {
	direction := s.rec.getByte()
	top := s.rec.getByte()
	bot := s.rec.getByte()
	lines := int(direction & 0x1f)
	if direction&0x80 == 0 {
		lines = -lines
	}
	if lines == 0 {
		return
	}
	s.display.roll(int(top), int(bot), lines)
}

// isPrintable mirrors tn5250_char_map_printable_p, which accepts everything.
func isPrintable(byte) bool { return true }
