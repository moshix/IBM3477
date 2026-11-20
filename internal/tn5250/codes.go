// Copyright 2026 by moshix. All rights reserved.

// Package tn5250 is a pure-Go implementation of the IBM 5250 telnet protocol
// (TN5250 / TN5250E), ported faithfully from the GNU tn5250 C library
// (lib5250). It lets a program speak the 5250 data stream to an AS/400 (IBM i)
// host: negotiating the telnet options, parsing the 5250 command/order stream
// into a display buffer with fields, and building the input responses sent back
// when the operator presses an AID key.
//
// The port mirrors lib5250's structure: this file corresponds to codes5250.h
// (protocol constants), and the sibling files map to their C counterparts
// (telnet.go<-telnetstr.c, dbuffer.go<-dbuffer.c, field.go<-field.c,
// display.go<-display.c, wtd.go<-wtd.c, session.go<-session.c, ebcdic.go<-the
// translation tables in utility.c/transmaps.h).
package tn5250

// ── Misc ────────────────────────────────────────────────────────────────
const (
	ESC    = 0x04 // 5250 escape byte that introduces every command
	SOHLEN = 7    // valid Start-of-Header length
	ErrRow = 25   // the error/message row (1-based) on a 24-row display
)

// ── Commands (codes5250.h) ──────────────────────────────────────────────
// A 5250 record is a sequence of ESC + command. These are the command bytes.
const (
	CmdClearUnit               = 0x40
	CmdClearUnitAlternate      = 0x20
	CmdClearFormatTable        = 0x50
	CmdWriteToDisplay          = 0x11
	CmdWriteErrorCode          = 0x21
	CmdWriteErrorCodeWindow    = 0x22
	CmdReadInputFields         = 0x42
	CmdReadMDTFields           = 0x52
	CmdReadMDTFieldsAlt        = 0x82
	CmdReadScreenImmediate     = 0x62
	CmdReadScreenExtended      = 0x64
	CmdReadScreenPrint         = 0x66
	CmdReadScreenPrintExtended = 0x68
	CmdReadScreenPrintGrid     = 0x6A
	CmdReadScreenPrintExtGrid  = 0x6C
	CmdReadImmediate           = 0x72
	CmdReadImmediateAlt        = 0x83
	CmdSaveScreen              = 0x02
	CmdSavePartialScreen       = 0x03
	CmdRestoreScreen           = 0x12
	CmdRestorePartialScreen    = 0x13
	CmdRoll                    = 0x23
	CmdWriteStructuredField    = 0xF3
)

// ── Orders inside a Write-To-Display command (codes5250.h) ──────────────
const (
	OrderSOH  = 0x01 // Start of header
	OrderRA   = 0x02 // Repeat to address
	OrderEA   = 0x03 // Erase to address (5494)
	OrderTD   = 0x10 // Transparent data (5494)
	OrderSBA  = 0x11 // Set buffer address
	OrderWEA  = 0x12 // Write extended attribute (5494)
	OrderIC   = 0x13 // Insert cursor
	OrderMC   = 0x14 // Move cursor (5494)
	OrderWDSF = 0x15 // Write to display structured field (5494)
	OrderSF   = 0x1D // Start of field
)

// ── Write-to-display structured field types (WDSF, order 0x15) ──────────
const (
	WDSFDefineSelectionField = 0x50
	WDSFCreateWindow         = 0x51
	WDSFUnrestWinCursMove    = 0x52
	WDSFDefineScrollBarField = 0x53
	WDSFWriteData            = 0x54
	WDSFProgrammableMouseBut = 0x55
	WDSFRemGUISelField       = 0x58
	WDSFRemGUIWindow         = 0x59
	WDSFRemGUIScrollBarField = 0x5B
	WDSFRemAllGUIConstructs  = 0x5F
	WDSFDrawEraseGridLines   = 0x60
	WDSFClearGridLineBuffer  = 0x61
)

// ── Write structured field types (WSF, command 0xF3) ────────────────────
const (
	WSFDefineAuditWindowTable  = 0x30
	WSFDefineCommandKeyFunc    = 0x31
	WSFReadTextScreen          = 0x32
	WSFDefinePendingOperations = 0x33
	WSFDefineTextScreenFormat  = 0x34
	WSFDefineScaleTime         = 0x35
	WSFWriteTextScreen         = 0x36
	WSFDefineSpecialChars      = 0x37
	WSFPendingData             = 0x38
	WSFDefineOperatorErrorMsgs = 0x39
	WSFDefinePitchTable        = 0x3A
	WSFDefineFakeDPCmdKeyFunc  = 0x3B
	WSFPassThrough             = 0x3F
	WSF5250Query               = 0x70
	WSF5250QueryStationState   = 0x72
)

// ── Field attribute bytes (codes5250.h; 5494 Functions Ref 15.6.12.3) ───
// Bits 0-2 are always 001 to mark a byte as an attribute. The remaining bits
// select color and display characteristics.
const (
	Attr5250Green   = 0x20 // default
	Attr5250White   = 0x22
	Attr5250NonDisp = 0x27 // nondisplay
	Attr5250Red     = 0x28
	Attr5250Turq    = 0x30
	Attr5250Yellow  = 0x32
	Attr5250Pink    = 0x38
	Attr5250Blue    = 0x3A

	Attr5250Normal = Attr5250Green
)

// ── Operator error codes (5494 User's Guide GA27-3960-03 2.3.4) ─────────
const (
	ErrDontKnow       = 0x01
	ErrBypassField    = 0x04
	ErrNoField        = 0x05
	ErrInvalidSysReq  = 0x06
	ErrMandatoryEntry = 0x07
	ErrAlphaOnly      = 0x08
	ErrNumericOnly    = 0x09
	ErrDigitsOnly     = 0x10
	ErrLastSigned     = 0x11
	ErrNoRoom         = 0x12
	ErrMandatoryFill  = 0x14
	ErrCheckDigit     = 0x15
	ErrNotSigned      = 0x16
	ErrExitNotValid   = 0x18
	ErrDupNotEnabled  = 0x19
	ErrNoFieldExit    = 0x20
	ErrNoInput        = 0x26
	ErrBadChar        = 0x27
)

// errorMessages maps an operator error code to the message shown on the error
// row, matching the MSG_* strings in codes5250.h.
var errorMessages = map[int]string{
	ErrDontKnow:       "Keyboard overrun.",
	ErrBypassField:    "Entry of data not allowed in this input/output field.",
	ErrNoField:        "Cursor in protected area of display.",
	ErrInvalidSysReq:  "Key pressed following System Request key was not valid.",
	ErrMandatoryEntry: "Mandatory data entry field. Must have data entered.",
	ErrAlphaOnly:      "Field requires alphabetic characters.",
	ErrNumericOnly:    "Field requires numeric characters.",
	ErrDigitsOnly:     "Only characters 0 through 9 allowed.",
	ErrLastSigned:     "Key for sign position of field not valid.",
	ErrNoRoom:         "No room to insert data.",
	ErrMandatoryFill:  "Mandatory fill field. Must fill to exit.",
	ErrCheckDigit:     "Modulo 10 or 11 check digit error.",
	ErrNotSigned:      "Field Minus key not valid in field.",
	ErrExitNotValid:   "The key used to exit field not valid.",
	ErrDupNotEnabled:  "Duplicate key or Field Mark key not allowed in field.",
	ErrNoFieldExit:    "Enter key not allowed in field.",
	ErrNoInput:        "Field- entry not allowed.",
	ErrBadChar:        "Cannot use undefined key.",
}

// ── Data-stream negative responses (SC30-3533-04 13.4) ──────────────────
const (
	DSNRResEqErr   = 0x03
	DSNRInvCursPos = 0x22
	DSNRRAB4WSA    = 0x23
	DSNRInvSFA     = 0x26
	DSNRFldEOD     = 0x28
	DSNRFmtOvf     = 0x29
	DSNRWrtEOD     = 0x2A
	DSNRSOHLen     = 0x2B
	DSNRRollParm   = 0x2C
	DSNRNoEsc      = 0x31
	DSNRInvWECW    = 0x32
	DSNRUnknown    = -1
)

// ── Keyboard / error handling states (codes5250.h) ──────────────────────
const (
	KeyStateUnlocked = 0
	KeyStateLocked   = 1
	KeyStateHardware = 2
	KeyStatePreHelp  = 3
	KeyStatePostHelp = 4
)

// Keyboard source (SRC) codes reported to the host / shown in the OIA.
const (
	KBDSrcNone           = 0x0000
	KBDSrcInvalidCmd     = 0x0003
	KBDSrcDataDisallowed = 0x0004
	KBDSrcProtect        = 0x0005
	KBDSrcAlphaOnly      = 0x0008
	KBDSrcNumOnly        = 0x0009
	KBDSrcOnly09         = 0x0010
	KBDSrcSignPos        = 0x0011
	KBDSrcNoRoom         = 0x0012
	KBDSrcFldMDisallowed = 0x0016
	KBDSrcFER            = 0x0018
	KBDSrcDupDisallowed  = 0x0019
)

// ── Field Format Word bits (byte one; field.h) ──────────────────────────
const (
	FieldBypass     = 0x2000
	FieldDupEnable  = 0x1000
	FieldModified   = 0x0800
	FieldAlphaShift = 0x0000 // 000
	FieldAlphaOnly  = 0x0100 // 100
	FieldNumShift   = 0x0200 // 010
	FieldNumOnly    = 0x0300 // 011
	FieldKataShift  = 0x0400 // 001
	FieldDigitOnly  = 0x0500 // 101
	FieldMagReader  = 0x0600 // 110
	FieldSignedNum  = 0x0700 // 111
	FieldTypeMask   = 0x0700 // 111
)

// ── Field Format Word bits (byte two; field.h) ──────────────────────────
const (
	FieldAutoEnter     = 0x0080
	FieldFER           = 0x0040 // field exit required
	FieldMonocase      = 0x0020
	FieldMandatory     = 0x0008
	FieldNoAdjust      = 0x0000
	FieldRightZero     = 0x0005 // right-adjust, zero fill
	FieldRightBlank    = 0x0006 // right-adjust, blank fill
	FieldMandatoryFill = 0x0007
	FieldMandFillMask  = 0x0007
)

// Field Control Word (FCW) high-byte patterns (field.h).
const (
	FieldControlHighlightedEntry = 0x8900
	FieldControlContinued        = 0x8600
)

// ── Key codes (terminal.h) ──────────────────────────────────────────────
// These match the K_* values in lib5250's terminal.h. They flow from the UI
// through the display's key queue; AID-producing keys become 5250 AID bytes
// (see aidForKey in session.go). Values that coincide with curses key codes are
// preserved exactly, since lib5250 relies on them being unique.
const (
	KEnter      = 0x0d
	KNewLine    = 0x200
	KTab        = 0x09
	KBacktab    = 0541
	KF1         = 0410 + 1
	KF2         = 0410 + 2
	KF3         = 0410 + 3
	KF4         = 0410 + 4
	KF5         = 0410 + 5
	KF6         = 0410 + 6
	KF7         = 0410 + 7
	KF8         = 0410 + 8
	KF9         = 0410 + 9
	KF10        = 0410 + 10
	KF11        = 0410 + 11
	KF12        = 0410 + 12
	KF13        = 0410 + 13
	KF14        = 0410 + 14
	KF15        = 0410 + 15
	KF16        = 0410 + 16
	KF17        = 0410 + 17
	KF18        = 0410 + 18
	KF19        = 0410 + 19
	KF20        = 0410 + 20
	KF21        = 0410 + 21
	KF22        = 0410 + 22
	KF23        = 0410 + 23
	KF24        = 0410 + 24
	KLeft       = 0404
	KRight      = 0405
	KUp         = 0403
	KDown       = 0402
	KRollDn     = 0523
	KRollUp     = 0522
	KBackspace  = 0407
	KHome       = 0406
	KEnd        = 0550
	KInsert     = 0513
	KDelete     = 0512
	KReset      = 0531
	KPrint      = 0532
	KHelp       = 0553
	KSysReq     = 0401
	KClear      = 0515
	KRefresh    = 0564
	KFieldExit  = 0517
	KTestReq    = 0516
	KToggle     = 0533
	KErase      = 0514
	KAttention  = 0511
	KDuplicate  = 0524
	KFieldMinus = 0526
	KFieldPlus  = 0520
	KPrevWord   = 0611
	KNextWord   = 0622
	KPrevFld    = 0xfffb
	KNextFld    = 0xfffc
	KFieldHome  = 0607
	KExec       = 0507
	KMemo       = 0510
	KCopyText   = 0xfffd
	KPasteText  = 0xfffe
	KUnknown    = 0xffff
)

// ── AID (Attention Identifier) bytes sent to the host ───────────────────
// These are the 5250 AID codes placed at the head of an inbound record when
// the operator presses a function/command key (session.c).
const (
	AIDNone         = 0x00
	AIDEnter        = 0xF1
	AIDHelp         = 0xF3
	AIDRollDown     = 0xF4 // page up
	AIDRollUp       = 0xF5 // page down
	AIDPrint        = 0xF6
	AIDRecBackspace = 0xF8
	AIDCmd07        = 0xF7 // (unused placeholder to keep table readable)
	AIDPF1          = 0x31
	AIDPF2          = 0x32
	AIDPF3          = 0x33
	AIDPF4          = 0x34
	AIDPF5          = 0x35
	AIDPF6          = 0x36
	AIDPF7          = 0x37
	AIDPF8          = 0x38
	AIDPF9          = 0x39
	AIDPF10         = 0x3A
	AIDPF11         = 0x3B
	AIDPF12         = 0x3C
	AIDPF13         = 0xB1
	AIDPF14         = 0xB2
	AIDPF15         = 0xB3
	AIDPF16         = 0xB4
	AIDPF17         = 0xB5
	AIDPF18         = 0xB6
	AIDPF19         = 0xB7
	AIDPF20         = 0xB8
	AIDPF21         = 0xB9
	AIDPF22         = 0xBA
	AIDPF23         = 0xBB
	AIDPF24         = 0xBC
	AIDClear        = 0xBD
	AIDFieldExit    = 0x00 // field exit is handled locally, not an AID
)
