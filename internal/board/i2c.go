package board

import "fmt"

// I2C bus on the CRTC status register 0x2012F, bit-banged by the
// firmware (F000:0DB3 write byte, F000:0E4C read byte; bus primitives
// F000:0EE8 write, 1031 random read, 120C send byte, 139E receive byte):
//
//	bit 0  SDA, inverted: writing 1 pulls the line low; reading 1 means
//	       the line is low
//	bit 1  SCL, inverted the same way
//
// so 0x18 = both lines released (idle), 0x19 = SDA low (START while SCL
// is high), 0x1B = both low, 0x1A = SCL low with SDA released. Bits 3-4
// are unrelated video control bits that are always written as 1 here.
//
// The ROM uses this bus only for display type 6 (POST state 0x0AA1 = 6,
// 0x0AC3 = 2), a monitor with its own adjustment memory: F000:2FE2 and
// the monitor adjustment screens (F000:4D91, 5A0C, 5A89). The 3477's
// strap values give display types 3-5, so the bus is idle on the 3477
// and its Setup configuration lives in battery-backed RAM instead
// (nvram.go). Two slaves are modelled:
//
//	A0/A1  a 24C02-style 256-byte EEPROM ("word address, then data");
//	       0x01-0x2B hold monitor adjustments (table F000:5D07)
//	8C/8D  a monitor control chip; the firmware writes register 2 with
//	       a value taken from EEPROM 0x29/0x2A (F000:30CC, F000:4E49).
//	       It is modelled as a plain register file.

// EEPROMSize is the size of the monitor adjustment EEPROM in bytes.
const EEPROMSize = 256

// i2cPhase is where the slave side of the bus is within a transfer.
type i2cPhase int

const (
	i2cIdle   i2cPhase = iota // waiting for START
	i2cRxByte                 // shifting in a byte from the master
	i2cRxAck                  // slave holds SDA low to acknowledge it
	i2cTxByte                 // slave shifts out a byte (read transfer)
	i2cTxAck                  // master acknowledges (or not) that byte
)

// i2cRegs is one addressable slave: a pointer register followed by a
// byte array, which covers both the EEPROM and the monitor chip.
type i2cRegs struct {
	addr uint8 // 8-bit bus address with the R/W bit clear
	data []uint8
}

// I2C is the two-wire bus on 0x2012F bits 0-1 with its slaves.
type I2C struct {
	// lines as driven by the master (true = released, i.e. high)
	mSDA, mSCL bool
	// sSDA is the slave's SDA driver (true = released)
	sSDA bool

	phase   i2cPhase
	bits    int   // bits shifted in the current byte
	shift   uint8 // byte being received or sent
	nbyte   int   // bytes received since START (0 = device address)
	read    bool  // the current transfer is a read
	dev     *i2cRegs
	ptr     uint8 // word address pointer of the selected slave
	ackedRx bool  // master acknowledged the last byte it read

	eeprom  i2cRegs
	monitor i2cRegs

	// Dirty is set whenever the firmware writes the EEPROM, so the host
	// application knows when to persist Save().
	Dirty bool
	// Log records transfers for reverse engineering (bounded).
	Log []string
}

func newI2C() *I2C {
	return &I2C{
		mSDA: true, mSCL: true, sSDA: true,
		eeprom:  i2cRegs{addr: 0xA0, data: make([]uint8, EEPROMSize)},
		monitor: i2cRegs{addr: 0x8C, data: make([]uint8, 16)},
	}
}

// logXfer records one byte transferred to or from a slave.
func (b *I2C) logXfer(op string, dev, ptr, v uint8) {
	if len(b.Log) < 4096 {
		b.Log = append(b.Log, fmt.Sprintf("%s %02X[%02X] = %02X", op, dev, ptr, v))
	}
}

// sda and scl return the wired-AND bus levels (true = high).
func (b *I2C) sda() bool { return b.mSDA && b.sSDA }
func (b *I2C) scl() bool { return b.mSCL }

// Lines returns the bus state as the firmware reads it from 0x2012F
// bits 0-1 (1 = line low).
func (b *I2C) Lines() uint8 {
	var r uint8
	if !b.sda() {
		r |= 1
	}
	if !b.scl() {
		r |= 2
	}
	return r
}

// Drive applies a write to 0x2012F: bit 0 pulls SDA low, bit 1 SCL.
// When both lines change in one write, a falling SCL is handled first
// and a rising SCL last, so that a data change is never mistaken for a
// START/STOP condition while the clock is moving.
func (b *I2C) Drive(v uint8) {
	newSDA, newSCL := v&1 == 0, v&2 == 0
	if b.mSCL && !newSCL {
		b.mSCL = false
		b.clockFell()
	}
	if newSDA != b.mSDA {
		before := b.sda()
		b.mSDA = newSDA
		b.dataChanged(before)
	}
	if !b.mSCL && newSCL {
		b.mSCL = true
		b.clockRose()
	}
}

// dataChanged detects START (SDA falls while SCL is high) and STOP (SDA
// rises while SCL is high).
func (b *I2C) dataChanged(before bool) {
	now := b.sda()
	if !b.scl() || now == before {
		return
	}
	if !now { // START or repeated START
		b.phase, b.bits, b.shift, b.nbyte = i2cRxByte, 0, 0, 0
		b.sSDA = true
		return
	}
	b.phase = i2cIdle // STOP
	b.sSDA = true
}

// clockRose samples SDA: a data bit from the master, or the master's
// acknowledge after a byte the slave sent.
func (b *I2C) clockRose() {
	switch b.phase {
	case i2cRxByte:
		b.shift <<= 1
		if b.sda() {
			b.shift |= 1
		}
		b.bits++
	case i2cTxByte:
		b.bits++
	case i2cTxAck:
		b.ackedRx = !b.sda()
	}
}

// clockFell moves the slave to its next output: acknowledge a complete
// byte, release SDA after the acknowledge, or present the next data bit.
func (b *I2C) clockFell() {
	switch b.phase {
	case i2cRxByte:
		if b.bits == 8 {
			if b.received(b.shift) {
				b.sSDA = false // ACK
				b.phase = i2cRxAck
			} else {
				b.sSDA = true // NAK: ignore the rest until START
				b.phase = i2cIdle
			}
		}
	case i2cRxAck:
		b.sSDA = true
		if b.read {
			b.startTx()
		} else {
			b.phase, b.bits, b.shift = i2cRxByte, 0, 0
		}
	case i2cTxByte:
		if b.bits == 8 {
			b.sSDA = true // the master drives the acknowledge bit
			b.phase = i2cTxAck
		} else {
			b.sSDA = b.shift&(0x80>>b.bits) != 0
		}
	case i2cTxAck:
		if b.ackedRx {
			b.startTx()
		} else {
			b.phase = i2cIdle
		}
	}
}

// startTx loads the byte at the pointer and drives its MSB.
func (b *I2C) startTx() {
	b.shift = b.dev.data[int(b.ptr)%len(b.dev.data)]
	b.logXfer("rd", b.dev.addr, b.ptr, b.shift)
	b.ptr++
	b.phase, b.bits = i2cTxByte, 0
	b.sSDA = b.shift&0x80 != 0
}

// received handles one byte from the master and reports whether a slave
// acknowledges it.
func (b *I2C) received(v uint8) bool {
	n := b.nbyte
	b.nbyte++
	if n == 0 { // device address + R/W
		b.dev = nil
		for _, d := range []*i2cRegs{&b.eeprom, &b.monitor} {
			if v&0xFE == d.addr {
				b.dev = d
			}
		}
		b.read = v&1 != 0
		if b.dev == nil {
			b.logXfer("nak", v, 0, 0)
		}
		return b.dev != nil
	}
	if n == 1 { // word address
		b.ptr = v
		return true
	}
	// data bytes of a write transfer
	if int(b.ptr) < len(b.dev.data) {
		if b.dev.data[b.ptr] != v && b.dev == &b.eeprom {
			b.Dirty = true
		}
		b.dev.data[b.ptr] = v
	}
	b.logXfer("wr", b.dev.addr, b.ptr, v)
	b.ptr++
	return true
}

// LoadEEPROM replaces the EEPROM contents (e.g. from a saved file).
// Shorter input leaves the remaining bytes erased (FF).
func (b *I2C) LoadEEPROM(p []uint8) {
	for i := range b.eeprom.data {
		b.eeprom.data[i] = 0xFF
	}
	copy(b.eeprom.data, p)
	b.Dirty = false
}

// SaveEEPROM returns a copy of the EEPROM contents.
func (b *I2C) SaveEEPROM() []uint8 {
	return append([]uint8(nil), b.eeprom.data...)
}

// MonitorRegs returns a copy of the monitor control chip's registers.
func (b *I2C) MonitorRegs() []uint8 {
	return append([]uint8(nil), b.monitor.data...)
}
