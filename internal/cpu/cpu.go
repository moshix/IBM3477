// Package cpu implements an Intel 8088 interpreter, faithful enough to
// run the IBM InfoWindow terminal firmware unmodified.
//
// The model is instruction-level (not cycle-exact): each Step executes one
// instruction. A REP string instruction is interruptible as on real
// hardware: Step stops it between iterations when an interrupt is pending
// or a cycle budget is used up, leaving CX/SI/DI updated and IP on its
// first prefix byte so the next Step resumes it. Behaviour is checked against the hardware-captured
// SingleStepTests 8088 suite, including the 8088's undocumented opcode
// aliases (60-6F, C0/C1, C8/C9, D6 SALC, shift /6 SETMO, 0F POP CS, ...).
package cpu

// Bus is the CPU's view of the outside world: a 20-bit memory space and
// a 16-bit I/O port space.
type Bus interface {
	Read8(addr uint32) uint8
	Write8(addr uint32, v uint8)
	In8(port uint16) uint8
	Out8(port uint16, v uint8)
}

// Interrupts is the maskable-interrupt input (INTR) and its acknowledge
// cycle, normally provided by the board's interrupt controller.
type Interrupts interface {
	// INTR reports whether an interrupt request is asserted.
	INTR() bool
	// Ack runs the interrupt-acknowledge cycle and returns the vector.
	Ack() uint8
}

// General register indices, in ModRM encoding order.
const (
	AX = iota
	CX
	DX
	BX
	SP
	BP
	SI
	DI
)

// Segment register indices, in ModRM encoding order.
const (
	ES = iota
	CS
	SS
	DS
)

// Flag bits.
const (
	FlagCF = 0x0001
	FlagPF = 0x0004
	FlagAF = 0x0010
	FlagZF = 0x0040
	FlagSF = 0x0080
	FlagTF = 0x0100
	FlagIF = 0x0200
	FlagDF = 0x0400
	FlagOF = 0x0800

	// On the 8086/8088, bits 12-15 always read as 1 and bit 1 is fixed 1;
	// bits 3 and 5 always read as 0.
	flagsFixed1 = 0xF002
	flagsWrite  = 0x0FD5
)

// CPU is the complete architectural state of an 8088.
type CPU struct {
	R      [8]uint16 // AX CX DX BX SP BP SI DI
	S      [4]uint16 // ES CS SS DS
	IP     uint16
	Flags  uint16
	Halted bool

	// Cycles is an approximate T-state count, used only for pacing the
	// emulation in real time (4 per bus byte plus per-opcode extras).
	Cycles uint64

	Bus  Bus
	Intr Interrupts // may be nil

	nmi bool

	// Per-instruction decode state
	segOver   int    // segment override (-1 = none)
	rep       uint8  // 0, 0xF2 (REPNE) or 0xF3 (REP/REPE)
	instStart uint16 // IP of the instruction's first prefix byte
	// inPrefix is set when execute stopped inside a long run of prefix
	// bytes (see maxPrefixes); the next execute continues decoding with
	// segOver/rep/instStart kept, and no interrupt is taken in between.
	inPrefix bool
	// stepping is set while Step runs execute: only then may a REP
	// string instruction stop early (see stringOp). Direct callers of
	// execute (the SingleStepTests harness) see whole REP instructions.
	stepping  bool
	stepStart uint64 // Cycles at the start of the current Step

	// noIntr holds off every interrupt (NMI, INTR, single-step trap)
	// until after the next instruction; set by MOV/POP to a segment
	// register, as on the 8086/8088.
	noIntr bool
	// stiShadow holds off INTR and the single-step trap until after the
	// instruction following STI; NMI is not affected.
	stiShadow bool

	// Decoded ModRM operand
	mod, reg, rm uint8
	eaSeg        int
	eaOff        uint16
}

// New returns a CPU in the 8088 reset state (CS:IP = FFFF:0000).
func New(bus Bus) *CPU {
	c := &CPU{Bus: bus}
	c.Reset()
	return c
}

// Reset puts the CPU into its power-on state.
func (c *CPU) Reset() {
	c.R = [8]uint16{}
	c.S = [4]uint16{0, 0xFFFF, 0, 0}
	c.IP = 0
	c.Flags = flagsFixed1
	c.Halted = false
	c.nmi = false
	c.noIntr = false
	c.stiShadow = false
	c.inPrefix = false
}

// NMI latches a non-maskable interrupt request (edge triggered).
func (c *CPU) NMI() { c.nmi = true }

// SetFlags stores a flags value with the 8088's fixed bits applied.
func (c *CPU) SetFlags(v uint16) { c.Flags = v&flagsWrite | flagsFixed1 }

// ---- memory helpers -----------------------------------------------------

// phys forms a 20-bit physical address; the 8088 wraps at 1 MB.
func phys(seg, off uint16) uint32 {
	return (uint32(seg)<<4 + uint32(off)) & 0xFFFFF
}

func (c *CPU) read8(seg int, off uint16) uint8 {
	c.Cycles += 4
	return c.Bus.Read8(phys(c.S[seg], off))
}

// read16 reads a little-endian word; the offset wraps inside the segment
// (a word at offset FFFF takes its high byte from offset 0000).
func (c *CPU) read16(seg int, off uint16) uint16 {
	lo := c.read8(seg, off)
	hi := c.read8(seg, off+1)
	return uint16(lo) | uint16(hi)<<8
}

func (c *CPU) write8(seg int, off uint16, v uint8) {
	c.Cycles += 4
	c.Bus.Write8(phys(c.S[seg], off), v)
}

func (c *CPU) write16(seg int, off uint16, v uint16) {
	c.write8(seg, off, uint8(v))
	c.write8(seg, off+1, uint8(v>>8))
}

func (c *CPU) fetch8() uint8 {
	v := c.read8(CS, c.IP)
	c.IP++
	return v
}

func (c *CPU) fetch16() uint16 {
	lo := c.fetch8()
	hi := c.fetch8()
	return uint16(lo) | uint16(hi)<<8
}

func (c *CPU) push(v uint16) {
	c.R[SP] -= 2
	c.write16(SS, c.R[SP], v)
}

func (c *CPU) pop() uint16 {
	v := c.read16(SS, c.R[SP])
	c.R[SP] += 2
	return v
}

// dataSeg returns the segment for a DS-default access, honouring an
// override prefix.
func (c *CPU) dataSeg() int {
	if c.segOver >= 0 {
		return c.segOver
	}
	return DS
}

// ---- register helpers ---------------------------------------------------

// reg8 reads an 8-bit register by encoding: 0-3 = AL CL DL BL,
// 4-7 = AH CH DH BH.
func (c *CPU) reg8(i uint8) uint8 {
	if i < 4 {
		return uint8(c.R[i])
	}
	return uint8(c.R[i-4] >> 8)
}

func (c *CPU) setReg8(i uint8, v uint8) {
	if i < 4 {
		c.R[i] = c.R[i]&0xFF00 | uint16(v)
	} else {
		c.R[i-4] = c.R[i-4]&0x00FF | uint16(v)<<8
	}
}

// ---- ModRM decoding -----------------------------------------------------

// decodeModRM reads a ModRM byte (and displacement) and computes the
// effective address for memory operands. BP-based modes default to SS.
func (c *CPU) decodeModRM() {
	m := c.fetch8()
	c.mod = m >> 6
	c.reg = m >> 3 & 7
	c.rm = m & 7
	if c.mod == 3 {
		return
	}
	var off uint16
	seg := DS
	switch c.rm {
	case 0:
		off = c.R[BX] + c.R[SI]
	case 1:
		off = c.R[BX] + c.R[DI]
	case 2:
		off = c.R[BP] + c.R[SI]
		seg = SS
	case 3:
		off = c.R[BP] + c.R[DI]
		seg = SS
	case 4:
		off = c.R[SI]
	case 5:
		off = c.R[DI]
	case 6:
		if c.mod == 0 {
			off = c.fetch16() // direct address, DS
		} else {
			off = c.R[BP]
			seg = SS
		}
	case 7:
		off = c.R[BX]
	}
	switch c.mod {
	case 1:
		off += uint16(int16(int8(c.fetch8())))
	case 2:
		off += c.fetch16()
	}
	if c.segOver >= 0 {
		seg = c.segOver
	}
	c.eaSeg = seg
	c.eaOff = off
}

func (c *CPU) rm8() uint8 {
	if c.mod == 3 {
		return c.reg8(c.rm)
	}
	return c.read8(c.eaSeg, c.eaOff)
}

func (c *CPU) setRm8(v uint8) {
	if c.mod == 3 {
		c.setReg8(c.rm, v)
		return
	}
	c.write8(c.eaSeg, c.eaOff, v)
}

func (c *CPU) rm16() uint16 {
	if c.mod == 3 {
		return c.R[c.rm]
	}
	return c.read16(c.eaSeg, c.eaOff)
}

func (c *CPU) setRm16(v uint16) {
	if c.mod == 3 {
		c.R[c.rm] = v
		return
	}
	c.write16(c.eaSeg, c.eaOff, v)
}

// ---- interrupts and stepping --------------------------------------------

// interrupt performs the 8088 interrupt sequence: push FLAGS, CS, IP,
// clear IF and TF, and load CS:IP from the vector table at 0000:vec*4.
// Taking any interrupt ends a HLT (the pushed IP is past the HLT).
func (c *CPU) interrupt(vec uint8) {
	c.Halted = false
	c.push(c.Flags)
	c.Flags &^= FlagIF | FlagTF
	c.push(c.S[CS])
	c.push(c.IP)
	c.IP = c.read16Abs(uint32(vec) * 4)
	c.S[CS] = c.read16Abs(uint32(vec)*4 + 2)
	c.Cycles += 30
}

func (c *CPU) read16Abs(a uint32) uint16 {
	c.Cycles += 8
	return uint16(c.Bus.Read8(a)) | uint16(c.Bus.Read8(a+1&0xFFFFF))<<8
}

// intrPending reports whether an interrupt would be recognised at the next
// instruction boundary: NMI, INTR with IF set, or the single-step trap.
// It is checked between iterations of a REP string instruction, where the
// segment-load and STI shadows no longer apply.
func (c *CPU) intrPending() bool {
	return c.nmi || c.Flags&FlagTF != 0 ||
		c.Flags&FlagIF != 0 && c.Intr != nil && c.Intr.INTR()
}

// Step executes one instruction (or services one pending interrupt, or
// idles one bus cycle while halted) and returns the cycles it took. A REP
// string instruction may span several Steps (see stringOp), as may a run
// of more than maxPrefixes prefix bytes.
func (c *CPU) Step() int {
	start := c.Cycles
	shadow := c.noIntr || c.inPrefix
	switch {
	case c.nmi && !shadow:
		c.nmi = false
		c.interrupt(2)
	case !shadow && !c.stiShadow && c.Flags&FlagIF != 0 && c.Intr != nil && c.Intr.INTR():
		c.interrupt(c.Intr.Ack())
	}
	c.noIntr = false
	c.stiShadow = false
	if c.Halted {
		c.Cycles += 4
		return int(c.Cycles - start)
	}
	trap := c.Flags&FlagTF != 0
	c.stepping, c.stepStart = true, start
	c.execute()
	c.stepping = false
	// Single-step trap after an instruction that started with TF set
	if trap && c.Flags&FlagTF != 0 && !c.noIntr && !c.stiShadow && !c.inPrefix {
		c.interrupt(1)
	}
	return int(c.Cycles - start)
}
