// Package emu runs one emulated terminal in real time: it paces the 8088
// to wall-clock time, injects keyboard events and publishes screen
// snapshots for the web front end.
package emu

import (
	"bytes"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"

	"ibm3486/internal/board"
	"ibm3486/internal/controller"
)

// cpuHz is the emulated 8088 clock (59.8185 MHz crystal / 8).
const cpuHz = 59818500 / 8

// Snapshot is one published screen state.
type Snapshot struct {
	Rows, Cols int
	CellWidth  int // dots per character cell
	CellHeight int // scan lines per character row
	CursorRow  int // cursor position (CRTC)
	CursorCol  int
	CursorOn   bool
	Cells      []byte // 3 bytes per cell: EBCDIC char, attribute, extra
	// Text is the displayed text, one line per row, for copying: blanks,
	// nulls, attribute positions and non-display fields are spaces
	Text []string
	Font []byte // font RAM; nil when unchanged since the last snapshot
	LED  uint8  // POST/error code register (0x20010)
	// Sound is the speaker state (tone on/off, volume, clicks so far)
	Sound board.Sound
}

// Machine is a running terminal.
type Machine struct {
	b    *board.Board
	keys chan []uint8
	// paste delivers text to type (paste.go); pending holds its remaining
	// steps, one every pasteKeyGap cycles
	paste    chan []pasteStep
	pending  []pasteStep
	nextPast uint64
	// pasting is set while a paste is being typed (read by Paste from
	// other goroutines); stallSince is when the current pasted key began
	// waiting for the keyboard buffer to drain (0 = not waiting)
	pasting    atomic.Bool
	stallSince uint64
	stop       chan struct{}
	stopOnce   sync.Once
	done       sync.WaitGroup

	// ctlCh hands a (new) workstation controller to the machine
	// goroutine, which owns the board; nil detaches it.
	ctlCh chan *controller.Controller
	ctl   *controller.Controller

	// decode is the EBCDIC codepage table that decodes the screen for
	// copying (SetCodepage)
	decode *[256]rune

	// non-volatile setup memory persistence (nvram.go)
	nvPath string
	lastNV []byte

	// Key clicks the firmware makes for pasted keys are not played: only
	// typing clicks. boardClicks is the firmware's click count last seen,
	// mutedClicks those that fell into a paste (subtracted from Sound).
	boardClicks, mutedClicks uint32
}

// New builds a terminal of the given model with ROMs from roms.
func New(model string, roms fs.FS) (*Machine, error) {
	m, ok := board.Models[model]
	if !ok {
		return nil, errUnknownModel(model)
	}
	b, err := board.New(m, roms)
	if err != nil {
		return nil, err
	}
	mach := &Machine{
		b:     b,
		keys:  make(chan []uint8, 64),
		paste: make(chan []pasteStep, 1),
		stop:  make(chan struct{}),
		ctlCh: make(chan *controller.Controller, 1),
	}
	mach.SetCodepage("")
	mach.nvPath = mach.loadNVRAM(model)
	return mach, nil
}

type errUnknownModel string

func (e errUnknownModel) Error() string { return "unknown terminal model " + string(e) }

// Key queues scan code bytes for the emulated keyboard (safe to call
// from any goroutine).
func (m *Machine) Key(codes []uint8) {
	select {
	case m.keys <- codes:
	default: // drop when the queue is full, like a real typeahead overrun
	}
}

// Start runs the machine until Stop, calling update (from the machine's
// goroutine) whenever the screen changes, at most 30 times a second.
func (m *Machine) Start(update func(*Snapshot)) {
	m.done.Add(1)
	go m.loop(update)
}

// Stop halts the machine and waits for its goroutine to exit. Calling it
// again (or from several goroutines) is harmless.
func (m *Machine) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.done.Wait()
}

// loop emulates in 5 ms slices: each slice runs the number of CPU cycles
// that correspond to the wall-clock time elapsed, so the firmware's delay
// loops and timeouts behave as on real hardware.
func (m *Machine) loop(update func(*Snapshot)) {
	defer m.done.Done()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	var emulated uint64
	pub := newPublisher()
	lastSave := time.Now()
	for {
		select {
		case <-m.stop:
			m.saveNVRAM()
			return
		case steps := <-m.paste:
			// one paste at a time: a paste arriving while another is
			// still being typed is dropped (Paste)
			if len(m.pending) == 0 {
				m.pending = steps
				m.pasting.Store(len(steps) > 0)
			}
			continue
		case codes := <-m.keys:
			m.b.IO.Keyboard().Key(codes...)
			continue
		case ctl := <-m.ctlCh:
			if m.ctl != nil {
				m.ctl.Shutdown() // clear its status-row indicators
			}
			m.ctl = ctl
			continue
		case now := <-tick.C:
			target := uint64(now.Sub(start).Seconds() * cpuHz)
			if target-emulated > cpuHz/10 {
				emulated = target - cpuHz/10 // never try to catch up > 100 ms
			}
			m.run(target - emulated)
			emulated = target
			if now.Sub(lastSave) >= time.Second {
				m.saveNVRAM() // the firmware may have changed the setup
				lastSave = now
			}
			if s := pub.check(m, now); s != nil {
				update(s)
			}
		}
	}
}

// publisher decides when the screen changed and builds the snapshots.
// It looks at the board at most every 33 ms (sound at once) and does the
// costly work, decoding the text and copying the 32 KB font, only for
// what actually changed: an idle terminal costs one compare of the cells
// and the font per check and allocates nothing.
type publisher struct {
	scratch   []byte // cells read from video RAM for the comparison
	lastCells []byte // cells last published (owned by that snapshot)
	lastText  []string
	lastFont  []byte
	lastGeom  [3]int // rows, cols, cell height
	lastLED   uint8
	lastCur   [3]int
	lastSound board.Sound
	lastCheck time.Time
}

func newPublisher() *publisher { return &publisher{lastLED: 0xFF} }

// check returns a snapshot when something changed since the last one,
// otherwise nil. Called from the machine goroutine.
func (p *publisher) check(m *Machine, now time.Time) *Snapshot {
	// sound changes go out at once (a click or the start/end of a
	// tone must not wait for the 30 Hz screen rate)
	snd := m.b.IO.Sound()
	snd.Clicks -= m.mutedClicks // pasted keys do not click
	if snd == p.lastSound && now.Sub(p.lastCheck) < 33*time.Millisecond {
		return nil
	}
	p.lastCheck = now
	rows, cols, ch, cells := m.b.Video.ScreenTo(p.scratch)
	p.scratch = cells
	led := m.b.IO.LED()
	crow, ccol, con := m.b.Video.Cursor()
	cur := [3]int{crow, ccol, btoi(con)}
	geom := [3]int{rows, cols, ch}
	cellsChanged := geom != p.lastGeom || !bytes.Equal(cells, p.lastCells)
	fontRAM := m.b.Video.FontRAM()
	fontChanged := !bytes.Equal(fontRAM, p.lastFont)
	if !cellsChanged && !fontChanged && led == p.lastLED && cur == p.lastCur && snd == p.lastSound {
		return nil
	}
	if cellsChanged {
		// the published cells belong to the snapshot: a copy, never
		// the scratch buffer reused at the next check
		p.lastCells = append([]byte(nil), cells...)
		p.lastText = screenText(p.lastCells, cols, m.decode)
		p.lastGeom = geom
	}
	s := &Snapshot{Rows: rows, Cols: cols, CellWidth: m.b.Video.CellWidth(cols),
		CellHeight: ch, CursorRow: crow, CursorCol: ccol, CursorOn: con,
		Cells: p.lastCells, LED: led, Text: p.lastText, Sound: snd}
	if fontChanged {
		p.lastFont = append([]byte(nil), fontRAM...)
		s.Font = p.lastFont
	}
	p.lastLED, p.lastCur, p.lastSound = led, cur, snd
	return s
}

// run emulates cycles in 1 ms chunks so an attached workstation
// controller polls the station at the twinax pace (about every 1 ms).
func (m *Machine) run(cycles uint64) {
	const chunk = cpuHz / 1000
	for cycles > 0 {
		n := uint64(chunk)
		if cycles < n {
			n = cycles
		}
		m.b.Run(n)
		// type the next pasted key when its time has come
		if len(m.pending) > 0 && m.b.CPU.Cycles >= m.nextPast && m.pasteKeyboardReady() {
			switch st := m.pending[0]; {
			case m.ctl != nil && m.ctl.InputError():
				// a pasted character was rejected (e.g. a letter in a
				// numeric field): stop, as typing would, instead of
				// beeping for every remaining character
				m.pending = nil
			case st.keys != nil:
				m.b.IO.Keyboard().Key(st.keys...)
				m.pending = m.pending[1:]
			default:
				if m.ctl != nil {
					m.ctl.TypeDirect(st.char)
				}
				m.pending = m.pending[1:]
			}
			m.nextPast = m.b.CPU.Cycles + pasteKeyGap
			m.pasting.Store(len(m.pending) > 0)
		}
		if m.ctl != nil {
			m.ctl.Tick(int(n))
		}
		m.mutePasteClicks()
		cycles -= n
	}
}

// mutePasteClicks counts the clicks the firmware made since the last
// call as muted while a paste is being typed: steps are left, or the
// last step went out less than two gaps ago (its keys are still on their
// way through the keyboard line and the firmware).
func (m *Machine) mutePasteClicks() {
	clk := m.b.IO.Sound().Clicks
	if clk == m.boardClicks {
		return
	}
	if len(m.pending) > 0 || m.b.CPU.Cycles < m.nextPast+pasteKeyGap {
		m.mutedClicks += clk - m.boardClicks
	}
	m.boardClicks = clk
}

// Attach connects a TN5250 host session to the terminal's twinax port
// through an emulated workstation controller (nil host detaches). Safe to
// call from any goroutine.
func (m *Machine) Attach(host controller.Host) {
	var ctl *controller.Controller
	if host != nil {
		// station address unknown (terminal setup): search 0-6
		ctl = controller.New(m.b.Twinax, -1, cpuHz/1000, host)
	}
	select {
	case <-m.ctlCh: // replace a hand-over not yet picked up
	default:
	}
	m.ctlCh <- ctl
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
