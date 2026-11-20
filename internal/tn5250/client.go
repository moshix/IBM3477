// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client is the outward façade documented in api.go. It wires the telnet stream,
// the session (data-stream parser + inbound builder), and the display together,
// and turns display state into Snapshots for the browser bridge.
type Client struct {
	rows, cols int
	termType   string
	codepage   string
	deviceName string // NEW-ENVIRON DEVNAME; empty => omitted (host auto-assigns)
	kbdType    string // NEW-ENVIRON KBDTYPE; empty => "USB"

	mu   sync.Mutex
	disp *display
	st   *stream
	sess *session

	conn io.ReadWriteCloser
	cb   UpdateFunc

	insert bool // insert mode (operator.go, twinax bridge)

	// tx/rx count the bytes sent to and received from the host
	// (traffic.go)
	tx, rx atomic.Uint64
}

// SetDeviceName sets the DEVNAME sent in TN5250E NEW-ENVIRON negotiation, to
// bind a specific device. Must be called before Connect. An empty name omits
// DEVNAME so the host auto-assigns a virtual device.
func (c *Client) SetDeviceName(name string) { c.deviceName = name }

// SetKbdType sets the KBDTYPE sent in NEW-ENVIRON negotiation (default "USB").
// Must be called before Connect.
func (c *Client) SetKbdType(t string) { c.kbdType = t }

// NewClient creates a client for a named model: "3179-2" (24×80) or "3477-FC"
// (27×132). Unknown models default to 24×80.
func NewClient(model, codepage string) *Client {
	rows, cols := 24, 80
	if isWideModel(model) {
		rows, cols = 27, 132
	}
	termType := "IBM-" + model
	return newClient(rows, cols, termType, codepage)
}

// NewClientCustom creates a client with an explicit screen size.
func NewClientCustom(rows, cols int, codepage string) *Client {
	termType := "IBM-3179-2"
	if cols >= 132 {
		termType = "IBM-3477-FC"
	}
	return newClient(rows, cols, termType, codepage)
}

func newClient(rows, cols int, termType, codepage string) *Client {
	if codepage == "" {
		codepage = "37"
	}
	return &Client{
		rows:     rows,
		cols:     cols,
		termType: termType,
		codepage: codepage,
		disp:     newDisplay(cols, rows, codepage),
	}
}

func isWideModel(model string) bool {
	return strings.Contains(model, "3477") || strings.Contains(model, "132") ||
		strings.Contains(model, "27x132")
}

// SetUpdateCallback registers the function called with a fresh Snapshot after
// each display change.
func (c *Client) SetUpdateCallback(fn UpdateFunc) { c.cb = fn }

func (c *Client) Rows() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disp.height()
}

func (c *Client) Cols() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disp.width()
}

// Connect dials host:port (optionally over TLS) and prepares the stream and
// session. Telnet negotiation is answered lazily by the read loop in Run, so
// this returns as soon as the socket is up.
func (c *Client) Connect(host, port string, useTLS bool) error {
	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error
	// A bounded dial, so a user hanging up never waits for an OS timeout.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if useTLS {
		// Match sslstream.c's intent: TLS over the same TCP transport.
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true, // AS/400 hosts often use self-signed certs
		})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	c.attach(conn)
	return nil
}

// attach binds a connection to the client (also used by tests with an in-memory
// pipe).
func (c *Client) attach(conn io.ReadWriteCloser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn = &countingConn{ReadWriteCloser: conn, tx: &c.tx, rx: &c.rx}
	c.conn = conn
	c.st = newStream(conn, c.termType, c.codepage, c.deviceName, c.kbdType)
	c.sess = newSession(c.disp, c.st, c.termType)
}

// Run is the blocking read loop: it reads full records, feeds them to the
// session, and pushes a Snapshot after each. It returns on disconnect/EOF.
func (c *Client) Run() error {
	if c.st == nil {
		return errors.New("tn5250: not connected")
	}
	// Emit the initial (blank) screen so the UI has something to paint.
	c.emit()

	for {
		rec, err := c.st.readRecord()
		if err == nil && !is5250Record(rec) {
			err = ErrNot5250Host // e.g. a 3270 Erase/Write from a TN3270 server
		}
		if errors.Is(err, ErrNot5250Host) {
			c.Disconnect() // hang up at once: nothing it sends can be shown
			return err
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		snap, perr := c.processRecordSafe(rec)
		if perr != nil {
			return perr
		}
		if c.cb != nil && snap != nil {
			c.cb(snap)
		}
	}
}

// processRecordSafe processes one record under the session lock and converts any
// panic (e.g. from a malformed data stream slipping past a bounds check) into an
// error that ends only THIS session, instead of an unrecovered panic that would
// crash the whole server process. The mutex is released even on panic.
func (c *Client) processRecordSafe(rec []byte) (snap *Snapshot, err error) {
	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		if r := recover(); r != nil {
			err = fmt.Errorf("tn5250: recovered from panic while processing record: %v", r)
		}
	}()
	c.sess.processRecord(rec)
	snap = c.buildSnapshot()
	return snap, nil
}

// Disconnect closes the connection, unblocking Run.
func (c *Client) Disconnect() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// emit builds and delivers a snapshot under the lock.
func (c *Client) emit() {
	c.mu.Lock()
	snap := c.buildSnapshot()
	c.mu.Unlock()
	if c.cb != nil {
		c.cb(snap)
	}
}

// ── SendAID (operator input) ────────────────────────────────────────────

// aidCodeForName maps a UI key name to a 5250 AID code (or pseudo code). The
// second result marks keys handled locally (field editing/navigation) rather
// than sent as an AID.
func aidCodeForName(name string) (code int, local bool, ok bool) {
	switch name {
	case "Enter":
		return AIDEnter, false, true
	case "Help":
		return AIDHelp, false, true
	case "RollUp":
		return AIDRollUp, false, true
	case "RollDown":
		return AIDRollDown, false, true
	case "Print":
		return AIDPrint, false, true
	case "Clear":
		return AIDClear, false, true
	case "SysReq":
		return aidSysReq, false, true
	case "TestReq":
		return aidTestReq, false, true
	case "FieldExit":
		return 0, true, true
	case "FieldPlus":
		return 0, true, true
	case "FieldMinus":
		return 0, true, true
	case "Dup":
		return 0, true, true
	}
	// PF1..PF24
	if strings.HasPrefix(name, "PF") {
		n := atoiPrefix(name[2:])
		if n >= 1 && n <= 12 {
			return AIDPF1 + (n - 1), false, true
		}
		if n >= 13 && n <= 24 {
			return AIDPF13 + (n - 13), false, true
		}
	}
	return 0, false, false
}

// SendAID applies the operator's field edits, positions the cursor, and sends
// the inbound record for the named key (or performs a local field operation).
func (c *Client) SendAID(aidName string, cursorRow, cursorCol int, fields map[int]string) error {
	code, local, ok := aidCodeForName(aidName)
	if !ok {
		return errors.New("tn5250: unknown AID key " + aidName)
	}
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return errors.New("tn5250: not connected")
	}

	c.applyFieldEdits(fields)
	c.setCursorClamped(cursorRow, cursorCol)

	if local {
		c.handleLocalKey(aidName)
	} else if c.sess.readOpcode != 0 || code < 0 {
		// Faithful to display.c do_aidkey: AID keys are ignored outside a read,
		// except the pseudo (negative) codes.
		c.sess.handleAIDKey(code)
	}

	snap := c.buildSnapshot()
	c.mu.Unlock()
	if c.cb != nil {
		c.cb(snap)
	}
	return nil
}

// SendAttn sends the Attention key (a pseudo AID handled directly by the host).
func (c *Client) SendAttn() {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	c.sess.handleAIDKey(aidAttn)
	snap := c.buildSnapshot()
	c.mu.Unlock()
	if c.cb != nil {
		c.cb(snap)
	}
}

// Reset performs the 5250 Error-Reset (K_RESET) on the engine, mirroring
// display.c tn5250_display_do_key's K_RESET case. When the keyboard is not
// host-locked it clears the input-inhibit indicator — uninhibit() restores the
// saved message line the host overwrote with a Write-Error-Code (display.go
// indicatorClear) — and returns the keyboard to the unlocked keystate. Like
// do_key's LOCKED-state gate, Reset is DENIED while the host still holds the
// keyboard (KeyStateLocked, i.e. X SYSTEM awaiting a host response), so the
// operator can't jump ahead of the host.
//
// A snapshot is emitted UNCONDITIONALLY — even in the denied (still-locked)
// case — because tn5250 keeps a single authoritative keystate the operator can
// never desync from. The browser's doReset clears its own lock optimistically;
// re-emitting the authoritative state here re-syncs the frontend, either to the
// cleared inhibit / restored message line (allowed) or back to the still-locked
// X SYSTEM (denied). It is purely local: nothing is transmitted.
func (c *Client) Reset() {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	d := c.disp
	if d.keystate != KeyStateLocked {
		d.uninhibit() // clears indInhibit -> restores savedMsgLine (display.go)
		d.keystate = KeyStateUnlocked
	}
	snap := c.buildSnapshot()
	c.mu.Unlock()
	if c.cb != nil {
		c.cb(snap)
	}
}

// applyFieldEdits writes the operator's UTF-8 field contents into the display
// buffer, translating to EBCDIC and setting each edited field's MDT.
func (c *Client) applyFieldEdits(fields map[int]string) {
	if len(fields) == 0 {
		return
	}
	db := c.disp.dbuf()
	// Index fields by their data start position (SnapField.Addr).
	byAddr := map[int]*field{}
	if db.fieldList != nil {
		f := db.fieldList
		for {
			byAddr[f.startPos()] = f
			f = f.next
			if f == db.fieldList {
				break
			}
		}
	}
	for addr, text := range fields {
		f := byAddr[addr]
		if f == nil {
			continue
		}
		c.setFieldText(f, text)
	}
}

// setFieldText left-justifies text into a field (padding with NULs), sets MDT,
// and applies field adjust for right-adjust/signed fields — the state a host
// expects after the operator exits the field.
func (c *Client) setFieldText(f *field, text string) {
	data := c.disp.fieldData(f)
	for i := range data {
		data[i] = 0
	}
	runes := []rune(text)
	// A numeric-only field (ftype 3) carries a negative sign in the ZONE of its
	// last digit — display.c:1737 zone-flips it: data = (data & 0x0F) | 0xD0. The
	// browser encodes that by writing the digit's EBCDIC negative-overpunch glyph
	// (}=−0, J..R=−1..−9). Translate those glyphs to the zoned-negative byte
	// DIRECTLY here, NOT via toRemoteRune: J..R are invariant EBCDIC letters, but
	// '}' is a variant code point that only maps to 0xD0 on CP037-family pages, so
	// routing it through the code page would corrupt a negative-zero on national
	// pages (273/277/278/280/297/871). In a numeric-only field these glyphs can
	// never be legitimate data (letters and '}' are rejected on input), so this is
	// unambiguous. Signed-num (ftype 7) uses a separate sign cell and is untouched.
	numOnly := f.isNumOnly()
	for i := 0; i < len(data) && i < len(runes); i++ {
		if numOnly {
			if d, ok := negOverpunchDigit(runes[i]); ok {
				data[i] = 0xD0 | d
				continue
			}
		}
		data[i] = c.disp.cmap.toRemoteRune(runes[i])
	}
	f.setMDT(c.disp.dbuf())
	if f.isRightZero() || f.isRightBlank() || f.isSignedNum() {
		c.disp.fieldAdjust(f)
	}
}

// negOverpunchDigit maps an EBCDIC negative-overpunch display glyph to its digit
// value (0..9): '}' = −0, 'J'..'R' = −1..−9. Returns ok=false for anything else.
// Used only in numeric-only fields, where these glyphs are always the browser's
// negative-sign encoding for the last digit (they cannot be valid input there).
func negOverpunchDigit(r rune) (byte, bool) {
	switch r {
	case '}':
		return 0, true
	case 'J', 'K', 'L', 'M', 'N', 'O', 'P', 'Q', 'R':
		return byte(r-'J') + 1, true
	}
	return 0, false
}

func (c *Client) setCursorClamped(row, col int) {
	if row < 0 {
		row = 0
	}
	if row >= c.disp.height() {
		row = c.disp.height() - 1
	}
	if col < 0 {
		col = 0
	}
	if col >= c.disp.width() {
		col = c.disp.width() - 1
	}
	c.disp.setCursor(row, col)
}

// handleLocalKey performs field-editing keys that do not produce a host record.
func (c *Client) handleLocalKey(name string) {
	f := c.disp.currentField()
	if f == nil || f.isBypass() {
		return
	}
	switch name {
	case "FieldExit", "FieldPlus":
		c.disp.fieldAdjust(f)
		if f.isAutoEnter() {
			c.sess.handleAIDKey(AIDEnter)
			return
		}
		c.disp.setCursorNextField()
	case "FieldMinus":
		// For signed numeric, force a negative sign in the last position.
		if f.isSignedNum() {
			data := c.disp.fieldData(f)
			if len(data) > 0 {
				data[len(data)-1] = c.disp.cmap.remote('-')
			}
		}
		c.disp.fieldAdjust(f)
		if f.isAutoEnter() {
			c.sess.handleAIDKey(AIDEnter)
			return
		}
		c.disp.setCursorNextField()
	case "Dup":
		if !f.isDupEnable() {
			return
		}
		i := f.countLeft(c.disp.cursorY(), c.disp.cursorX())
		data := c.disp.fieldData(f)
		for ; i < f.length; i++ {
			data[i] = 0x1c // DUP fill
		}
		f.setMDT(c.disp.dbuf())
		c.disp.fieldAdjust(f)
		c.disp.setCursorNextField()
	}
}

// ── Snapshot production ─────────────────────────────────────────────────

// attrDecode is one entry of the 5250 attribute table, mirroring the
// attribute_map in cursesterm.c.
type attrDecode struct {
	fg         string
	reverse    bool
	underline  bool
	blink      bool
	colsep     bool
	nondisplay bool
	intense    bool
}

// attr5250Table decodes attribute bytes 0x20..0x3F (index = byte-0x20). It is a
// faithful copy of cursesterm.c's attribute_map, with nondisplay entries keeping
// their group's foreground colour so hidden text still carries a colour.
var attr5250Table = [32]attrDecode{
	/* 0x20 */ {fg: "green"},
	/* 0x21 */ {fg: "green", reverse: true},
	/* 0x22 */ {fg: "white", intense: true},
	/* 0x23 */ {fg: "white", reverse: true, intense: true},
	/* 0x24 */ {fg: "green", underline: true},
	/* 0x25 */ {fg: "green", underline: true, reverse: true},
	/* 0x26 */ {fg: "white", underline: true, intense: true},
	/* 0x27 */ {fg: "green", nondisplay: true},
	/* 0x28 */ {fg: "red"},
	/* 0x29 */ {fg: "red", reverse: true},
	/* 0x2A */ {fg: "red", blink: true},
	/* 0x2B */ {fg: "red", blink: true, reverse: true},
	/* 0x2C */ {fg: "red", underline: true},
	/* 0x2D */ {fg: "red", underline: true, reverse: true},
	/* 0x2E */ {fg: "red", underline: true, blink: true},
	/* 0x2F */ {fg: "red", nondisplay: true},
	/* 0x30 */ {fg: "turquoise", colsep: true},
	/* 0x31 */ {fg: "turquoise", colsep: true, reverse: true},
	/* 0x32 */ {fg: "yellow", colsep: true},
	/* 0x33 */ {fg: "yellow", colsep: true, reverse: true},
	/* 0x34 */ {fg: "turquoise", underline: true, colsep: true},
	/* 0x35 */ {fg: "turquoise", underline: true, reverse: true, colsep: true},
	/* 0x36 */ {fg: "yellow", underline: true, colsep: true},
	/* 0x37 */ {fg: "turquoise", nondisplay: true},
	/* 0x38 */ {fg: "pink"},
	/* 0x39 */ {fg: "pink", reverse: true},
	/* 0x3A */ {fg: "blue"},
	/* 0x3B */ {fg: "blue", reverse: true},
	/* 0x3C */ {fg: "pink", underline: true},
	/* 0x3D */ {fg: "pink", underline: true, reverse: true},
	/* 0x3E */ {fg: "blue", underline: true},
	/* 0x3F */ {fg: "pink", nondisplay: true},
}

// toggleReverse flips the "reverse" token in a space-joined highlight string,
// mirroring cursesterm.c's `curs_attr ^= A_REVERSE` for the 0x1F/0x3F blocks.
func toggleReverse(hl string) string {
	had := false
	var out []string
	for _, p := range strings.Fields(hl) {
		if p == "reverse" {
			had = true
		} else {
			out = append(out, p)
		}
	}
	if !had {
		out = append(out, "reverse")
	}
	return strings.Join(out, " ")
}

// applyAttr fills a cell's colour/highlight from a 5250 attribute byte.
func applyAttr(cell *SnapCell, attr byte) {
	idx := int(attr) - 0x20
	if idx < 0 || idx >= 32 {
		idx = 0
	}
	a := attr5250Table[idx]
	cell.Fg = a.fg
	cell.Intense = a.intense
	// Highlights are ADDITIVE, exactly like cursesterm.c which ORs A_REVERSE /
	// A_UNDERLINE / A_BLINK. The frontend applies one CSS class per token. Note
	// tn5250 renders the column-separator (A_VERTICAL) as an underline, so it
	// maps to "underscore" too.
	var hl []string
	if a.reverse {
		hl = append(hl, "reverse")
	}
	if a.underline || a.colsep {
		hl = append(hl, "underscore")
	}
	if a.blink {
		hl = append(hl, "blink")
	}
	cell.Highlight = strings.Join(hl, " ")
	if a.nondisplay {
		cell.Hidden = true
	}
}

// buildSnapshot renders the current display state into a Snapshot. Must be
// called with c.mu held.
func (c *Client) buildSnapshot() *Snapshot {
	d := c.disp
	db := d.dbuf()
	w, h := db.w, db.h

	snap := &Snapshot{
		Rows:      h,
		Cols:      w,
		CursorRow: db.cy,
		CursorCol: db.cx,
		Cells:     make([]SnapCell, w*h),
	}

	// Map each cell to the field that owns it, for protect/numeric/hidden.
	fieldOfCell := make([]*field, w*h)
	if db.fieldList != nil {
		f := db.fieldList
		for {
			start := f.startPos()
			for i := 0; i < f.length; i++ {
				if pos := start + i; pos >= 0 && pos < len(fieldOfCell) {
					fieldOfCell[pos] = f
				}
			}
			f = f.next
			if f == db.fieldList {
				break
			}
		}
	}

	// The active attribute carries across positions (and row boundaries) until
	// the next attribute byte; the screen starts at normal green.
	curAttr := byte(Attr5250Green)
	for pos := 0; pos < w*h; pos++ {
		b := db.data[pos]
		cell := &snap.Cells[pos]

		if isAttribute(b) {
			curAttr = b
			// The attribute byte occupies a screen cell that tn5250 always draws
			// as a plain normal-green blank (cursesterm.c: attribute_map[0] | ' '),
			// regardless of the colour/highlight it establishes for the cells
			// that follow it. So do NOT paint it with its own attribute.
			cell.FieldAttr = true
			cell.Char = ' '
			cell.Fg = "green"
			cell.Protected = true
			continue
		}

		applyAttr(cell, curAttr)
		// Normalize non-printable bytes exactly like cursesterm.c (curses_terminal
		// _update): 0x1F/0x3F render as a reverse-video block (blank with reverse
		// toggled); other control bytes (<0x40) and 0xFF render as a blank; the
		// rest are translated through the code page.
		switch {
		case b == 0:
			cell.Char = 0 // null cell → blank in the browser
		case b == 0x1F || b == 0x3F:
			cell.Char = ' '
			cell.Highlight = toggleReverse(cell.Highlight)
		case b < 0x40 || b == 0xFF:
			cell.Char = ' '
		default:
			cell.Char = d.cmap.toLocalRune(b)
		}

		f := fieldOfCell[pos]
		if f != nil && !f.isBypass() {
			cell.Protected = false
		} else {
			cell.Protected = true
		}
		if f != nil && f.isNonDisplay() {
			cell.Hidden = true
		}
	}

	if db.fieldList != nil {
		f := db.fieldList
		for {
			snap.Fields = append(snap.Fields, SnapField{
				// Addr is the field's first DATA position, exactly as tn5250
				// defines a field start (field->start_row/start_col ==
				// tn5250_field_start_pos). The 5250 attribute byte sits at the
				// position before it. The frontend follows this same convention.
				Addr:      f.startPos(),
				Length:    f.length,
				Protected: f.isBypass(),
				Numeric:   f.isNumeric(),
				Hidden:    f.isNonDisplay(),
				Autoskip:  f.isBypass(),
				Type:      int(f.fieldType() >> 8),
				AutoEnter: f.isAutoEnter(),
				FER:       f.isFER(),
				Monocase:  f.isMonocase(),
				Mandatory: f.isMandatory(),
				DupEnable: f.isDupEnable(),
				Adjust:    int(f.mandFillType()),
				ID:        f.id,
				NextProg:  f.nextFieldProgressionID,
			})
			f = f.next
			if f == db.fieldList {
				break
			}
		}
	}

	// Enhanced (GUI) overlays. These are empty on ordinary screens, so a non-GUI
	// snapshot is unchanged.
	for _, w := range db.guiWindows {
		snap.Windows = append(snap.Windows, SnapWindow{
			Row:    w.row,
			Col:    w.col,
			Width:  w.width,
			Height: w.height,
			Border: string(w.border[:]),
		})
	}
	for _, sf := range db.guiSelections {
		out := SnapSelectionField{Row: sf.row, Col: sf.col, Type: int(sf.fieldType)}
		for _, it := range sf.items {
			out.Items = append(out.Items, SnapSelectionItem{
				Row:       it.row,
				Col:       it.col,
				Text:      it.text,
				Selected:  it.selected,
				Available: it.available,
			})
		}
		snap.Selections = append(snap.Selections, out)
	}
	for _, sb := range db.guiScrollbars {
		snap.Scrollbars = append(snap.Scrollbars, SnapScrollbar{
			Row:        sb.row,
			Col:        sb.col,
			Horizontal: sb.horizontal,
			Total:      sb.total,
			Slider:     sb.sliderPos,
			Size:       sb.size,
		})
	}

	// In 5250 the cursor position is always host-authoritative: at the end of a
	// Write-To-Display, tn5250 homes the cursor to the IC target (if an IC order
	// was given) or the first non-bypass field, and cursesterm.c's renderer
	// unconditionally draws the cursor at db.cx/cy. So every host-produced frame
	// carries a valid cursor the browser must honor — not only when an explicit
	// IC order set pending_insert. (The frontend still de-dupes identical
	// positions so a resent frame doesn't yank the cursor mid-type.)
	snap.CursorSet = true
	// The Home / IC target the browser's Home key should jump to (kf_home).
	snap.HomeRow, snap.HomeCol = d.homeTarget()
	snap.Alarm = d.alarm
	snap.KeyboardLocked = d.indicators&(indXSystem|indInhibit) != 0 ||
		d.keystate == KeyStateLocked
	snap.InsertMode = d.indicators&indInsert != 0
	snap.MessageWait = d.indicators&indMessageWaiting != 0
	snap.ErrorText = d.errorText
	// Distinct input-inhibit states for the OIA (tn5250 col-9: II > CLOCK > SYSTEM).
	snap.Inhibit = d.indicators&indInhibit != 0
	snap.XClock = d.indicators&indXClock != 0
	snap.XSystem = d.indicators&indXSystem != 0

	d.alarm = false // consumed by this frame
	return snap
}
