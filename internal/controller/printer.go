package controller

import (
	"ibm3486/internal/tn5250"
)

// PrinterStation drives a twinax printer station: the 3477's printer in
// Display-Printer mode, at the Printer Address of its Setup. It sends
// SCS print data the way IBM's 5250 attachment manual describes (chapter
// 3, "Printer Interface"); the 3477 firmware formats it with its Setup
// Printer choices and prints it on its printer port.
//
// Protocol (printed pages of the manual):
//   - The first Poll after power-on reports exception 7 (power-on
//     transition) and only one status frame; Set Mode resets it (p.93,
//     105). Any other exception is reset with Write Control Data, data
//     bit 0x04 (p.92).
//   - To print: wait for a Poll with not busy, no exception and receive
//     buffers not full (frame 2 bit 0x20); send Write Data (0x1E); poll
//     until not busy; send Activate Write (0x01) with up to 256 data
//     bytes (p.100-103).
//   - A cancel request (frame 2 bit 0x08) is answered with Clear (p.95).
type PrinterStation struct {
	st        Station
	addr      uint8
	pollEvery int
	since     int
	queue     [][]byte // print data, in blocks of at most 256 bytes
	activate  bool     // Write Data sent: Activate Write is next
	// Online reports whether the printer station answered the last poll.
	Online bool
}

// Printer station commands and status bits.
const (
	prnSetMode       = 0x13
	prnWCD           = 0x05
	prnWriteData     = 0x1E
	prnActivateWrite = 0x01
	prnClear         = 0x12
	prnBusy          = 0x80
	prnException     = 0x0E // frame 1 bits 11-13
	prnPowerOn       = 0x0E // exception 7
	prnResetExc      = 0x04 // WCD data: reset exception status
	prnBuffersFull   = 0x20 // frame 2
	prnCancelRequest = 0x08 // frame 2
	prnBlock         = 256  // data bytes per Activate Write
)

// NewPrinterStation returns a driver for the printer station at addr,
// polling every pollEvery CPU cycles.
func NewPrinterStation(st Station, addr uint8, pollEvery int) *PrinterStation {
	return &PrinterStation{st: st, addr: addr, pollEvery: pollEvery}
}

// Print queues SCS print data.
func (p *PrinterStation) Print(scs []byte) {
	for len(scs) > 0 {
		n := min(len(scs), prnBlock)
		p.queue = append(p.queue, scs[:n])
		scs = scs[n:]
	}
}

// Idle reports whether all queued data went to the printer.
func (p *PrinterStation) Idle() bool { return len(p.queue) == 0 && !p.activate }

// Tick advances time by n CPU cycles, polling when due.
func (p *PrinterStation) Tick(n int) {
	p.since += n
	if p.since < p.pollEvery {
		return
	}
	p.since = 0
	r, ok := p.st.Deliver(p.addr, cmdPoll, nil)
	if !ok || !r.Polled {
		p.Online = false
		return
	}
	p.Online = true
	p.st.Deliver(p.addr, cmdAck, nil)
	switch exc := r.Status & prnException; {
	case exc == prnPowerOn:
		p.st.Deliver(p.addr, prnSetMode, []uint8{0x00}) // fill count 0
		p.st.Deliver(p.addr, cmdEOQ, nil)
		p.activate = false
		return
	case exc != 0:
		p.st.Deliver(p.addr, prnWCD, []uint8{prnResetExc})
		p.st.Deliver(p.addr, cmdEOQ, nil)
		p.activate = false // the station emptied its queue: send again
		return
	}
	if r.Frame2&prnCancelRequest != 0 {
		p.st.Deliver(p.addr, prnClear, nil)
		p.queue, p.activate = nil, false
		return
	}
	if r.Status&prnBusy != 0 {
		return
	}
	switch {
	case p.activate:
		p.st.Deliver(p.addr, prnActivateWrite, p.queue[0])
		p.queue = p.queue[1:]
		p.activate = false
	case len(p.queue) > 0 && r.Frame2&prnBuffersFull == 0:
		p.st.Deliver(p.addr, prnWriteData, nil)
		p.activate = true
	}
}

// SCS control codes for a screen copy.
const (
	scsNL = 0x15 // new line
	scsFF = 0x0C // form feed
)

// screenSCS is a local copy of the host screen as SCS print data: one
// printed line per screen row, then a form feed. Attribute positions and
// non-display fields (passwords) print as blanks; trailing blanks are
// left out.
func screenSCS(s tn5250.ScreenState) []byte {
	var out []byte
	for row := 0; row < s.Rows; row++ {
		line := make([]byte, 0, s.Cols)
		hidden := false
		for col := 0; col < s.Cols; col++ {
			c := s.Data[row*s.Cols+col]
			switch {
			case c >= 0x20 && c <= 0x3F: // attribute: blank; 0x27/2F/37/3F hide
				hidden = c&0x07 == 0x07
				c = 0x40
			case c < 0x40 || hidden:
				c = 0x40 // nulls, controls, non-display text
			}
			line = append(line, c)
		}
		for len(line) > 0 && line[len(line)-1] == 0x40 {
			line = line[:len(line)-1]
		}
		out = append(out, line...)
		out = append(out, scsNL)
	}
	return append(out, scsFF)
}
