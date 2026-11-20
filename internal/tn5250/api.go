// Copyright 2026 by moshix. All rights reserved.

package tn5250

// This file defines the OUTWARD contract of the tn5250 engine: the Snapshot
// value the WebSocket bridge serializes to the browser, and the documented
// Client façade the bridge drives. The engine implementation (telnet, dbuffer,
// field, display, wtd, session, ebcdic) lives in the sibling files and fills in
// the Client methods; nothing outside this package should depend on those
// internals — only on the types and the Client API described here.

// UpdateFunc is invoked from the Client's Run loop whenever the display changes,
// with a fresh immutable Snapshot. The bridge uses it to push screen updates to
// the browser. It must not block for long (the caller offloads the actual
// WebSocket write to another goroutine).
type UpdateFunc func(*Snapshot)

// Snapshot is a complete, self-contained view of the display at one instant —
// everything the browser needs to paint a frame. It is produced by the engine
// and consumed by the bridge; it holds no pointers back into engine state.
type Snapshot struct {
	Rows int
	Cols int

	CursorRow int  // 0-based
	CursorCol int  // 0-based
	CursorSet bool // host explicitly positioned the cursor (Insert Cursor)

	// HomeRow/HomeCol are the tn5250 Home / Insert-Cursor target (kf_home /
	// display.c set_cursor_home): the host's IC position if one was set, else the
	// first non-bypass field, else 0,0. Both are 0-based. The browser's Home key
	// uses these so it homes to the host's IC target instead of blindly jumping
	// to the first unprotected field.
	HomeRow int
	HomeCol int

	Alarm          bool   // sound the alarm this frame
	KeyboardLocked bool   // input inhibited (X SYSTEM or II) — drives kbdRestore
	InsertMode     bool   // insert mode active — "IM"/insert indicator
	MessageWait    bool   // a message is waiting — "MW"
	ErrorText      string // text for the error/message row, "" if none

	// Distinct OIA input-inhibit states (host-driven), so the browser can show
	// the correct tn5250 col-9 indicator instead of a single lumped lock:
	//   Inhibit -> "X II"  (e.g. host Write Error Code)
	//   XClock  -> "X CLOCK"
	//   XSystem -> "X SYSTEM"
	XSystem bool
	XClock  bool
	Inhibit bool

	Cells  []SnapCell  // exactly Rows*Cols, row-major
	Fields []SnapField // input/output field map, in screen order

	// Enhanced (GUI) overlays, empty on an ordinary screen. The browser draws
	// these on top of the cell grid (see gui.go for how they are parsed).
	Windows    []SnapWindow         // 5250 windows (Create Window)
	Selections []SnapSelectionField // selection fields / menu bars
	Scrollbars []SnapScrollbar      // scroll-bar indicators
}

// SnapWindow is one 5250 window. Row/Col are the 0-based screen position of the
// window's top-left border corner; Width/Height are the interior size in
// columns/rows, so the drawn box is (Height+2) by (Width+2). Border holds the
// eight border characters as a string in the order UL, top, UR, left, right, LL,
// bottom, LR.
type SnapWindow struct {
	Row, Col      int
	Width, Height int
	Border        string
}

// SnapSelectionField is one selection field (or menu bar). Row/Col are its 0-based
// top-left; Type is the 5250 selection-field type byte.
type SnapSelectionField struct {
	Row, Col int
	Type     int
	Items    []SnapSelectionItem
}

// SnapSelectionItem is one choice of a selection field. Row/Col are the 0-based
// position of the choice text.
type SnapSelectionItem struct {
	Row, Col  int
	Text      string
	Selected  bool
	Available bool
}

// SnapScrollbar is one scroll-bar indicator. Row/Col are its 0-based anchor;
// Horizontal selects orientation; Total is the scrollable extent, Slider the
// current position within it, and Size the bar length in characters.
type SnapScrollbar struct {
	Row, Col   int
	Horizontal bool
	Total      int
	Slider     int
	Size       int
}

// SnapCell is one character cell. Colors are resolved to lowercase names the
// frontend already understands: "green","white","red","turquoise","yellow",
// "pink","blue" (foreground) and background names, matching web3270's cell
// model. An attribute position renders as a blank but still carries the color
// it establishes for the following cells.
type SnapCell struct {
	Char      rune   // display rune; 0 means a null cell (browser shows blank)
	Fg        string // foreground color name (default "green")
	Bg        string // background color name ("" = default/black)
	Highlight string // "", "underline", "reverse", "blink", "colsep"
	Protected bool   // cell is in a protected/output field
	Hidden    bool   // nondisplay (password) — do not show the character
	Intense   bool   // high intensity / white
	FieldAttr bool   // this position holds a 5250 field attribute byte
}

// SnapField describes one field for the browser's input model. Addr is the
// buffer address (row*Cols+col) of the field's first DATA position (i.e. the
// position just after the attribute byte).
type SnapField struct {
	Addr      int
	Length    int
	Protected bool
	Numeric   bool // numeric-only / digits-only / signed-numeric shift
	Hidden    bool // nondisplay field
	Autoskip  bool // bypass field (cursor skips it)

	// Field-format detail the browser needs to reproduce tn5250's LOCAL editing
	// (tn5250 does this in display.c; web5250 edits client-side). All derived
	// from the field's FFW.
	Type      int  // FFW type 0-7: 0 alpha-shift,1 alpha-only,2 num-shift,3 num-only,4 kata,5 digit-only,6 mag,7 signed-num
	AutoEnter bool // filling the field submits (Enter)
	FER       bool // field-exit-required: must press Field Exit to leave
	Monocase  bool // upper-cases alphabetic input
	Mandatory bool // mandatory-entry field
	DupEnable bool // Dup key allowed in this field
	Adjust    int  // mandatory-fill / right-adjust: 0 none, 5 right-zero, 6 right-blank, 7 mandatory-fill

	// Cursor progression (FCW 0x88): the field's own id, and the id of the field
	// the cursor should advance to next (0 = use physical next field). tn5250
	// honors this on Tab and on field-fill (display.c).
	ID       int
	NextProg int
}

// ── Client façade (implemented in client.go / session.go) ───────────────
//
// The bridge depends ONLY on the following surface. Signatures are fixed:
//
//   func NewClient(model, codepage string) *Client
//   func NewClientCustom(rows, cols int, codepage string) *Client
//   func (c *Client) SetUpdateCallback(fn UpdateFunc)
//   func (c *Client) Connect(host, port string, useTLS bool) error // dial + negotiate
//   func (c *Client) Run() error                                   // read loop until disconnect
//   func (c *Client) Disconnect()
//   func (c *Client) SendAID(aidName string, cursorRow, cursorCol int, fields map[int]string) error
//   func (c *Client) SendAttn()
//   func (c *Client) Rows() int
//   func (c *Client) Cols() int
//
// aidName is one of: "Enter","PF1".."PF24","Help","RollUp","RollDown","Print",
// "SysReq","Clear","FieldExit","FieldPlus","FieldMinus","Dup". Unknown names
// are an error. `fields` maps each modified field's Addr (as in SnapField.Addr)
// to its UTF-8 contents.
