// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"context"
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

	cb UpdateFunc

	// connMu guards the host connection's lifetime state. It is separate from
	// mu on purpose: a reply to the host is written while mu is held, and if
	// the host stops reading that write blocks (until the write deadline).
	// Disconnect must still be able to close the connection - which unblocks
	// the write - so it takes only connMu, never mu. Lock order: mu, then
	// connMu.
	connMu     sync.Mutex
	conn       io.Closer          // the attached connection, closed by Disconnect
	hungUp     bool               // Disconnect was called: Connect must not attach
	cancelDial context.CancelFunc // aborts a dial / TLS handshake in progress

	insert bool // insert mode (operator.go, twinax bridge)

	// fault is the panic an operator action recovered from (guarded); the
	// session was hung up and Run returns it.
	fault error

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

// ErrHungUp is returned by Connect when Disconnect was called before or while
// it was dialing: the user hung up, so the session is never attached.
var ErrHungUp = errors.New("tn5250: disconnected")

// dialTimeout bounds the TCP dial and the TLS handshake together, so a user
// never waits for an OS timeout.
var dialTimeout = 10 * time.Second

// Connect dials host:port (optionally over TLS) and prepares the stream and
// session. Telnet negotiation is answered lazily by the read loop in Run, so
// this returns as soon as the socket is up. A Disconnect before or during the
// dial / TLS handshake aborts it: Connect then returns ErrHungUp and nothing
// is attached.
func (c *Client) Connect(host, port string, useTLS bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	c.connMu.Lock()
	if c.hungUp {
		c.connMu.Unlock()
		return ErrHungUp
	}
	c.cancelDial = cancel
	c.connMu.Unlock()

	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error
	dialer := &net.Dialer{}
	if useTLS {
		// Match sslstream.c's intent: TLS over the same TCP transport.
		td := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true, // AS/400 hosts often use self-signed certs
		}}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}

	c.connMu.Lock()
	c.cancelDial = nil
	hungUp := c.hungUp
	c.connMu.Unlock()
	if err != nil {
		if hungUp {
			return ErrHungUp
		}
		return err
	}
	if !c.attach(conn) {
		_ = conn.Close() // Disconnect came after the dial finished
		return ErrHungUp
	}
	return nil
}

// attach binds a connection to the client (also used by tests with an in-memory
// pipe). It reports false, attaching nothing, when Disconnect was already
// called.
func (c *Client) attach(conn io.ReadWriteCloser) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.hungUp {
		return false
	}
	raw := conn
	conn = &countingConn{ReadWriteCloser: conn, tx: &c.tx, rx: &c.rx}
	c.conn = conn
	c.st = newStream(conn, c.termType, c.codepage, c.deviceName, c.kbdType)
	// Bound every write to the host where the transport supports it (TCP,
	// TLS, net.Pipe): a host that stops reading must not hold c.mu forever.
	if d, ok := raw.(writeDeadliner); ok {
		c.st.deadline = d
	}
	c.sess = newSession(c.disp, c.st, c.termType)
	return true
}

// closeConn closes the attached connection, if any. It needs only connMu, so
// it works while another goroutine holds c.mu blocked in a write.
func (c *Client) closeConn() {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Run is the blocking read loop: it reads full records, feeds them to the
// session, and pushes a Snapshot after each. It returns on disconnect/EOF.
func (c *Client) Run() error {
	if c.st == nil {
		return errors.New("tn5250: not connected")
	}
	// However Run ends (EOF, an oversized record, a recovered panic, a failed
	// write), the session is over: never leave the socket open behind it.
	defer c.closeConn()
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
			c.mu.Lock()
			fault := c.fault
			c.mu.Unlock()
			if fault != nil {
				return fault // hung up after an operator action panicked
			}
			c.connMu.Lock()
			hungUp := c.hungUp
			c.connMu.Unlock()
			if hungUp {
				return nil // the user hung up (a write it cut short is no fault)
			}
			if werr := c.st.writeError(); werr != nil {
				return werr // a write to the host failed or timed out
			}
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

// guarded runs an operator action (typing, a key, a cursor move) with
// c.mu held, as processRecordSafe does for host records: a panic (e.g. a
// field table the host left inconsistent) must end only THIS session, not
// crash the server for every user. On a panic it records the fault, hangs
// up (Run then returns the fault) and reports false; c.mu stays held.
func (c *Client) guarded(fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			c.fault = fmt.Errorf("tn5250: recovered from panic in operator action: %v", r)
			c.closeConn()
			ok = false
		}
	}()
	fn()
	return true
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

// Disconnect closes the connection, unblocking Run. It never takes c.mu, so it
// works even while a write to a host that stopped reading holds it (closing
// the connection fails that write). Called before or during Connect, it aborts
// the dial / TLS handshake and Connect returns ErrHungUp. A Client is one
// session: after Disconnect it cannot connect again.
func (c *Client) Disconnect() {
	c.connMu.Lock()
	c.hungUp = true
	conn, cancel := c.conn, c.cancelDial
	c.connMu.Unlock()
	if cancel != nil {
		cancel()
	}
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

	if !c.guarded(func() {
		c.applyFieldEdits(fields)
		c.setCursorClamped(cursorRow, cursorCol)

		if local {
			c.handleLocalKey(aidName)
		} else if c.sess.readOpcode != 0 || code < 0 {
			// Faithful to display.c do_aidkey: AID keys are ignored outside a
			// read, except the pseudo (negative) codes.
			c.sess.handleAIDKey(code)
		}
	}) {
		err := c.fault
		c.mu.Unlock()
		return err
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
	if !c.guarded(func() { c.sess.handleAIDKey(aidAttn) }) {
		c.mu.Unlock()
		return
	}
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
	case "FieldExit", "FieldPlus", "FieldMinus":
		// Field Exit / Field+ / Field- (display.c kf_field_exit / _plus /
		// _minus): null the field from the cursor to its end, apply the
		// field's adjust (right-adjust zero/blank fill; a signed numeric
		// field is right-adjusted blank-filled, never into its sign
		// position), then leave it. Field- is only valid in a numeric-only
		// or signed numeric field.
		minus := name == "FieldMinus"
		if minus && !f.isNumOnly() && !f.isSignedNum() {
			c.operatorError(KBDSrcFldMDisallowed)
			return
		}
		data := c.disp.fieldData(f)
		if i := f.countLeft(c.disp.cursorY(), c.disp.cursorX()); i >= 0 {
			for ; i < len(data); i++ {
				data[i] = 0
			}
		}
		f.setMDT(c.disp.dbuf())
		switch {
		case f.isSignedNum() && len(data) > 0:
			// The sign position: '-' for Field-, cleared otherwise (Field+
			// undoes an earlier Field-).
			if minus {
				data[len(data)-1] = c.disp.cmap.remote('-')
			} else {
				data[len(data)-1] = 0
			}
			c.disp.fieldAdjust(f)
		case f.isNumOnly():
			// No sign position: the sign is the zone of the low-order digit,
			// D for negative, F for positive (Field+ clears an earlier
			// Field-). It is set after the adjust has moved that digit to
			// its final place.
			c.disp.fieldAdjust(f)
			if name != "FieldExit" {
				zone := byte(0xF0)
				if minus {
					zone = 0xD0
				}
				for i := len(data) - 1; i >= 0; i-- {
					if data[i] == 0 || data[i] == 0x40 {
						continue
					}
					if lo := data[i] & 0x0F; lo <= 9 && (data[i]&0xF0 == 0xF0 || data[i]&0xF0 == 0xD0) {
						data[i] = zone | lo
					}
					break
				}
			}
		default:
			c.disp.fieldAdjust(f)
		}
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
