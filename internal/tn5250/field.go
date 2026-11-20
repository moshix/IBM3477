// Copyright 2026 by moshix. All rights reserved.

package tn5250

// field is the Go port of lib5250's Tn5250Field (field.c / field.h). It manages
// one input/output field on the display. It does NOT hold the field's character
// data — that lives in the display buffer; the field only records its position,
// length, format word (FFW) and the assorted field-control-word (FCW) flags.
//
// Fields are kept in a doubly-linked circular list on the dbuffer, exactly as in
// the C, because the 5250 read/aid logic walks that list in screen order.
type field struct {
	next, prev *field

	id      int // sequential id assigned when added to the dbuffer
	entryID int

	// Format Field Word: color/shift/type in the high byte, adjust/flags in the
	// low byte. See the Field* constants in codes.go.
	ffw uint16

	attribute byte // the screen attribute byte that introduces this field
	startRow  int  // row of the first DATA position (after the attribute)
	startCol  int  // col of the first DATA position
	length    int
	w         int // display width, needed for position arithmetic

	// FCW-derived flags (parsed in start_of_field).
	resequence             int
	magstripe              bool
	lightpen               bool
	magAndLight            bool // FCW 0x8103: magnetic stripe + light pen
	lightAndAttn           bool // FCW 0x8106: light pen + attention
	transparency           int
	continuous             bool
	continuedFirst         bool
	continuedMiddle        bool
	continuedLast          bool
	wordwrap               bool
	nextFieldProgressionID int
	highlightEntryAttr     byte
	pointerAID             byte
	selfCheckMod11         bool
	selfCheckMod10         bool
	forwardEdge            bool // FCW 0x8501: forward-edge trigger
	// Ideographic (DBCS) field flags (FCW 0x8200/0x8220/0x8240/0x8280/0x82C0).
	// Parsed for fidelity with tn5250; full double-byte rendering/entry is not
	// implemented, so these are recorded but not yet acted upon.
	ideographicOnly     bool // 0x8200
	ideographicDataType bool // 0x8220
	ideographicEither   bool // 0x8240
	ideographicOpen     bool // 0x8280 / 0x82C0
}

// newField mirrors tn5250_field_new: zeroed, id -1, remembering the display
// width for later position math.
func newField(w int) *field { return &field{id: -1, w: w} }

// ── position helpers (field.c) ──────────────────────────────────────────

func (f *field) startPos() int { return f.startRow*f.w + f.startCol }
func (f *field) endPos() int   { return f.startPos() + f.length - 1 }
func (f *field) endRow() int   { return f.endPos() / f.w }
func (f *field) endCol() int   { return f.endPos() % f.w }

// hitTest reports whether screen position (y,x) lies within this field.
func (f *field) hitTest(y, x int) bool {
	pos := y*f.w + x
	return pos >= f.startPos() && pos <= f.endPos()
}

// countRight returns how many characters lie to the right of (y,x) in the field.
func (f *field) countRight(y, x int) int { return f.endPos() - (y*f.w + x) }

// countLeft returns how many characters lie to the left of (y,x) in the field.
func (f *field) countLeft(y, x int) int { return (y*f.w + x) - f.startPos() }

// ── FFW predicates (field.h macros) ─────────────────────────────────────

func (f *field) isBypass() bool    { return f.ffw&FieldBypass != 0 }
func (f *field) isDupEnable() bool { return f.ffw&FieldDupEnable != 0 }
func (f *field) mdt() bool         { return f.ffw&FieldModified != 0 }
func (f *field) clearMDT()         { f.ffw &^= FieldModified }
func (f *field) isAutoEnter() bool { return f.ffw&FieldAutoEnter != 0 }
func (f *field) isFER() bool       { return f.ffw&FieldFER != 0 }
func (f *field) isMonocase() bool  { return f.ffw&FieldMonocase != 0 }
func (f *field) isMandatory() bool { return f.ffw&FieldMandatory != 0 }

func (f *field) fieldType() uint16 { return f.ffw & FieldTypeMask }
func (f *field) isAlphaOnly() bool { return f.fieldType() == FieldAlphaOnly }
func (f *field) isNumShift() bool  { return f.fieldType() == FieldNumShift }
func (f *field) isNumOnly() bool   { return f.fieldType() == FieldNumOnly }
func (f *field) isDigitOnly() bool { return f.fieldType() == FieldDigitOnly }
func (f *field) isSignedNum() bool { return f.fieldType() == FieldSignedNum }
func (f *field) isMagReader() bool { return f.fieldType() == FieldMagReader }

// isNumeric groups the field types the UI should treat as numeric-only input.
func (f *field) isNumeric() bool {
	return f.isNumOnly() || f.isDigitOnly() || f.isSignedNum()
}

// isNonDisplay reports whether the field's screen attribute is nondisplay.
func (f *field) isNonDisplay() bool { return f.attribute == Attr5250NonDisp }

func (f *field) mandFillType() uint16 { return f.ffw & FieldMandFillMask }
func (f *field) isRightZero() bool    { return f.mandFillType() == FieldRightZero }
func (f *field) isRightBlank() bool   { return f.mandFillType() == FieldRightBlank }

// setMDT sets this field's Modified flag and the owning buffer's master MDT.
// Continued (word-wrap) fields propagate the flag to the first sub-field, as in
// tn5250_field_set_mdt.
func (f *field) setMDT(db *dbuffer) {
	if f.continuous && !f.continuedFirst {
		iter := f.prev
		for iter.continuous && !iter.continuedFirst {
			iter = iter.prev
		}
		iter.setMDT(db)
		return
	}
	f.ffw |= FieldModified
	db.masterMDT = true
}

// validChar reports whether ch (a local/ASCII character) is valid for this
// field type, ported from tn5250_field_valid_char. On failure it returns the
// keyboard SRC to report.
func (f *field) validChar(ch int) (ok bool, src int) {
	switch f.fieldType() {
	case FieldAlphaShift:
		return true, KBDSrcNone
	case FieldAlphaOnly:
		if !(isAlpha(ch) || ch == ',' || ch == '.' || ch == '-' || ch == ' ') {
			return false, KBDSrcAlphaOnly
		}
		return true, KBDSrcNone
	case FieldNumShift:
		return true, KBDSrcNone
	case FieldNumOnly:
		if !(isDigit(ch) || ch == ',' || ch == '.' || ch == '-' || ch == ' ') {
			return false, KBDSrcNumOnly
		}
		return true, KBDSrcNone
	case FieldKataShift:
		return true, KBDSrcNone
	case FieldDigitOnly:
		if !isDigit(ch) {
			return false, KBDSrcOnly09
		}
		return true, KBDSrcNone
	case FieldMagReader:
		return false, KBDSrcDataDisallowed
	case FieldSignedNum:
		if !isDigit(ch) {
			return false, KBDSrcOnly09
		}
		return true, KBDSrcNone
	}
	return false, KBDSrcNone
}

func isAlpha(c int) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isDigit(c int) bool { return c >= '0' && c <= '9' }

// ── circular field list (field.c) ───────────────────────────────────────

// fieldListAdd appends node to the tail of a circular list and returns the head.
func fieldListAdd(list, node *field) *field {
	node.prev, node.next = nil, nil
	if list == nil {
		node.next, node.prev = node, node
		return node
	}
	node.next = list
	node.prev = list.prev
	node.prev.next = node
	node.next.prev = node
	return list
}

// fieldListFindByID finds a field by numeric id.
func fieldListFindByID(list *field, id int) *field {
	if list == nil {
		return nil
	}
	iter := list
	for {
		if iter.id == id {
			return iter
		}
		iter = iter.next
		if iter == list {
			return nil
		}
	}
}
