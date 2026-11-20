package cpu

import "math/bits"

// ALU operation numbers as encoded in opcodes 00-3F and group 80-83.
const (
	opADD = iota
	opOR
	opADC
	opSBB
	opAND
	opSUB
	opXOR
	opCMP
)

func (c *CPU) setFlag(f uint16, on bool) {
	if on {
		c.Flags |= f
	} else {
		c.Flags &^= f
	}
}

func (c *CPU) flag(f uint16) bool { return c.Flags&f != 0 }

// setSZP8/16 set SF, ZF and PF from a result. PF reflects only the low
// byte (even number of set bits => PF=1).
func (c *CPU) setSZP8(r uint8) {
	c.setFlag(FlagSF, r&0x80 != 0)
	c.setFlag(FlagZF, r == 0)
	c.setFlag(FlagPF, bits.OnesCount8(r)&1 == 0)
}

func (c *CPU) setSZP16(r uint16) {
	c.setFlag(FlagSF, r&0x8000 != 0)
	c.setFlag(FlagZF, r == 0)
	c.setFlag(FlagPF, bits.OnesCount8(uint8(r))&1 == 0)
}

// alu8 performs one of the eight ALU operations on bytes and sets flags.
// The result is returned even for CMP (the caller discards it).
func (c *CPU) alu8(op int, a, b uint8) uint8 {
	return uint8(c.alu(op, uint32(a), uint32(b), 8))
}

func (c *CPU) alu16(op int, a, b uint16) uint16 {
	return uint16(c.alu(op, uint32(a), uint32(b), 16))
}

// alu is the width-generic ALU. Carry/overflow rules:
//   - ADD/ADC: CF = carry out of the top bit; OF = both operands have the
//     same sign and the result's sign differs.
//   - SUB/SBB/CMP: CF = borrow; OF = operands have different signs and the
//     result's sign differs from the minuend.
//   - AF is the carry/borrow out of bit 3 in every arithmetic case.
//   - Logical ops clear CF/OF; AF is architecturally undefined and the
//     8088 clears it.
func (c *CPU) alu(op int, a, b uint32, width uint) uint32 {
	mask := uint32(1)<<width - 1
	sign := uint32(1) << (width - 1)
	var r uint32
	switch op {
	case opADD, opADC:
		cin := uint32(0)
		if op == opADC && c.flag(FlagCF) {
			cin = 1
		}
		r = a + b + cin
		c.setFlag(FlagCF, r > mask)
		c.setFlag(FlagOF, (a^r)&(b^r)&sign != 0)
		c.setFlag(FlagAF, (a^b^r)&0x10 != 0)
	case opSUB, opSBB, opCMP:
		cin := uint32(0)
		if op == opSBB && c.flag(FlagCF) {
			cin = 1
		}
		r = a - b - cin
		c.setFlag(FlagCF, a < b+cin)
		c.setFlag(FlagOF, (a^b)&(a^r)&sign != 0)
		c.setFlag(FlagAF, (a^b^r)&0x10 != 0)
	case opOR:
		r = a | b
		c.logicFlags()
	case opAND:
		r = a & b
		c.logicFlags()
	case opXOR:
		r = a ^ b
		c.logicFlags()
	}
	r &= mask
	if width == 8 {
		c.setSZP8(uint8(r))
	} else {
		c.setSZP16(uint16(r))
	}
	return r
}

func (c *CPU) logicFlags() {
	c.Flags &^= FlagCF | FlagOF | FlagAF
}

// inc/dec leave CF untouched.
func (c *CPU) inc8(a uint8) uint8 {
	cf := c.Flags & FlagCF
	r := c.alu8(opADD, a, 1)
	c.Flags = c.Flags&^FlagCF | cf
	return r
}

func (c *CPU) dec8(a uint8) uint8 {
	cf := c.Flags & FlagCF
	r := c.alu8(opSUB, a, 1)
	c.Flags = c.Flags&^FlagCF | cf
	return r
}

func (c *CPU) inc16(a uint16) uint16 {
	cf := c.Flags & FlagCF
	r := c.alu16(opADD, a, 1)
	c.Flags = c.Flags&^FlagCF | cf
	return r
}

func (c *CPU) dec16(a uint16) uint16 {
	cf := c.Flags & FlagCF
	r := c.alu16(opSUB, a, 1)
	c.Flags = c.Flags&^FlagCF | cf
	return r
}

// shift implements group D0-D3: ROL ROR RCL RCR SHL SHR SETMO SAR.
//
// The 8088 does not mask the count (CL up to 255 runs in full), so the
// operation is applied one bit at a time like the microcode does. OF is
// the value computed by the final single-bit step. A zero count changes
// nothing, not even flags. Rotates leave SF/ZF/PF/AF alone; shifts set
// SF/ZF/PF from the result. SETMO (/6, undocumented) sets the operand to
// all ones.
func (c *CPU) shift(op uint8, v uint32, count uint8, width uint) uint32 {
	if count == 0 {
		return v
	}
	mask := uint32(1)<<width - 1
	msb := uint32(1) << (width - 1)
	cf := c.flag(FlagCF)
	of := c.flag(FlagOF)
	for i := uint8(0); i < count; i++ {
		switch op {
		case 0: // ROL
			cf = v&msb != 0
			v = (v<<1 | b2u(cf)) & mask
			of = (v&msb != 0) != cf
		case 1: // ROR
			cf = v&1 != 0
			v = v>>1 | b2u(cf)<<(width-1)
			of = (v&msb != 0) != (v&(msb>>1) != 0)
		case 2: // RCL
			in := b2u(cf)
			cf = v&msb != 0
			v = (v<<1 | in) & mask
			of = (v&msb != 0) != cf
		case 3: // RCR
			in := b2u(cf)
			cf = v&1 != 0
			v = v>>1 | in<<(width-1)
			of = (v&msb != 0) != (v&(msb>>1) != 0)
		case 4: // SHL/SAL
			cf = v&msb != 0
			v = v << 1 & mask
			of = (v&msb != 0) != cf
		case 5: // SHR
			cf = v&1 != 0
			v >>= 1
			of = (v&msb != 0) != (v&(msb>>1) != 0)
		case 6: // SETMO
			v = mask
			cf = false
			of = false
		case 7: // SAR
			cf = v&1 != 0
			v = v>>1 | v&msb
			of = false
		}
	}
	c.setFlag(FlagCF, cf)
	c.setFlag(FlagOF, of)
	if op >= 4 {
		if width == 8 {
			c.setSZP8(uint8(v))
		} else {
			c.setSZP16(uint16(v))
		}
		// AF is undefined for shifts; the 8088 leaves it cleared except
		// for SETMO, which also clears it.
		c.Flags &^= FlagAF
	}
	return v
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
