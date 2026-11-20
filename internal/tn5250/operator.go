package tn5250

// Operator-side API for the twinax workstation-controller bridge
// (internal/controller). On twinax, the controller - not the terminal -
// owns the 5250 format table: the terminal only reports key scan codes,
// and the controller types them into fields, enforces field rules, moves
// the cursor and builds the inbound records. web5250 did that editing in
// the browser; here it is done in Go on top of the same engine.

import "unicode"

// ScreenState is the raw 5250 presentation space.
type ScreenState struct {
	Rows, Cols           int
	Data                 []byte // EBCDIC characters and inline attribute bytes (0x20-0x3F), row-major
	CursorRow, CursorCol int
	Inhibited            bool // input inhibited (operator error or host lock)
	Insert               bool // insert mode
	ErrorCode            int  // keyboard error code (SRC) while inhibited by an operator error

	// OIA state for the twinax controller's indicator commands.
	Connected   bool // a host session is attached ("system available")
	MessageWait bool // the host set Message Waiting
	HostLocked  bool // the host holds the keyboard (X SYSTEM), not an operator error
	ErrorRow    int  // the error (message) line, where operator error codes are shown
	// Alarms counts the alarms so far (host "sound alarm" and operator
	// errors); the controller sounds the terminal's alarm for each new one.
	Alarms int
}

// Screen returns a copy of the current presentation space.
func (c *Client) Screen() ScreenState {
	c.mu.Lock()
	defer c.mu.Unlock()
	db := c.disp.dbuf()
	st := ScreenState{
		Rows: db.h, Cols: db.w,
		Data:      append([]byte(nil), db.data...),
		CursorRow: db.cy, CursorCol: db.cx,
		Inhibited: c.disp.inhibited() || c.disp.keystate != KeyStateUnlocked,
		Insert:    c.insert,

		Connected:   c.sess != nil,
		MessageWait: c.disp.indicators&indMessageWaiting != 0,
		HostLocked:  c.disp.indicators&indXSystem != 0 || c.disp.keystate == KeyStateLocked,
		ErrorRow:    db.msgLine(),
		Alarms:      c.disp.alarms,
	}
	if c.disp.inhibited() {
		st.ErrorCode = c.disp.keySRC
	}
	return st
}

// operatorError locks the keyboard with a keyboard error code, as a 5250
// controller does for an invalid keystroke (cleared by Reset).
func (c *Client) operatorError(src int) {
	c.disp.keySRC = src
	c.disp.inhibit()
	c.disp.beep()
}

// unlockedForInput reports whether data keys are accepted; a locked
// keyboard rejects them with an error beep.
func (c *Client) unlockedForInput() bool {
	if c.disp.inhibited() || c.disp.keystate != KeyStateUnlocked {
		c.disp.beep()
		return false
	}
	return true
}

// TypeChar types one character at the cursor, applying the field rules:
// it must be in a non-bypass input field and valid for the field type;
// monocase fields store upper case; insert mode shifts the rest of the
// field right (error if the field is full). The cursor then advances,
// leaving the field at its end (auto-enter fields send Enter, FER fields
// wait for Field Exit).
func (c *Client) TypeChar(r rune) {
	c.withSession(func() {
		if !c.unlockedForInput() {
			return
		}
		d := c.disp
		f := d.currentField()
		if f == nil || f.isBypass() {
			c.operatorError(KBDSrcProtect)
			return
		}
		if f.isMonocase() {
			r = unicode.ToUpper(r)
		}
		if r < 0x80 {
			if ok, src := f.validChar(int(r)); !ok {
				c.operatorError(src)
				return
			}
		}
		e := d.cmap.toRemoteRune(r)
		data := d.fieldData(f)
		i := f.countLeft(d.cursorY(), d.cursorX())
		if i < 0 || i >= len(data) {
			return
		}
		if c.insert {
			last := data[len(data)-1]
			if last != 0x00 && last != 0x40 {
				c.operatorError(KBDSrcNoRoom)
				return
			}
			copy(data[i+1:], data[i:len(data)-1])
		}
		data[i] = e
		f.setMDT(d.dbuf())
		if i+1 < len(data) {
			c.cursorRight()
			return
		}
		// field full
		switch {
		case f.isAutoEnter():
			if c.sess.readOpcode != 0 {
				c.sess.handleAIDKey(AIDEnter)
			}
		case f.isFER():
			// Field Exit required: the cursor stays on the last position
		default:
			d.setCursorNextField()
		}
	})
}

// Key performs one 5250 keyboard function: AID keys (Enter, PF1-PF24,
// Help, RollUp, RollDown, Print, Clear, SysReq, TestReq, Attn), local
// field keys (FieldExit, FieldPlus, FieldMinus, Dup), editing (Insert,
// Delete, Backspace, EraseEOF, ErInp, Reset) and cursor movement (Tab, BackTab,
// NewLine, Home, Left, Right, Up, Down). Unknown names are ignored.
func (c *Client) Key(name string) {
	switch name {
	case "Attn":
		c.SendAttn()
		return
	case "Reset":
		c.mu.Lock()
		c.insert = false
		c.mu.Unlock()
		c.Reset()
		return
	}
	if code, local, ok := aidCodeForName(name); ok {
		c.withSession(func() {
			// Help and SysReq stay usable while locked; others need input
			if !local && name != "Help" && name != "SysReq" && c.disp.inhibited() {
				c.disp.beep()
				return
			}
			if local {
				if c.unlockedForInput() {
					c.handleLocalKey(name)
				}
			} else if c.sess.readOpcode != 0 || code < 0 {
				c.sess.handleAIDKey(code)
			}
		})
		return
	}
	c.withSession(func() {
		d := c.disp
		switch name {
		case "Tab":
			d.setCursorNextField()
		case "BackTab":
			// to the start of the current field, or the previous field
			if f := d.currentField(); f != nil && !f.isBypass() &&
				f.countLeft(d.cursorY(), d.cursorX()) > 0 {
				d.setCursorField(f)
			} else {
				d.setCursorField(d.prevField())
			}
		case "NewLine":
			d.setCursor((d.cursorY()+1)%d.height(), 0)
			if f := d.currentField(); f == nil || f.isBypass() {
				d.setCursorNextField()
			}
		case "Home":
			d.setCursorHome()
		case "Left":
			c.cursorLeft()
		case "Right":
			c.cursorRight()
		case "Up":
			d.setCursor((d.cursorY()+d.height()-1)%d.height(), d.cursorX())
		case "Down":
			d.setCursor((d.cursorY()+1)%d.height(), d.cursorX())
		case "Insert":
			c.insert = !c.insert
		case "Delete":
			if !c.unlockedForInput() {
				return
			}
			f := d.currentField()
			if f == nil || f.isBypass() {
				c.operatorError(KBDSrcProtect)
				return
			}
			c.deleteAtCursor(f)
		case "Backspace":
			// Destructive backspace: inside an input field, move left and
			// delete that character (the rest of the field shifts left).
			// At the start of a field, or outside one, only move left.
			f := d.currentField()
			if f == nil || f.isBypass() || f.countLeft(d.cursorY(), d.cursorX()) <= 0 {
				c.cursorLeft()
				return
			}
			if !c.unlockedForInput() {
				return
			}
			c.cursorLeft()
			c.deleteAtCursor(f)
		case "EraseEOF":
			if !c.unlockedForInput() {
				return
			}
			f := d.currentField()
			if f == nil || f.isBypass() {
				c.operatorError(KBDSrcProtect)
				return
			}
			data := d.fieldData(f)
			for i := f.countLeft(d.cursorY(), d.cursorX()); i < len(data); i++ {
				data[i] = 0x00
			}
			f.setMDT(d.dbuf())
		case "ErInp":
			// Erase Input: clear every modified input field, cursor home
			if !c.unlockedForInput() {
				return
			}
			if iter := d.dbuf().fieldList; iter != nil {
				for {
					if !iter.isBypass() && iter.mdt() {
						data := d.fieldData(iter)
						for i := range data {
							data[i] = 0x00
						}
						iter.clearMDT()
					}
					iter = iter.next
					if iter == d.dbuf().fieldList {
						break
					}
				}
			}
			d.setCursorHome()
		}
	})
}

// MoveCursor puts the cursor at row, col (0-based), as a click with a
// pointing device does. Only positions inside an input (non-bypass)
// field are accepted; others, and positions off the screen, are ignored.
func (c *Client) MoveCursor(row, col int) {
	c.withSession(func() {
		d := c.disp
		if row < 0 || col < 0 || row >= d.height() || col >= d.width() {
			return
		}
		// only into an input field: a click elsewhere (e.g. the click
		// that brings the browser window to the front) would leave the
		// cursor where typing fails with 0005 and AID keys go astray
		if f := d.fieldAt(row, col); f == nil || f.isBypass() {
			return
		}
		d.setCursor(row, col)
	})
}

// withSession runs fn under the client lock when connected, then publishes
// a snapshot to the update callback.
func (c *Client) withSession(fn func()) {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	fn()
	snap := c.buildSnapshot()
	c.mu.Unlock()
	if c.cb != nil {
		c.cb(snap)
	}
}

// deleteAtCursor removes the character at the cursor from field f: the
// rest of the field shifts left and a null fills its end.
func (c *Client) deleteAtCursor(f *field) {
	d := c.disp
	data := d.fieldData(f)
	i := f.countLeft(d.cursorY(), d.cursorX())
	if i < 0 || i >= len(data) {
		return
	}
	copy(data[i:], data[i+1:])
	data[len(data)-1] = 0x00
	f.setMDT(d.dbuf())
}

// cursorRight/cursorLeft move the cursor one position, wrapping at the
// screen edges.
func (c *Client) cursorRight() {
	d := c.disp
	p := (d.cursorY()*d.width() + d.cursorX() + 1) % (d.width() * d.height())
	d.setCursor(p/d.width(), p%d.width())
}

func (c *Client) cursorLeft() {
	d := c.disp
	n := d.width() * d.height()
	p := (d.cursorY()*d.width() + d.cursorX() + n - 1) % n
	d.setCursor(p/d.width(), p%d.width())
}
