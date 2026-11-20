package board

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"ibm3486/internal/tn5250"
)

// screenText returns the screen as text, one string per row.
func screenText(b *Board) []string {
	rows, cols, _, cells := b.Video.Screen()
	out := make([]string, rows)
	for r := 0; r < rows; r++ {
		var s []rune
		for c := 0; c < cols; c++ {
			e := cells[(r*cols+c)*3]
			ru := tn5250.EBCDICToRune("37", e)
			if e == 0 || ru < 0x20 {
				ru = ' '
			}
			s = append(s, ru)
		}
		out[r] = string(s)
	}
	return out
}

func boot3477(t *testing.T) *Board {
	t.Helper()
	needROM(t, "ibm3477_part1.bin")
	b, err := New(&Model3477, os.DirFS(romDir))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDefaultSetupChecks verifies the default image against the checks
// of POST test 7 (F000:2EFE) without running the firmware.
func TestDefaultSetupChecks(t *testing.T) {
	d := Default3477Setup
	at := func(a int) uint8 { return d[a-0x1A0] }
	for _, a := range []int{0x1A0, 0x1A2, 0x1A4} {
		if at(a)+at(a+1) != 0xEA || at(a)&0x7F > 6 {
			t.Errorf("address pair at %03X = %02X %02X", a, at(a), at(a+1))
		}
	}
	sum := func(lo, hi int) uint8 {
		var s uint8
		for a := lo; a <= hi; a++ {
			s += at(a)
		}
		return s
	}
	if s := sum(0x1A6, 0x1DD) + at(0x1DE); s != 0xEA {
		t.Errorf("setup block sum %02X, want EA", s)
	}
	if s := sum(0x1F6, 0x1FE) + at(0x1FF); s != 0xEA {
		t.Errorf("serial number sum %02X, want EA", s)
	}
}

// TestNVRAMSaveLoad: the non-volatile range round-trips through
// SaveNVRAM/LoadNVRAM and a new board starts with the default image.
func TestNVRAMSaveLoad(t *testing.T) {
	b := boot3477(t)
	img := b.SaveNVRAM()
	if len(img) != 0x900 || !bytes.Equal(img[:len(Default3477Setup)], Default3477Setup) {
		t.Fatalf("new board: %d bytes, starts % X", len(img), img[:16])
	}
	img[0x100] = 0x5A
	b.LoadNVRAM(img)
	if b.Mem()[0x2A0] != 0x5A || !bytes.Equal(b.SaveNVRAM(), img) {
		t.Fatal("LoadNVRAM/SaveNVRAM do not round-trip")
	}
}

// TestDefaultSetupBootsNormally: with the default image POST test 7
// finds a valid configuration (0x0AA0 = 0) and the run time shows its
// normal screen, not Setup. This also needs the video range engine's
// completion interrupt (the run time waits for it at C000:9D1A).
func TestDefaultSetupBootsNormally(t *testing.T) {
	b := boot3477(t)
	b.Run(200_000_000)
	if f := b.Mem()[0xAA0]; f != 0 {
		t.Fatalf("POST configuration check flags 0AA0 = %02X, want 0", f)
	}
	scr := screenText(b)
	all := strings.Join(scr, "\n")
	for _, bad := range []string{"Setup", "SETUP", "Selections have been reset"} {
		if strings.Contains(all, bad) {
			t.Fatalf("screen shows %q:\n%s", bad, all)
		}
	}
	if !strings.Contains(scr[len(scr)-1], "1/80") {
		t.Errorf("status row %q, want the cursor position indicator", scr[len(scr)-1])
	}
}

// TestSetupKey: at run time the set-3 key 01 (make 01, break F0 01)
// opens Online Setup.
func TestSetupKey(t *testing.T) {
	b := boot3477(t)
	b.Run(160_000_000)
	b.IO.Keyboard().Key(0x01)
	b.Run(1_000_000)
	b.IO.Keyboard().Key(0xF0, 0x01)
	b.Run(40_000_000)
	scr := screenText(b)
	if !strings.Contains(scr[0], "Online Setup") || !strings.Contains(scr[len(scr)-1], "SETUP") {
		t.Fatalf("no Online Setup:\n%s", strings.Join(scr, "\n"))
	}
}

// TestPowerOnKeyOpensOfflineSetup: a key held during POST makes
// F000:01A0 set 0x0AA2 bit 6 and the run time opens Offline Setup
// (C000:0901) without resetting the stored selections.
func TestPowerOnKeyOpensOfflineSetup(t *testing.T) {
	b := boot3477(t)
	b.Run(10_000_000)
	b.IO.Keyboard().Key(0x1C)
	b.Run(150_000_000)
	b.IO.Keyboard().Key(0xF0, 0x1C)
	b.Run(20_000_000)
	all := strings.Join(screenText(b), "\n")
	if !strings.Contains(all, "Offline Setup") || strings.Contains(all, "Selections have been reset") {
		t.Fatalf("screen:\n%s", all)
	}
}

// i2cMaster drives the bus the way F000:0EE8/1031/120C/139E do
// (0x2012F bit 0 = SDA, bit 1 = SCL, 1 = pull low).
type i2cMaster struct{ bus *I2C }

func (m i2cMaster) set(sdaLow, sclLow bool) {
	v := uint8(0x18)
	if sdaLow {
		v |= 1
	}
	if sclLow {
		v |= 2
	}
	m.bus.Drive(v)
}

func (m i2cMaster) start() { m.set(false, false); m.set(true, false); m.set(true, true) }

func (m i2cMaster) stop() { m.set(true, true); m.set(true, false); m.set(false, false) }

// send clocks out a byte MSB first and returns whether it was ACKed.
func (m i2cMaster) send(v uint8) bool {
	for i := 7; i >= 0; i-- {
		low := v&(1<<i) == 0
		m.set(low, true)
		m.set(low, false)
		m.set(low, true)
	}
	m.set(false, true) // release SDA for the acknowledge
	m.set(false, false)
	ack := m.bus.Lines()&1 != 0
	m.set(false, true)
	return ack
}

// recv clocks in a byte and answers with ACK or NAK.
func (m i2cMaster) recv(ack bool) uint8 {
	var v uint8
	for i := 0; i < 8; i++ {
		m.set(false, true)
		m.set(false, false)
		v <<= 1
		if m.bus.Lines()&1 == 0 {
			v |= 1
		}
	}
	m.set(false, true)
	m.set(ack, true)
	m.set(ack, false)
	m.set(ack, true)
	return v
}

// TestI2CEEPROM writes and reads back EEPROM bytes over the bit-banged
// bus and checks that an absent device is not acknowledged.
func TestI2CEEPROM(t *testing.T) {
	bus := newI2C()
	m := i2cMaster{bus}
	m.start()
	if !m.send(0xA0) || !m.send(0x1B) || !m.send(0x42) || !m.send(0x43) {
		t.Fatal("write not acknowledged")
	}
	m.stop()
	if !bus.Dirty {
		t.Error("EEPROM write did not set Dirty")
	}
	// random read: address the word, repeated START, read two bytes
	m.start()
	m.send(0xA0)
	m.send(0x1B)
	m.set(false, true)
	m.set(false, false)
	m.start()
	if !m.send(0xA1) {
		t.Fatal("read address not acknowledged")
	}
	a, b := m.recv(true), m.recv(false)
	m.stop()
	if a != 0x42 || b != 0x43 {
		t.Fatalf("read %02X %02X, want 42 43", a, b)
	}
	m.start()
	if m.send(0x50) {
		t.Error("absent device acknowledged")
	}
	m.stop()
	if got := bus.SaveEEPROM()[0x1B]; got != 0x42 {
		t.Errorf("SaveEEPROM()[1B] = %02X", got)
	}
}
