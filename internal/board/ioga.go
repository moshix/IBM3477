package board

// IOGA is the I/O gate array at 0x20000-0x200FF: interrupt controller,
// twinax engine registers, keyboard port, beeper and model straps.
// Registers not yet understood behave as plain storage and every access
// to them is traced. See INTERRUPTS.md and TWINAX.md.
type IOGA struct {
	b    *Board
	regs [0x100]uint8
	// known marks offsets whose behaviour is modelled (not traced)
	known [0x100]bool
	kbd   kbdPort
	snd   sound // speaker registers 0x20010/11 (sound.go)
	// videoIRQ is the video range engine's completion request (vector
	// 0Ch), cleared by writing 0x20136 (video.go)
	videoIRQ bool
}

// Register offsets within the I/O gate array (0x20000 + offset).
const (
	regChannel      = 0x00 // 0x20000/1: twinax channel start (write 0x80)
	regTwinaxStatus = 0x02 // 0x20002/3: interrupt status, write 1 to clear
	regMask         = 0x04 // 0x20004/5: interrupt mask word, 1 = enabled
	regIntEnable    = 0x07 // bit 0: keyboard interrupt enable (F000:0C25)
	regStationAddr  = 0x08 // twinax station addresses (ch0 bits 0-2, ch1 3-5)
	regTwinaxCtl    = 0x09 // bit7 run, bit1/bit2 enable channel 0/1
	strapReg        = 0x0A // read-only hardware straps
	regKbdData      = 0x12 // keyboard data (see kbdPort)
	regKbdCtl       = 0x13 // keyboard control (W) / status (R)
	regKbdAck       = 0x14 // keyboard acknowledge
	regWrapMode     = 0x15 // twinax wrap/test mode (POST); 0x0C at runtime
	regWrapResult   = 0x2B // POST wrap-test result
)

// stTimer is the frame-timer status bit (16-bit view: 0x20002 = bits 0-7,
// 0x20003 = bits 8-15; the timer is 0x20003 bit7).
const stTimer = 0x8000

// Interrupt vectors supplied by the gate array on INTA. The firmware
// copies its handler table to vectors 0-14 (C000:0058); the nesting masks
// its handlers load give the priority used here, highest first.
const (
	vecTimer    = 0x06 // C000:5806
	vecCh0Poll  = 0x07 // C000:5E02: channel 0 status bit0
	vecCh1Poll  = 0x08 // C000:5E60 / D000:5902: channel 1 status bit0
	vecCh0      = 0x09 // C000:6C1F: channel 0 status bits 1-6
	vecCh1      = 0x0A // C000:6D22 / D000:5102: channel 1 status bits 1-6
	vecVideo    = 0x0C // C000:5702: video range engine done
	vecKeyboard = 0x0E // POST installs its keyboard handler at 0000:0038
)

func newIOGA(b *Board) *IOGA {
	g := &IOGA{b: b}
	g.kbd.kbd.powerOn() // the keyboard announces itself at power-on
	g.regs[strapReg] = b.Model.Straps
	for _, r := range []int{regChannel, regChannel + 1, regTwinaxStatus,
		regTwinaxStatus + 1, regMask, regMask + 1, regStationAddr,
		regTwinaxCtl, strapReg, regKbdData, regKbdCtl, regKbdAck,
		regIntEnable, regWrapMode, regWrapResult, regSound, regClick} {
		g.known[r] = true
	}
	return g
}

// Read returns a register value.
func (g *IOGA) Read(off uint32) uint8 {
	switch off {
	case regKbdData:
		return g.kbd.rx
	case regKbdCtl:
		return g.kbd.status()
	case regChannel, regChannel + 1:
		return 0 // channel idle
	}
	if !g.known[off] {
		g.b.trace(mmioBase+off, false, 0)
	}
	return g.regs[off]
}

// Write stores a register value.
func (g *IOGA) Write(off uint32, v uint8) {
	switch off {
	case strapReg:
		g.b.trace(mmioBase+off, true, v) // straps are read-only
		return
	case regTwinaxStatus, regTwinaxStatus + 1:
		g.regs[off] &^= v // write 1 to clear
		return
	case regKbdData:
		g.kbd.writeData(v)
	case regKbdCtl:
		g.kbd.writeControl(v)
	case regKbdAck:
		g.kbd.ack()
	case regSound, regClick:
		g.snd.write(off, v)
	}
	if g.b.Twinax.write(off, v) {
		return
	}
	if !g.known[off] {
		g.b.trace(mmioBase+off, true, v)
	}
	g.regs[off] = v
}

// Tick advances device time by n CPU cycles.
func (g *IOGA) Tick(n int) { g.kbd.tick(n) }

// FrameTimer raises the timer interrupt (0x20003 bit7). The firmware
// never programs a rate; it is modelled as a 60 Hz frame interrupt raised
// at each vertical sync [G].
func (g *IOGA) FrameTimer() { g.regs[regTwinaxStatus+1] |= stTimer >> 8 }

// pending returns the 16 status bits that are enabled in the mask.
func (g *IOGA) pending() uint16 {
	st := uint16(g.regs[regTwinaxStatus]) | uint16(g.regs[regTwinaxStatus+1])<<8
	mask := uint16(g.regs[regMask]) | uint16(g.regs[regMask+1])<<8
	return st & mask
}

// INTR implements cpu.Interrupts: a level request while any enabled
// source is pending.
func (g *IOGA) INTR() bool {
	return g.pending() != 0 || g.videoIRQ || g.kbd.irq && g.regs[regIntEnable]&0x01 != 0
}

// Ack implements cpu.Interrupts: the INTA cycle returns the vector of the
// highest-priority pending source. Requests stay asserted until the
// firmware clears the source (status write-1-to-clear, 0x20014 for the
// keyboard).
func (g *IOGA) Ack() uint8 {
	p := g.pending()
	switch {
	case p&stTimer != 0:
		return vecTimer
	case p&0x0001 != 0:
		return vecCh0Poll
	case p&0x0100 != 0:
		return vecCh1Poll
	case p&0x007E != 0:
		return vecCh0
	case p&0x7E00 != 0:
		return vecCh1
	case g.videoIRQ:
		return vecVideo
	}
	return vecKeyboard
}

// LED returns register 0x20010 without tracing: the POST writes its
// progress there; at run time it is the tone control (sound.go).
func (g *IOGA) LED() uint8 { return g.regs[0x10] }

// Keyboard gives access to the attached keyboard (to inject key events).
func (g *IOGA) Keyboard() *Keyboard { return &g.kbd.kbd }
