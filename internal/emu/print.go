package emu

import (
	"sync/atomic"

	"ibm3486/internal/controller"
	"ibm3486/internal/printer"
)

// Printing: the firmware drives its printer port (board/printer.go) with
// IBM Proprinter commands. The machine collects what it sends and, after
// printIdle without new data, interprets it as one print job. Streams
// without visible text (the printer set-up the firmware sends at every
// start) are not jobs.

// printIdle is how long the printer port must stay quiet before what it
// received counts as a finished job: 2 s of emulated time.
const printIdle = 2 * cpuHz

// maxPrintJob caps one job's data (about 2,000 full pages of text); more
// is dropped, so a runaway stream cannot fill memory.
const maxPrintJob = 4 << 20

// printCollector gathers printer data into jobs (owned by the machine
// goroutine).
type printCollector struct {
	buf       []byte
	lastData  uint64 // CPU cycle of the last byte received
	truncated bool   // the job went over maxPrintJob: the rest is dropped
	handler   func(*printer.Document)
}

// SetPrintHandler sets the function that receives each finished print
// job. It is called from the machine goroutine; call before Start.
func (m *Machine) SetPrintHandler(f func(*printer.Document)) { m.prn.handler = f }

// collectPrint takes the printer data of the last slice and finishes a
// job once the port has been quiet for printIdle.
func (m *Machine) collectPrint() {
	if out := m.b.IO.PrinterOutput(); len(out) > 0 {
		// past the limit the rest of the job is dropped, not pieced
		// together: a later small piece could land inside a cut-off
		// command and be misread
		if !m.prn.truncated && len(m.prn.buf)+len(out) <= maxPrintJob {
			m.prn.buf = append(m.prn.buf, out...)
		} else {
			m.prn.truncated = true
		}
		m.prn.lastData = m.b.CPU.Cycles
		return
	}
	if (len(m.prn.buf) == 0 && !m.prn.truncated) || m.b.CPU.Cycles-m.prn.lastData < printIdle {
		return
	}
	doc := printer.Interpret(m.prn.buf)
	m.prn.buf, m.prn.truncated = nil, false
	if doc.HasText() && m.prn.handler != nil {
		m.prn.handler(doc)
	}
}

// Setup memory of the 3477 (NVRAM.md): terminal mode and the station
// addresses, each with a check byte (pair sum 0xEA).
const (
	nvDisplayAddr  = 0x1A0 // display address 1, check byte at +1
	nvPrinterAddr  = 0x1A4 // printer address, check byte at +1
	nvTerminalMode = 0x1A6 // 1 One-Display, 2 Two-Display, 3 Display-Printer
	modeDispPrint  = 3
)

// setupAddresses reads the terminal's display and printer station
// addresses from its setup memory; ok is false unless the terminal is in
// Display-Printer mode with valid addresses.
func (m *Machine) setupAddresses() (display, prn uint8, ok bool) {
	mem := m.b.Mem()
	valid := func(a uint32) (uint8, bool) {
		v := mem[a] & 0x7F
		return v, mem[a]+mem[a+1] == 0xEA && v <= 6
	}
	d, okD := valid(nvDisplayAddr)
	p, okP := valid(nvPrinterAddr)
	return d, p, mem[nvTerminalMode] == modeDispPrint && okD && okP && d != p
}

// attach builds the workstation controller for a host session (nil
// detaches). In Display-Printer mode the display controller uses the
// setup's display address and a printer station driver serves the
// printer address; otherwise the controller searches addresses 0-6 for
// the display. Runs on the machine goroutine.
func (m *Machine) attach(host controller.Host) {
	m.ctl, m.prnStation = nil, nil
	if host == nil {
		return
	}
	const pollEvery = cpuHz / 1000 // about every 1 ms, as a controller does
	disp, prn, ok := m.setupAddresses()
	if !ok {
		m.ctl = controller.New(m.b.Twinax, -1, pollEvery, host)
		m.ctl.SetDirectPrint(m.printDirect)
		return
	}
	m.ctl = controller.New(m.b.Twinax, int(disp), pollEvery, host)
	m.prnStation = controller.NewPrinterStation(m.b.Twinax, prn, pollEvery)
	m.ctl.SetPrinter(m.prnStation, !m.hostPrint.Load())
}

// SetPrintKeyToHost chooses what the Print key does in Display-Printer
// mode: false (default) prints a copy of the screen on the terminal's
// printer; true sends it to the host, which prints on its own printer
// device. Safe from any goroutine; applies at once.
func (m *Machine) SetPrintKeyToHost(toHost bool) { m.hostPrint.Store(toHost) }

// hostPrintFlag is SetPrintKeyToHost's setting (atomic: set by the web
// goroutine, read by the machine goroutine).
type hostPrintFlag = atomic.Bool

// printDirect makes a print job from SCS print data (a screen copy, or a
// host print job) when the terminal has no printer station: the SCS is
// laid out as text in the session's codepage (printer.SCSText).
func (m *Machine) printDirect(scs []byte) {
	text := printer.SCSText(scs, func(c byte) rune { return m.decode[c] })
	if doc := printer.TextDocument(text); doc.HasText() && m.prn.handler != nil {
		m.prn.handler(doc)
	}
}

// PrintSCS prints a host print job (SCS data from a printer session):
// through the terminal's printer station in Display-Printer mode, so the
// 3477 formats it with its Setup Printer choices, else directly. Safe
// from any goroutine.
func (m *Machine) PrintSCS(scs []byte) {
	select {
	case m.hostJobs <- scs:
	case <-m.stop:
	}
}

// hostPrint runs on the machine goroutine.
func (m *Machine) hostPrintJob(scs []byte) {
	if m.prnStation != nil {
		// queued even before the station answers its first poll (just
		// attached, or the terminal restarting): Tick delivers it
		m.prnStation.Print(scs)
		return
	}
	m.printDirect(scs)
}
