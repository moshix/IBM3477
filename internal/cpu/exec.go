package cpu

// maxPrefixes bounds the prefix bytes decoded in one call of execute, so
// that a long (or, with IP wrapping, endless) run of prefixes cannot hang
// Step. Real code uses at most a few; a longer run is decoded over
// several calls with the prefix state kept (see CPU.inPrefix).
const maxPrefixes = 16

// execute decodes prefixes and runs one instruction.
func (c *CPU) execute() {
	if !c.inPrefix {
		c.segOver = -1
		c.rep = 0
		c.instStart = c.IP
	}
	c.inPrefix = false
	for n := 0; ; n++ {
		if n == maxPrefixes {
			c.inPrefix = true
			return
		}
		op := c.fetch8()
		switch op {
		case 0x26, 0x2E, 0x36, 0x3E:
			// ES: CS: SS: DS: -- bits 3-4 of the opcode select the segment
			c.segOver = int(op >> 3 & 3)
			continue
		case 0xF0, 0xF1:
			// LOCK (F1 is an undocumented alias); no bus effect here
			continue
		case 0xF2, 0xF3:
			c.rep = op
			continue
		}
		c.exec(op)
		return
	}
}

// cond evaluates condition code cc (low nibble of Jcc opcodes).
func (c *CPU) cond(cc uint8) bool {
	var r bool
	switch cc >> 1 {
	case 0: // O
		r = c.flag(FlagOF)
	case 1: // B / C
		r = c.flag(FlagCF)
	case 2: // E / Z
		r = c.flag(FlagZF)
	case 3: // BE
		r = c.flag(FlagCF) || c.flag(FlagZF)
	case 4: // S
		r = c.flag(FlagSF)
	case 5: // P
		r = c.flag(FlagPF)
	case 6: // L: SF != OF
		r = c.flag(FlagSF) != c.flag(FlagOF)
	case 7: // LE: ZF or SF != OF
		r = c.flag(FlagZF) || c.flag(FlagSF) != c.flag(FlagOF)
	}
	// Odd condition codes are the negations of the even ones
	if cc&1 != 0 {
		return !r
	}
	return r
}

func (c *CPU) jumpRel8(take bool) {
	rel := int8(c.fetch8())
	if take {
		c.IP += uint16(int16(rel))
		c.Cycles += 8
	}
}

// exec runs the instruction with opcode op (prefixes already consumed).
func (c *CPU) exec(op uint8) {
	// ---- 00-3F: ALU forms, segment push/pop, BCD adjust ----
	if op < 0x40 {
		switch op & 7 {
		case 0, 1, 2, 3, 4, 5:
			c.aluForm(op)
			return
		}
		switch op {
		case 0x06, 0x0E, 0x16, 0x1E: // PUSH sreg
			c.push(c.S[op>>3])
		case 0x07, 0x0F, 0x17, 0x1F: // POP sreg (0F = POP CS on 8088)
			c.S[op>>3] = c.pop()
			c.noIntr = true
		case 0x27:
			c.daa()
		case 0x2F:
			c.das()
		case 0x37:
			c.aaa()
		case 0x3F:
			c.aas()
		}
		return
	}

	switch {
	case op <= 0x47: // INC r16
		c.R[op&7] = c.inc16(c.R[op&7])
		return
	case op <= 0x4F: // DEC r16
		c.R[op&7] = c.dec16(c.R[op&7])
		return
	case op <= 0x57: // PUSH r16; PUSH SP pushes the already-decremented SP
		c.R[SP] -= 2
		c.write16(SS, c.R[SP], c.R[op&7])
		return
	case op <= 0x5F: // POP r16 (POP SP loads the popped value)
		v := c.pop()
		c.R[op&7] = v
		return
	case op <= 0x7F: // Jcc rel8 (60-6F are 8088 aliases of 70-7F)
		c.jumpRel8(c.cond(op & 0x0F))
		return
	case op >= 0x91 && op <= 0x97: // XCHG AX, r16
		c.R[AX], c.R[op&7] = c.R[op&7], c.R[AX]
		return
	case op >= 0xB0 && op <= 0xB7: // MOV r8, imm8
		c.setReg8(op&7, c.fetch8())
		return
	case op >= 0xB8 && op <= 0xBF: // MOV r16, imm16
		c.R[op&7] = c.fetch16()
		return
	case op >= 0xD8 && op <= 0xDF: // ESC (8087 escape): decode, no-op
		c.decodeModRM()
		return
	}

	switch op {
	case 0x80, 0x82: // group 1, r/m8, imm8 (82 is an alias of 80)
		c.decodeModRM()
		a := c.rm8()
		r := c.alu8(int(c.reg), a, c.fetch8())
		if c.reg != opCMP {
			c.setRm8(r)
		}
	case 0x81: // group 1, r/m16, imm16
		c.decodeModRM()
		a := c.rm16()
		r := c.alu16(int(c.reg), a, c.fetch16())
		if c.reg != opCMP {
			c.setRm16(r)
		}
	case 0x83: // group 1, r/m16, sign-extended imm8
		c.decodeModRM()
		a := c.rm16()
		r := c.alu16(int(c.reg), a, uint16(int16(int8(c.fetch8()))))
		if c.reg != opCMP {
			c.setRm16(r)
		}
	case 0x84: // TEST r/m8, r8
		c.decodeModRM()
		c.alu8(opAND, c.rm8(), c.reg8(c.reg))
	case 0x85:
		c.decodeModRM()
		c.alu16(opAND, c.rm16(), c.R[c.reg])
	case 0x86: // XCHG r/m8, r8
		c.decodeModRM()
		a := c.rm8()
		c.setRm8(c.reg8(c.reg))
		c.setReg8(c.reg, a)
	case 0x87:
		c.decodeModRM()
		a := c.rm16()
		c.setRm16(c.R[c.reg])
		c.R[c.reg] = a
	case 0x88: // MOV r/m8, r8
		c.decodeModRM()
		c.setRm8(c.reg8(c.reg))
	case 0x89:
		c.decodeModRM()
		c.setRm16(c.R[c.reg])
	case 0x8A: // MOV r8, r/m8
		c.decodeModRM()
		c.setReg8(c.reg, c.rm8())
	case 0x8B:
		c.decodeModRM()
		c.R[c.reg] = c.rm16()
	case 0x8C: // MOV r/m16, sreg (only 2 reg bits are decoded)
		c.decodeModRM()
		c.setRm16(c.S[c.reg&3])
	case 0x8D: // LEA (register form leaves the destination unchanged)
		c.decodeModRM()
		if c.mod != 3 {
			c.R[c.reg] = c.eaOff
		}
	case 0x8E: // MOV sreg, r/m16 (MOV CS works on the 8088)
		c.decodeModRM()
		c.S[c.reg&3] = c.rm16()
		c.noIntr = true
	case 0x8F: // POP r/m16
		c.decodeModRM()
		v := c.pop()
		c.setRm16(v)
	case 0x90: // NOP (XCHG AX, AX)
	case 0x98: // CBW
		c.R[AX] = uint16(int16(int8(c.R[AX])))
	case 0x99: // CWD
		if c.R[AX]&0x8000 != 0 {
			c.R[DX] = 0xFFFF
		} else {
			c.R[DX] = 0
		}
	case 0x9A: // CALL far ptr16:16
		off := c.fetch16()
		seg := c.fetch16()
		c.push(c.S[CS])
		c.push(c.IP)
		c.S[CS], c.IP = seg, off
	case 0x9B: // WAIT: no coprocessor, TEST pin always ready
	case 0x9C: // PUSHF
		c.push(c.Flags)
	case 0x9D: // POPF
		c.SetFlags(c.pop())
	case 0x9E: // SAHF: SF ZF AF PF CF from AH
		c.Flags = c.Flags&0xFF00 | uint16(c.reg8(4))&0xD5 | 0x02
	case 0x9F: // LAHF
		c.setReg8(4, uint8(c.Flags))
	case 0xA0: // MOV AL, [moffs]
		c.setReg8(0, c.read8(c.dataSeg(), c.fetch16()))
	case 0xA1:
		c.R[AX] = c.read16(c.dataSeg(), c.fetch16())
	case 0xA2: // MOV [moffs], AL
		c.write8(c.dataSeg(), c.fetch16(), uint8(c.R[AX]))
	case 0xA3:
		c.write16(c.dataSeg(), c.fetch16(), c.R[AX])
	case 0xA4, 0xA5, 0xA6, 0xA7, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF:
		c.stringOp(op)
	case 0xA8: // TEST AL, imm8
		c.alu8(opAND, uint8(c.R[AX]), c.fetch8())
	case 0xA9:
		c.alu16(opAND, c.R[AX], c.fetch16())
	case 0xC0, 0xC2: // RET imm16 (C0 is an 8088 alias)
		n := c.fetch16()
		c.IP = c.pop()
		c.R[SP] += n
	case 0xC1, 0xC3: // RET (C1 alias)
		c.IP = c.pop()
	case 0xC4, 0xC5: // LES / LDS r16, m16:16
		c.decodeModRM()
		off := c.rm16()
		seg := c.read16(c.eaSeg, c.eaOff+2)
		c.R[c.reg] = off
		if op == 0xC4 {
			c.S[ES] = seg
		} else {
			c.S[DS] = seg
		}
	case 0xC6: // MOV r/m8, imm8 (reg field ignored)
		c.decodeModRM()
		c.setRm8(c.fetch8())
	case 0xC7:
		c.decodeModRM()
		c.setRm16(c.fetch16())
	case 0xC8, 0xCA: // RETF imm16 (C8 alias)
		n := c.fetch16()
		c.IP = c.pop()
		c.S[CS] = c.pop()
		c.R[SP] += n
	case 0xC9, 0xCB: // RETF (C9 alias)
		c.IP = c.pop()
		c.S[CS] = c.pop()
	case 0xCC: // INT3
		c.interrupt(3)
	case 0xCD: // INT imm8
		c.interrupt(c.fetch8())
	case 0xCE: // INTO
		if c.flag(FlagOF) {
			c.interrupt(4)
		}
	case 0xCF: // IRET
		c.IP = c.pop()
		c.S[CS] = c.pop()
		c.SetFlags(c.pop())
	case 0xD0, 0xD1, 0xD2, 0xD3: // shift/rotate group
		c.decodeModRM()
		count := uint8(1)
		if op >= 0xD2 {
			count = uint8(c.R[CX])
		}
		c.Cycles += 4 * uint64(count)
		if op&1 == 0 {
			c.setRm8(uint8(c.shift(c.reg, uint32(c.rm8()), count, 8)))
		} else {
			c.setRm16(uint16(c.shift(c.reg, uint32(c.rm16()), count, 16)))
		}
	case 0xD4:
		c.aam(c.fetch8())
	case 0xD5:
		c.aad(c.fetch8())
	case 0xD6: // SALC (undocumented): AL = CF ? FF : 00
		if c.flag(FlagCF) {
			c.setReg8(0, 0xFF)
		} else {
			c.setReg8(0, 0)
		}
	case 0xD7: // XLAT
		c.setReg8(0, c.read8(c.dataSeg(), c.R[BX]+uint16(uint8(c.R[AX]))))
	case 0xE0: // LOOPNZ
		c.R[CX]--
		c.jumpRel8(c.R[CX] != 0 && !c.flag(FlagZF))
	case 0xE1: // LOOPZ
		c.R[CX]--
		c.jumpRel8(c.R[CX] != 0 && c.flag(FlagZF))
	case 0xE2: // LOOP
		c.R[CX]--
		c.jumpRel8(c.R[CX] != 0)
	case 0xE3: // JCXZ
		c.jumpRel8(c.R[CX] == 0)
	case 0xE4: // IN AL, imm8
		c.setReg8(0, c.in8(uint16(c.fetch8())))
	case 0xE5:
		p := uint16(c.fetch8())
		c.R[AX] = uint16(c.in8(p)) | uint16(c.in8(p+1))<<8
	case 0xE6: // OUT imm8, AL
		c.out8(uint16(c.fetch8()), uint8(c.R[AX]))
	case 0xE7:
		p := uint16(c.fetch8())
		c.out8(p, uint8(c.R[AX]))
		c.out8(p+1, uint8(c.R[AX]>>8))
	case 0xE8: // CALL rel16
		rel := c.fetch16()
		c.push(c.IP)
		c.IP += rel
	case 0xE9: // JMP rel16
		rel := c.fetch16()
		c.IP += rel
	case 0xEA: // JMP far ptr16:16
		off := c.fetch16()
		seg := c.fetch16()
		c.S[CS], c.IP = seg, off
	case 0xEB: // JMP rel8
		c.jumpRel8(true)
	case 0xEC: // IN AL, DX
		c.setReg8(0, c.in8(c.R[DX]))
	case 0xED:
		c.R[AX] = uint16(c.in8(c.R[DX])) | uint16(c.in8(c.R[DX]+1))<<8
	case 0xEE: // OUT DX, AL
		c.out8(c.R[DX], uint8(c.R[AX]))
	case 0xEF:
		c.out8(c.R[DX], uint8(c.R[AX]))
		c.out8(c.R[DX]+1, uint8(c.R[AX]>>8))
	case 0xF4: // HLT
		c.Halted = true
	case 0xF5: // CMC
		c.Flags ^= FlagCF
	case 0xF6, 0xF7:
		c.group3(op == 0xF7)
	case 0xF8:
		c.Flags &^= FlagCF
	case 0xF9:
		c.Flags |= FlagCF
	case 0xFA:
		c.Flags &^= FlagIF
	case 0xFB: // STI: interrupts are recognised after the next instruction
		c.Flags |= FlagIF
		c.stiShadow = true
	case 0xFC:
		c.Flags &^= FlagDF
	case 0xFD:
		c.Flags |= FlagDF
	case 0xFE: // group 4: INC/DEC r/m8 (/2-/7 undefined, treated as no-op)
		c.decodeModRM()
		switch c.reg {
		case 0:
			c.setRm8(c.inc8(c.rm8()))
		case 1:
			c.setRm8(c.dec8(c.rm8()))
		}
	case 0xFF:
		c.group5()
	}
}

func (c *CPU) in8(port uint16) uint8 {
	c.Cycles += 4
	return c.Bus.In8(port)
}

func (c *CPU) out8(port uint16, v uint8) {
	c.Cycles += 4
	c.Bus.Out8(port, v)
}

// aluForm handles opcodes 00-3F whose low 3 bits are 0-5:
// op>>3 selects ADD OR ADC SBB AND SUB XOR CMP; op&7 selects the operand
// form: 0 r/m8,r8  1 r/m16,r16  2 r8,r/m8  3 r16,r/m16  4 AL,imm8  5 AX,imm16.
func (c *CPU) aluForm(op uint8) {
	f := int(op >> 3)
	switch op & 7 {
	case 0:
		c.decodeModRM()
		r := c.alu8(f, c.rm8(), c.reg8(c.reg))
		if f != opCMP {
			c.setRm8(r)
		}
	case 1:
		c.decodeModRM()
		r := c.alu16(f, c.rm16(), c.R[c.reg])
		if f != opCMP {
			c.setRm16(r)
		}
	case 2:
		c.decodeModRM()
		r := c.alu8(f, c.reg8(c.reg), c.rm8())
		if f != opCMP {
			c.setReg8(c.reg, r)
		}
	case 3:
		c.decodeModRM()
		r := c.alu16(f, c.R[c.reg], c.rm16())
		if f != opCMP {
			c.R[c.reg] = r
		}
	case 4:
		r := c.alu8(f, uint8(c.R[AX]), c.fetch8())
		if f != opCMP {
			c.setReg8(0, r)
		}
	case 5:
		r := c.alu16(f, c.R[AX], c.fetch16())
		if f != opCMP {
			c.R[AX] = r
		}
	}
}

// stringOp runs MOVS CMPS STOS LODS SCAS, with REP/REPE/REPNE repetition.
// Source is DS:SI (override allowed), destination is always ES:DI.
// A direct call of execute runs every iteration of a repeated instruction;
// under Step it may stop early and resume on the next Step.
func (c *CPU) stringOp(op uint8) {
	word := op&1 != 0
	step := uint16(1)
	if word {
		step = 2
	}
	if c.flag(FlagDF) {
		step = -step
	}
	src := c.dataSeg()
	one := func() {
		switch op &^ 1 {
		case 0xA4: // MOVS
			if word {
				c.write16(ES, c.R[DI], c.read16(src, c.R[SI]))
			} else {
				c.write8(ES, c.R[DI], c.read8(src, c.R[SI]))
			}
			c.R[SI] += step
			c.R[DI] += step
		case 0xA6: // CMPS: [src:SI] - [ES:DI]
			if word {
				a := c.read16(src, c.R[SI])
				c.alu16(opSUB, a, c.read16(ES, c.R[DI]))
			} else {
				a := c.read8(src, c.R[SI])
				c.alu8(opSUB, a, c.read8(ES, c.R[DI]))
			}
			c.R[SI] += step
			c.R[DI] += step
		case 0xAA: // STOS
			if word {
				c.write16(ES, c.R[DI], c.R[AX])
			} else {
				c.write8(ES, c.R[DI], uint8(c.R[AX]))
			}
			c.R[DI] += step
		case 0xAC: // LODS
			if word {
				c.R[AX] = c.read16(src, c.R[SI])
			} else {
				c.setReg8(0, c.read8(src, c.R[SI]))
			}
			c.R[SI] += step
		case 0xAE: // SCAS: AL/AX - [ES:DI]
			if word {
				c.alu16(opSUB, c.R[AX], c.read16(ES, c.R[DI]))
			} else {
				c.alu8(opSUB, uint8(c.R[AX]), c.read8(ES, c.R[DI]))
			}
			c.R[DI] += step
		}
	}
	if c.rep == 0 {
		one()
		return
	}
	compares := op&^1 == 0xA6 || op&^1 == 0xAE
	for c.R[CX] != 0 {
		one()
		c.R[CX]--
		c.Cycles += 8
		// REPE stops when ZF=0, REPNE when ZF=1 (only for CMPS/SCAS)
		if compares && c.flag(FlagZF) != (c.rep == 0xF3) {
			break
		}
		if c.R[CX] == 0 || !c.stepping {
			continue
		}
		// Under Step, stop between iterations so that the interrupt is
		// taken (as the 8088 does) or the board's timers can advance;
		// the next Step re-decodes the instruction from its first
		// prefix and carries on with the updated CX/SI/DI.
		if c.intrPending() {
			c.IP = c.instStart
			return
		}
		if c.Cycles-c.stepStart >= repCycleBudget {
			// An emulator artefact, not a hardware event: credit the
			// cycles of fetching the instruction again.
			if n := 4 * uint64(c.IP-c.instStart); n < c.Cycles-c.stepStart {
				c.Cycles -= n
			}
			c.IP = c.instStart
			return
		}
	}
}

// repCycleBudget is how many cycles one Step may spend in a REP string
// instruction before stopping it to let the board's devices run (about
// 0.2 ms; the shortest device window, vertical sync, is 1,869 cycles).
const repCycleBudget = 1024

// group3 handles F6/F7: TEST NOT NEG MUL IMUL DIV IDIV (/1 aliases TEST).
func (c *CPU) group3(word bool) {
	c.decodeModRM()
	switch c.reg {
	case 0, 1: // TEST r/m, imm
		if word {
			c.alu16(opAND, c.rm16(), c.fetch16())
		} else {
			c.alu8(opAND, c.rm8(), c.fetch8())
		}
	case 2: // NOT (no flags)
		if word {
			c.setRm16(^c.rm16())
		} else {
			c.setRm8(^c.rm8())
		}
	case 3: // NEG = 0 - x; CF set unless x == 0
		if word {
			c.setRm16(c.alu16(opSUB, 0, c.rm16()))
		} else {
			c.setRm8(c.alu8(opSUB, 0, c.rm8()))
		}
	case 4:
		c.mul(word, false)
	case 5:
		c.mul(word, true)
	case 6:
		c.div(word, false)
	case 7:
		c.div(word, true)
	}
}

// mul implements MUL/IMUL. CF=OF=1 when the high half is significant
// (MUL: nonzero; IMUL: not just the sign extension of the low half).
func (c *CPU) mul(word, signed bool) {
	c.Cycles += 70
	var hiSig bool
	if !word {
		src := c.rm8()
		al := uint8(c.R[AX])
		var r uint16
		if signed {
			r = uint16(int16(int8(al)) * int16(int8(src)))
			hiSig = int16(r) != int16(int8(r))
		} else {
			r = uint16(al) * uint16(src)
			hiSig = r>>8 != 0
		}
		c.R[AX] = r
		c.setSZP8(uint8(r >> 8))
	} else {
		src := c.rm16()
		var r uint32
		if signed {
			r = uint32(int32(int16(c.R[AX])) * int32(int16(src)))
			hiSig = int32(r) != int32(int16(r))
		} else {
			r = uint32(c.R[AX]) * uint32(src)
			hiSig = r>>16 != 0
		}
		c.R[AX] = uint16(r)
		c.R[DX] = uint16(r >> 16)
		c.setSZP16(uint16(r >> 16))
	}
	c.setFlag(FlagCF, hiSig)
	c.setFlag(FlagOF, hiSig)
}

// div implements DIV/IDIV. A zero divisor or a quotient that does not
// fit raises INT 0, whose pushed IP points after the DIV on the 8088.
// The 8088's IDIV also faults on the most negative quotient (-128 /
// -32768), which later CPUs accept.
func (c *CPU) div(word, signed bool) {
	c.Cycles += 80
	if !word {
		src := c.rm8()
		if src == 0 {
			c.interrupt(0)
			return
		}
		if signed {
			n := int32(int16(c.R[AX]))
			d := int32(int8(src))
			q, r := n/d, n%d
			if q > 127 || q < -127 {
				c.interrupt(0)
				return
			}
			// 8088 quirk: a REP/REPNE prefix makes IDIV negate the
			// quotient (the microcode reuses the prefix's sign latch)
			if c.rep != 0 {
				q = -q
			}
			c.R[AX] = uint16(uint8(r))<<8 | uint16(uint8(q))
		} else {
			n := uint32(c.R[AX])
			q, r := n/uint32(src), n%uint32(src)
			if q > 0xFF {
				c.interrupt(0)
				return
			}
			c.R[AX] = uint16(r)<<8 | uint16(q)
		}
		return
	}
	src := c.rm16()
	if src == 0 {
		c.interrupt(0)
		return
	}
	n := uint32(c.R[DX])<<16 | uint32(c.R[AX])
	if signed {
		sn := int64(int32(n))
		d := int64(int16(src))
		q, r := sn/d, sn%d
		if q > 32767 || q < -32767 {
			c.interrupt(0)
			return
		}
		if c.rep != 0 { // REP quirk, see the byte form above
			q = -q
		}
		c.R[AX] = uint16(q)
		c.R[DX] = uint16(r)
	} else {
		q, r := n/uint32(src), n%uint32(src)
		if q > 0xFFFF {
			c.interrupt(0)
			return
		}
		c.R[AX] = uint16(q)
		c.R[DX] = uint16(r)
	}
}

// group5 handles FF: INC DEC CALL CALLF JMP JMPF PUSH (/7 aliases PUSH).
func (c *CPU) group5() {
	c.decodeModRM()
	switch c.reg {
	case 0:
		c.setRm16(c.inc16(c.rm16()))
	case 1:
		c.setRm16(c.dec16(c.rm16()))
	case 2: // CALL near r/m16
		t := c.rm16()
		c.push(c.IP)
		c.IP = t
	case 3: // CALL far m16:16
		off := c.rm16()
		seg := c.read16(c.eaSeg, c.eaOff+2)
		c.push(c.S[CS])
		c.push(c.IP)
		c.S[CS], c.IP = seg, off
	case 4: // JMP near r/m16
		c.IP = c.rm16()
	case 5: // JMP far m16:16
		off := c.rm16()
		seg := c.read16(c.eaSeg, c.eaOff+2)
		c.S[CS], c.IP = seg, off
	case 6, 7: // PUSH r/m16; PUSH SP pushes the decremented SP
		if c.mod == 3 && c.rm == SP {
			c.R[SP] -= 2
			c.write16(SS, c.R[SP], c.R[SP])
			return
		}
		c.push(c.rm16())
	}
}

// ---- BCD adjust instructions (8086 semantics) -----------------------------

func (c *CPU) daa() {
	al := uint8(c.R[AX])
	oldAL, oldCF := al, c.flag(FlagCF)
	// 8088 quirk (measured on hardware): the high-nibble adjust triggers
	// above 0x99 when AF was clear on entry but above 0x9F when AF was set.
	limit := uint8(0x99)
	if c.flag(FlagAF) {
		limit = 0x9F
	}
	if al&0x0F > 9 || c.flag(FlagAF) {
		al += 6
		c.Flags |= FlagAF
	} else {
		c.Flags &^= FlagAF
	}
	if oldAL > limit || oldCF {
		al += 0x60
		c.Flags |= FlagCF
	} else {
		c.Flags &^= FlagCF
	}
	c.setReg8(0, al)
	c.setSZP8(al)
}

func (c *CPU) das() {
	al := uint8(c.R[AX])
	oldAL, oldCF := al, c.flag(FlagCF)
	// 8088 quirk (measured on hardware): the high-nibble adjust triggers
	// above 0x99 when AF was clear on entry but above 0x9F when AF was set.
	limit := uint8(0x99)
	if c.flag(FlagAF) {
		limit = 0x9F
	}
	if al&0x0F > 9 || c.flag(FlagAF) {
		al -= 6
		c.Flags |= FlagAF
	} else {
		c.Flags &^= FlagAF
	}
	if oldAL > limit || oldCF {
		al -= 0x60
		c.Flags |= FlagCF
	} else {
		c.Flags &^= FlagCF
	}
	c.setReg8(0, al)
	c.setSZP8(al)
}

// aaa: on the 8086 the +6 on AL does not carry into AH; AH gets +1.
func (c *CPU) aaa() {
	al, ah := uint8(c.R[AX]), uint8(c.R[AX]>>8)
	if al&0x0F > 9 || c.flag(FlagAF) {
		al += 6
		ah++
		c.Flags |= FlagAF | FlagCF
	} else {
		c.Flags &^= FlagAF | FlagCF
	}
	c.R[AX] = uint16(ah)<<8 | uint16(al&0x0F)
}

func (c *CPU) aas() {
	al, ah := uint8(c.R[AX]), uint8(c.R[AX]>>8)
	if al&0x0F > 9 || c.flag(FlagAF) {
		al -= 6
		ah--
		c.Flags |= FlagAF | FlagCF
	} else {
		c.Flags &^= FlagAF | FlagCF
	}
	c.R[AX] = uint16(ah)<<8 | uint16(al&0x0F)
}

// aam: AH = AL / base, AL = AL % base; base 0 raises INT 0.
func (c *CPU) aam(base uint8) {
	c.Cycles += 80
	if base == 0 {
		// The 8088 sets SF/ZF/PF as for a zero result before trapping
		c.Flags &^= FlagCF | FlagOF | FlagAF
		c.setSZP8(0)
		c.interrupt(0)
		return
	}
	al := uint8(c.R[AX])
	c.R[AX] = uint16(al/base)<<8 | uint16(al%base)
	c.Flags &^= FlagCF | FlagOF | FlagAF
	c.setSZP8(al % base)
}

// aad: AL = AL + AH*base, AH = 0; flags come from that addition.
func (c *CPU) aad(base uint8) {
	c.Cycles += 60
	al, ah := uint8(c.R[AX]), uint8(c.R[AX]>>8)
	r := c.alu8(opADD, al, ah*base)
	c.R[AX] = uint16(r)
}
