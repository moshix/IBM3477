package controller

import (
	"testing"

	"ibm3486/internal/board"
)

// fakePrinter is a printer station: it answers polls with a scripted
// status and records the commands it gets.
type fakePrinter struct {
	status, frame2 uint8
	cmds           []uint8
	data           [][]byte
}

func (f *fakePrinter) Deliver(addr, cmd uint8, data []uint8) (board.Response, bool) {
	if cmd == cmdPoll || cmd == cmdAck {
		return board.Response{Polled: true, Status: f.status, Frame2: f.frame2}, true
	}
	f.cmds = append(f.cmds, cmd)
	if cmd == prnActivateWrite {
		f.data = append(f.data, append([]byte(nil), data...))
	}
	return board.Response{}, true
}

// TestPrinterStationExceptions: power-on gets Set Mode, other exceptions
// a WCD reset, each closed by End of Queue.
func TestPrinterStationExceptions(t *testing.T) {
	f := &fakePrinter{status: prnPowerOn}
	p := NewPrinterStation(f, 1, 1)
	p.Tick(1)
	f.status = 0x08 // invalid command
	p.Tick(1)
	want := []uint8{prnSetMode, cmdEOQ, prnWCD, cmdEOQ}
	if string(f.cmds) != string(want) {
		t.Fatalf("commands % X, want % X", f.cmds, want)
	}
}

// TestPrinterStationCancelDropsOneJob: a cancel request drops the job
// being printed; the next job still prints.
func TestPrinterStationCancelDropsOneJob(t *testing.T) {
	f := &fakePrinter{}
	p := NewPrinterStation(f, 1, 1)
	p.Print(make([]byte, 600)) // job 1: 3 blocks
	p.Print([]byte{0xC1})      // job 2
	p.Tick(1)                  // Write Data
	p.Tick(1)                  // Activate Write: first block of job 1
	f.frame2 = prnCancelRequest
	p.Tick(1) // Clear: the rest of job 1 goes
	f.frame2 = 0
	for i := 0; i < 4; i++ {
		p.Tick(1)
	}
	if len(f.data) != 2 || len(f.data[1]) != 1 || f.data[1][0] != 0xC1 {
		t.Fatalf("printed blocks %d, last % X", len(f.data), f.data[len(f.data)-1])
	}
	if !p.Idle() {
		t.Fatal("station not idle")
	}
}
