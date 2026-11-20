// Copyright 2026 by moshix. All rights reserved.

package tn5250

// gui.go implements the 5250 enhanced (GUI) Write-To-Display structured fields:
// Create Window (0x51), Define Selection Field (0x50) and Define Scroll Bar
// Field (0x53), plus the matching REMOVE constructs. It ports the parsing side
// of session.c (tn5250_session_create_window_structured_field,
// tn5250_session_define_selection_field / _item, tn5250_session_define_scrollbar
// and the remove_gui_* handlers) and the little model objects window.c / menu.c /
// scrollbar.c carry.
//
// These constructs are ADDITIVE overlays: they never touch the character grid or
// the field list, so the ordinary (non-GUI) screen path is byte-for-byte
// unchanged. The models are stored on the dbuffer and surfaced through the
// Snapshot so the browser can draw the window border, the selection choices and
// a scroll-bar indicator on top of the normal cell render.

import "strings"

// ── models (window.c / menu.c / scrollbar.c) ────────────────────────────

// guiWindow is a 5250 window (Create Window, WDSF 0x51). row/col are the 0-based
// screen position of the window's top-left BORDER corner (== the cursor position
// when the Create Window order arrived, matching window->row/column in the C).
// width/height are the interior size in columns/rows; the drawn box is therefore
// (height+2) rows by (width+2) columns. border holds the eight decoded border
// runes in the order the data stream carries them.
type guiWindow struct {
	id       int
	row, col int
	width    int
	height   int
	border   [8]rune // UL, top, UR, left, right, LL, bottom, LR

	// raw is the exact WDSF order byte-sequence that created this window
	// (OrderWDSF, len hi/lo, 0xD9, type, payload). Save Screen replays it,
	// SBA-prefixed to row/col, so Restore Screen recreates the overlay
	// (gold wtd.c convert_nosrc re-emits write_cwsf/write_dsfsf).
	raw []byte
}

// guiSelectionItem is one choice of a selection field (a "menu item" in menu.c).
// row/col are the 0-based screen position of the choice text; size is the text
// length plus the two attribute characters that bracket it (menuitem->size).
type guiSelectionItem struct {
	row, col  int
	text      string
	size      int
	selected  bool
	available bool
}

// guiSelectionField is a Define Selection Field (WDSF 0x50). row/col are the
// 0-based top-left of the field (== the cursor position, like menubar->row/column
// in the C, which are stored 0-based). fieldType is the selection-field type byte
// (0x01 menu bar, 0x11/0x12 single/multiple choice selection field, …).
type guiSelectionField struct {
	id        int
	row, col  int
	fieldType byte
	itemSize  int
	items     []guiSelectionItem

	raw []byte // WDSF order bytes, replayed by Save/Restore Screen (see guiWindow.raw)
}

// guiScrollbar is a Define Scroll Bar Field (WDSF 0x53). total is the number of
// scrollable rows/columns, sliderPos the slider position within that total, and
// size the scroll bar length in characters.
type guiScrollbar struct {
	id         int
	row, col   int
	horizontal bool
	total      int
	sliderPos  int
	size       int

	raw []byte // WDSF order bytes, replayed by Save/Restore Screen (see guiWindow.raw)
}

// defaultWindowBorder returns the box-drawing runes used when the host does not
// send an explicit border character (or sends a control byte).
func defaultWindowBorder() [8]rune {
	return [8]rune{'┌', '─', '┐', '│', '│', '└', '─', '┘'}
}

// setBorderRune decodes an EBCDIC border byte and, unless it is null or a control
// character, stores it as the border rune (leaving the box-drawing default in
// place otherwise).
func setBorderRune(dst *rune, b byte, cmap *charMap) {
	if b == 0 {
		return
	}
	r := cmap.toLocalRune(b)
	if r < 0x20 {
		return
	}
	*dst = r
}

// ── dbuffer storage helpers ─────────────────────────────────────────────

// addOrReplaceWindow stores a window, replacing an existing one anchored at the
// same corner so a host that redefines a window in place does not accumulate
// duplicates.
func (d *dbuffer) addOrReplaceWindow(w *guiWindow) {
	for i, ex := range d.guiWindows {
		if ex.row == w.row && ex.col == w.col {
			w.id = ex.id
			d.guiWindows[i] = w
			return
		}
	}
	w.id = len(d.guiWindows)
	d.guiWindows = append(d.guiWindows, w)
}

func (d *dbuffer) addOrReplaceSelection(s *guiSelectionField) {
	for i, ex := range d.guiSelections {
		if ex.row == s.row && ex.col == s.col {
			s.id = ex.id
			d.guiSelections[i] = s
			return
		}
	}
	s.id = len(d.guiSelections)
	d.guiSelections = append(d.guiSelections, s)
}

func (d *dbuffer) addOrReplaceScrollbar(s *guiScrollbar) {
	for i, ex := range d.guiScrollbars {
		if ex.row == s.row && ex.col == s.col {
			s.id = ex.id
			d.guiScrollbars[i] = s
			return
		}
	}
	s.id = len(d.guiScrollbars)
	d.guiScrollbars = append(d.guiScrollbars, s)
}

// removeWindowAt removes the window whose drawn box contains (y,x); if none does,
// it removes the most recently added window (mirroring the C, which hit-tests at
// the cursor and otherwise just drops a window from the list).
func (d *dbuffer) removeWindowAt(y, x int) {
	for i, w := range d.guiWindows {
		if y >= w.row && y <= w.row+w.height+1 &&
			x >= w.col && x <= w.col+w.width+1 {
			d.guiWindows = append(d.guiWindows[:i], d.guiWindows[i+1:]...)
			return
		}
	}
	if n := len(d.guiWindows); n > 0 {
		d.guiWindows = d.guiWindows[:n-1]
	}
}

// clearGUI drops every GUI construct. Called from the full display clear (Clear
// Unit / Clear Unit Alternate) — it must NOT run on a mere format-table clear,
// where windows survive.
func (d *dbuffer) clearGUI() {
	d.guiWindows = nil
	d.guiSelections = nil
	d.guiScrollbars = nil
}

// copyGUI deep-enough-copies the GUI lists for save/restore-screen. Elements are
// replaced (never mutated in place), so sharing the pointers is safe.
func (d *dbuffer) copyGUI() (w []*guiWindow, s []*guiSelectionField, sb []*guiScrollbar) {
	if d.guiWindows != nil {
		w = append([]*guiWindow(nil), d.guiWindows...)
	}
	if d.guiSelections != nil {
		s = append([]*guiSelectionField(nil), d.guiSelections...)
	}
	if d.guiScrollbars != nil {
		sb = append([]*guiScrollbar(nil), d.guiScrollbars...)
	}
	return
}

// ── parsers (session.c) ─────────────────────────────────────────────────

// parseCreateWindow ports tn5250_session_create_window_structured_field. length
// is the SF payload length (bytes after class+type). The caller re-syncs the
// record cursor past the whole payload afterwards, so this only needs to extract
// the geometry and border characters; reads are still bounded to `length` so a
// truncated record cannot spill into the next order before that re-sync.
func (s *session) parseCreateWindow(length int, raw []byte) {
	cmap := s.display.cmap
	// g reads the next payload byte, or 0 once the payload is exhausted.
	g := func() byte {
		if length <= 0 {
			return 0
		}
		length--
		return s.rec.getByte()
	}

	_ = g() // flagbyte1 (0x80 restrict cursor, 0x40 pull-down) — not needed to draw
	_ = g() // reserved
	_ = g() // reserved
	depth := int(g())
	width := int(g())

	win := &guiWindow{
		row:    s.display.cursorY(),
		col:    s.display.cursorX(),
		width:  width,
		height: depth,
		border: defaultWindowBorder(),
		raw:    raw,
	}

	// Optional border minor structure. The first byte is a self-inclusive length.
	if length > 0 {
		borderLen := int(g()) - 1 // exclude the length byte itself
		var borderType byte
		if borderLen > 0 {
			borderType = g()
			borderLen--
		}
		if borderType == 0x01 {
			// Border Presentation Minor Structure: flags, mono attr, color attr,
			// then up to eight border characters: UL, top, UR, left, right, LL,
			// bottom, LR (session.c create_window border_* order).
			var b [11]byte
			for i := 0; i < 11 && borderLen > 0; i++ {
				b[i] = g()
				borderLen--
			}
			setBorderRune(&win.border[0], b[3], cmap)  // upper-left
			setBorderRune(&win.border[1], b[4], cmap)  // top
			setBorderRune(&win.border[2], b[5], cmap)  // upper-right
			setBorderRune(&win.border[3], b[6], cmap)  // left
			setBorderRune(&win.border[4], b[7], cmap)  // right
			setBorderRune(&win.border[5], b[8], cmap)  // lower-left
			setBorderRune(&win.border[6], b[9], cmap)  // bottom
			setBorderRune(&win.border[7], b[10], cmap) // lower-right
		}
		// Title/footer (0x10) and any trailing minor structures are consumed by
		// the caller's re-sync.
	}

	s.display.dbuf().addOrReplaceWindow(win)
}

// parseSelectionField ports tn5250_session_define_selection_field: a 16-byte
// header followed by minor structures, where type 0x10 is a choice (menu item).
func (s *session) parseSelectionField(length int, raw []byte) {
	cmap := s.display.cmap
	g := func() byte {
		if length <= 0 {
			return 0
		}
		length--
		return s.rec.getByte()
	}

	_ = g() // flagbyte1 (mouse / auto-enter / auto-select)
	_ = g() // flagbyte2 (0x80 scroll bar — unsupported inside a selection field)
	_ = g() // flagbyte3
	fieldType := g()
	for i := 0; i < 5; i++ {
		_ = g() // reserved
	}
	itemSize := int(g())
	_ = g() // height (rows)
	_ = g() // number of choices — we count the items we actually parse
	_ = g() // padding
	_ = g() // separator
	_ = g() // selection character
	_ = g() // cancel AID
	// 16 header bytes consumed.

	sel := &guiSelectionField{
		row:       s.display.cursorY(),
		col:       s.display.cursorX(),
		fieldType: fieldType,
		itemSize:  itemSize,
		raw:       raw,
	}

	// Minor structures: [len][type][data…], len self-inclusive of len+type.
	for length > 0 {
		minorLen := int(g()) - 2
		if minorLen < 0 {
			minorLen = 0
		}
		mtype := g()
		if mtype == 0x10 {
			item := parseSelectionItem(g, minorLen, itemSize, cmap)
			positionSelectionItem(sel, &item)
			sel.items = append(sel.items, item)
		} else {
			// Choice-presentation / separator / indicator structures: skip.
			for i := 0; i < minorLen; i++ {
				_ = g()
			}
		}
	}

	s.display.dbuf().addOrReplaceSelection(sel)
}

// parseSelectionItem ports tn5250_session_define_selection_item. It consumes
// exactly minorLen payload bytes via g (which also advances the field-level
// counter), so the caller's minor-structure loop stays aligned.
func parseSelectionItem(g func() byte, minorLen, itemSize int, cmap *charMap) guiSelectionItem {
	n := minorLen
	read := func() byte {
		if n <= 0 {
			return 0
		}
		n--
		return g()
	}

	flag1 := read()
	_ = read() // flagbyte2 (cursor / roll behaviour)
	flag3 := read()

	var item guiSelectionItem
	// Choice state lives in flagbyte1 bits 0xC0.
	switch {
	case flag1&0x40 != 0:
		item.selected, item.available = true, true // default (selected) choice
	case flag1&0x80 != 0:
		item.selected, item.available = false, false // not available
	default:
		item.available = true // available, not selected
	}
	offsetIncl := flag1&0x08 != 0 // mnemonic offset present
	aidIncl := flag1&0x04 != 0    // AID-if-selected present
	selectIncl := flag1&0x03 != 0 // numeric selection character(s) present

	// The first three bits of flagbyte3 select which device the structure targets;
	// all off means "ignore this minor structure".
	if flag3&0xE0 == 0 {
		for n > 0 {
			read()
		}
		return item
	}

	if offsetIncl {
		read()
	}
	if aidIncl {
		read()
	}
	if selectIncl {
		read()
	}

	runes := make([]rune, 0, itemSize)
	for i := 0; i < itemSize && n > 0; i++ {
		b := read()
		if b == 0 {
			continue
		}
		runes = append(runes, cmap.toLocalRune(b))
	}
	item.text = strings.TrimRight(string(runes), " ")

	// Drain any remaining bytes of this minor structure.
	for n > 0 {
		read()
	}
	return item
}

// positionSelectionItem assigns a choice its screen row/col, following
// tn5250_menuitem_new_row / _new_col: menu-bar choices flow horizontally, every
// other selection-field type stacks one choice per row.
func positionSelectionItem(sel *guiSelectionField, item *guiSelectionItem) {
	item.size = len([]rune(item.text)) + 2 // text + two attribute characters
	if len(sel.items) == 0 {
		item.row = sel.row
		item.col = sel.col + 1
		return
	}
	prev := sel.items[len(sel.items)-1]
	if sel.fieldType == 0x01 { // menu bar
		item.row = prev.row
		item.col = prev.col + prev.size + 1
	} else { // selection field / list / push buttons
		item.row = prev.row + 1
		item.col = sel.col + 1
	}
}

// parseScrollbar ports tn5250_session_define_scrollbar. The total and slider
// position are transmitted as four decimal digits (thousands…units).
func (s *session) parseScrollbar(length int, raw []byte) {
	g := func() byte {
		if length <= 0 {
			return 0
		}
		length--
		return s.rec.getByte()
	}

	flag1 := g()
	_ = g() // reserved
	t1, t2, t3, t4 := int(g()), int(g()), int(g()), int(g())
	total := 1000*t1 + 100*t2 + 10*t3 + t4
	p1, p2, p3, p4 := int(g()), int(g()), int(g()), int(g())
	slider := 1000*p1 + 100*p2 + 10*p3 + p4
	size := int(g())

	sb := &guiScrollbar{
		row:        s.display.cursorY(),
		col:        s.display.cursorX(),
		horizontal: flag1&0x80 != 0,
		total:      total,
		sliderPos:  slider,
		size:       size,
		raw:        raw,
	}
	s.display.dbuf().addOrReplaceScrollbar(sb)
}

// ── remove handlers (session.c remove_gui_*) ────────────────────────────

func (s *session) removeGUIWindow() {
	db := s.display.dbuf()
	db.removeWindowAt(s.display.cursorY(), s.display.cursorX())
	db.guiScrollbars = nil // the C drops scroll bars with the window
}

func (s *session) removeGUISelection() {
	s.display.dbuf().guiSelections = nil
}

func (s *session) removeGUIScrollbar() {
	s.display.dbuf().guiScrollbars = nil
}

func (s *session) removeAllGUI() {
	s.display.dbuf().clearGUI()
}
