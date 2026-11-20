package cpu

import "testing"

// Hand-written CPU tests that always run (the SingleStepTests suite needs
// downloaded data). They use testBus from cpu_test.go.

const (
	codeSeg    = 0x1000
	stackSeg   = 0x2000
	stackTop   = 0x1000
	handlerSeg = 0x0060 // vector v -> 0060:v*2, which holds NOP; IRET
)

// fakeIntr asserts INTR once more than `after` INTR() queries have been
// made, until acknowledged.
type fakeIntr struct {
	after, calls int
	vec          uint8
	acks         int
}

func (f *fakeIntr) INTR() bool {
	f.calls++
	return f.calls > f.after && f.acks == 0
}

func (f *fakeIntr) Ack() uint8 { f.acks++; return f.vec }

// newTestCPU loads code at 1000:0000 and returns a CPU ready to run it.
func newTestCPU(code ...uint8) (*CPU, *testBus) {
	bus := &testBus{}
	for v := 0; v < 256; v++ {
		off := uint16(v * 2)
		bus.mem[v*4], bus.mem[v*4+1] = uint8(off), uint8(off>>8)
		bus.mem[v*4+2], bus.mem[v*4+3] = handlerSeg&0xFF, handlerSeg>>8
		h := phys(handlerSeg, off)
		bus.mem[h], bus.mem[h+1] = 0x90, 0xCF // NOP; IRET
	}
	copy(bus.mem[phys(codeSeg, 0):], code)
	c := New(bus)
	c.S[CS], c.IP = codeSeg, 0
	c.S[SS], c.R[SP] = stackSeg, stackTop
	c.S[DS], c.S[ES] = 0x3000, 0x4000
	return c, bus
}

func (b *testBus) word(seg, off uint16) uint16 {
	return uint16(b.mem[phys(seg, off)]) | uint16(b.mem[phys(seg, off+1)])<<8
}

func TestResetVector(t *testing.T) {
	c := New(&testBus{})
	if c.S[CS] != 0xFFFF || c.IP != 0 || c.Flags != flagsFixed1 {
		t.Fatalf("reset CS:IP=%04X:%04X flags=%04X", c.S[CS], c.IP, c.Flags)
	}
}

func TestBasicInstructions(t *testing.T) {
	c, bus := newTestCPU(
		0xB8, 0xFF, 0x7F, // MOV AX, 7FFF
		0x05, 0x01, 0x00, // ADD AX, 1 -> 8000, OF SF AF
		0x50,       // PUSH AX
		0x5B,       // POP BX
		0xEB, 0x01, // JMP +1
		0xF4,       // HLT (skipped)
		0xB1, 0x03, // MOV CL, 3
		0xD2, 0xE3, // SHL BL, CL (BL=00 -> 00, ZF)
		0x88, 0x3E, 0x10, 0x00, // MOV [0010], BH
	)
	for i := 0; i < 8; i++ {
		c.Step()
	}
	if c.R[AX] != 0x8000 || c.R[BX] != 0x8000 || c.R[CX]&0xFF != 3 {
		t.Errorf("AX=%04X BX=%04X CX=%04X", c.R[AX], c.R[BX], c.R[CX])
	}
	if c.R[SP] != stackTop || c.IP != 19 || c.Halted {
		t.Errorf("SP=%04X IP=%04X halted=%v", c.R[SP], c.IP, c.Halted)
	}
	if got := bus.mem[phys(0x3000, 0x10)]; got != 0x80 {
		t.Errorf("[DS:0010]=%02X want 80", got)
	}
	if c.Flags&FlagZF == 0 {
		t.Errorf("flags=%04X: SHL 0 should set ZF", c.Flags)
	}
}

func TestDivideErrorTraps(t *testing.T) {
	c, bus := newTestCPU(0xF6, 0xF1) // DIV CL with CL=0
	c.R[AX] = 0x1234
	c.Step()
	if c.S[CS] != handlerSeg || c.IP != 0 { // INT 0 is taken inside the DIV
		t.Fatalf("CS:IP=%04X:%04X want %04X:0000", c.S[CS], c.IP, handlerSeg)
	}
	if ip := bus.word(stackSeg, c.R[SP]); ip != 2 {
		t.Errorf("pushed IP=%04X want 0002", ip)
	}
}

// Under Step, a long REP is split into bounded chunks; the result and the
// total cycle count are the same as running it in one go.
func TestRepStosChunked(t *testing.T) {
	code := []uint8{0xF3, 0xAA, 0xF4} // REP STOSB; HLT
	run := func(stepped bool) (*CPU, *testBus, int) {
		c, bus := newTestCPU(code...)
		c.R[CX], c.R[DI], c.R[AX] = 0xFFFF, 0, 0xA5
		steps := 0
		if !stepped {
			c.execute()
			return c, bus, 1
		}
		for c.IP != 2 {
			if n := c.Step(); n > repCycleBudget+64 {
				t.Fatalf("step %d took %d cycles", steps, n)
			}
			steps++
			if steps > 1000 {
				t.Fatal("REP STOSB did not finish")
			}
		}
		return c, bus, steps
	}
	c, bus, steps := run(true)
	if c.R[CX] != 0 || c.R[DI] != 0xFFFF {
		t.Fatalf("CX=%04X DI=%04X", c.R[CX], c.R[DI])
	}
	for off := 0; off < 0xFFFF; off++ {
		if bus.mem[phys(0x4000, uint16(off))] != 0xA5 {
			t.Fatalf("ES:%04X not written", off)
		}
	}
	if steps < 100 {
		t.Errorf("only %d steps: REP not split", steps)
	}
	d, _, _ := run(false)
	if d.R[CX] != 0 || d.IP != 2 {
		t.Fatalf("execute: CX=%04X IP=%04X, want whole REP", d.R[CX], d.IP)
	}
	if c.Cycles != d.Cycles {
		t.Errorf("stepped cycles %d != direct %d", c.Cycles, d.Cycles)
	}
}

// An interrupt raised during a REP is taken between iterations; the pushed
// IP is the first prefix byte and the instruction resumes after IRET with
// its segment override intact.
func TestRepInterruptible(t *testing.T) {
	c, bus := newTestCPU(0x90, 0x26, 0xF3, 0xA4, 0xF4) // NOP; ES: REP MOVSB; HLT
	for i := 0; i < 10; i++ {
		bus.mem[phys(0x4000, uint16(i))] = uint8(i + 1)
	}
	c.R[CX], c.R[SI], c.R[DI] = 10, 0, 0x100
	c.Flags |= FlagIF
	// Queries: NOP step, REP step start, after iterations 1, 2, 3
	in := &fakeIntr{after: 4, vec: 0x40}
	c.Intr = in
	c.Step() // NOP
	c.Step() // three iterations of MOVSB, then stop
	if c.R[CX] != 7 || c.R[SI] != 3 || c.R[DI] != 0x103 || c.IP != 1 {
		t.Fatalf("after break: CX=%d SI=%04X DI=%04X IP=%04X", c.R[CX], c.R[SI], c.R[DI], c.IP)
	}
	c.Step() // interrupt + handler NOP
	if in.acks != 1 || c.S[CS] != handlerSeg {
		t.Fatalf("interrupt not taken: acks=%d CS=%04X", in.acks, c.S[CS])
	}
	if ip := bus.word(stackSeg, c.R[SP]); ip != 1 {
		t.Errorf("pushed IP=%04X want 0001 (the ES: prefix)", ip)
	}
	c.Step() // IRET
	for c.IP != 4 {
		c.Step()
	}
	if c.R[CX] != 0 {
		t.Fatalf("CX=%d", c.R[CX])
	}
	for i := 0; i < 10; i++ {
		if got := bus.mem[phys(0x4000, uint16(0x100+i))]; got != uint8(i+1) {
			t.Fatalf("ES:%04X=%02X want %02X", 0x100+i, got, i+1)
		}
	}
}

// A 64 KB run of prefix bytes must not hang Step, and interrupts are not
// taken in the middle of a prefix run.
func TestPrefixFloodBounded(t *testing.T) {
	c, bus := newTestCPU()
	for off := 0; off < 0x10000; off++ {
		bus.mem[phys(codeSeg, uint16(off))] = 0x26
	}
	c.Step() // enter the prefix run
	c.NMI()
	for i := 0; i < 100; i++ {
		if n := c.Step(); n > 4*maxPrefixes {
			t.Fatalf("step took %d cycles", n)
		}
	}
	if c.S[CS] != codeSeg || c.R[SP] != stackTop {
		t.Fatalf("interrupt taken inside a prefix run: CS=%04X SP=%04X", c.S[CS], c.R[SP])
	}
}

// A long but finite prefix run keeps its meaning across Steps.
func TestLongPrefixRunSemantics(t *testing.T) {
	var code []uint8
	for i := 0; i < 20; i++ {
		code = append(code, 0x2E) // CS:
	}
	code = append(code, 0x26, 0xA0, 0x00, 0x00) // ES: MOV AL, [0000]
	c, bus := newTestCPU(code...)
	bus.mem[phys(0x4000, 0)] = 0x5A
	bus.mem[phys(0x3000, 0)] = 0x11
	steps := 0
	for c.IP != uint16(len(code)) && steps < 10 {
		c.Step()
		steps++
	}
	if uint8(c.R[AX]) != 0x5A {
		t.Errorf("AL=%02X want 5A (ES override after 20 prefixes)", uint8(c.R[AX]))
	}
	if steps != 2 {
		t.Errorf("%d steps, want 2", steps)
	}
}

// HLT with TF set: the trap is taken and ends the halt.
func TestHltSingleStepTrap(t *testing.T) {
	c, bus := newTestCPU(0xF4)
	c.Flags |= FlagTF
	c.Step()
	if c.Halted {
		t.Fatal("still halted after the single-step trap")
	}
	if c.S[CS] != handlerSeg || c.IP != 2 {
		t.Fatalf("CS:IP=%04X:%04X, want trap handler %04X:0002", c.S[CS], c.IP, handlerSeg)
	}
	if ip := bus.word(stackSeg, c.R[SP]); ip != 1 {
		t.Errorf("pushed IP=%04X want 0001", ip)
	}
	c.Step() // handler NOP runs on the next Step
	if c.IP != 3 {
		t.Errorf("handler not running: IP=%04X", c.IP)
	}
}

func TestHltWokenByIntr(t *testing.T) {
	c, _ := newTestCPU(0xF4, 0x90)
	c.Flags |= FlagIF
	in := &fakeIntr{after: 2, vec: 0x08}
	c.Intr = in
	c.Step() // HLT
	c.Step() // idle
	if !c.Halted {
		t.Fatal("not halted")
	}
	c.Step() // INTR
	if c.Halted || c.S[CS] != handlerSeg {
		t.Fatalf("halted=%v CS=%04X", c.Halted, c.S[CS])
	}
}

// NMI is held off for one instruction after MOV SS / POP SS, but not
// after STI.
func TestNMIShadow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   []uint8
		shadow bool
	}{
		{"MOV SS", []uint8{0x8E, 0xD0, 0x90, 0x90}, true}, // MOV SS, AX
		{"POP SS", []uint8{0x17, 0x90, 0x90}, true},
		{"STI", []uint8{0xFB, 0x90, 0x90}, false},
	} {
		c, bus := newTestCPU(tc.code...)
		c.R[AX] = stackSeg
		bus.mem[phys(stackSeg, stackTop)] = stackSeg & 0xFF
		bus.mem[phys(stackSeg, stackTop+1)] = stackSeg >> 8
		c.Step()
		c.NMI()
		c.Step()
		if took := c.S[CS] == handlerSeg; took == tc.shadow {
			t.Errorf("%s: NMI taken=%v right after it, want %v", tc.name, took, !tc.shadow)
		}
		if tc.shadow {
			c.Step()
			if c.S[CS] != handlerSeg || c.IP != 4+1 {
				t.Errorf("%s: NMI not taken one instruction later: %04X:%04X",
					tc.name, c.S[CS], c.IP)
			}
		}
	}
}

// INTR is held off for one instruction after STI.
func TestSTIShadow(t *testing.T) {
	c, _ := newTestCPU(0xFB, 0x90, 0x90) // STI; NOP; NOP
	in := &fakeIntr{vec: 0x08}
	c.Intr = in
	c.Step() // STI
	c.Step() // NOP: INTR not yet recognised
	if in.acks != 0 || c.IP != 2 {
		t.Fatalf("INTR taken inside the STI shadow (IP=%04X)", c.IP)
	}
	c.Step()
	if in.acks != 1 || c.S[CS] != handlerSeg {
		t.Fatalf("INTR not taken after the shadow")
	}
}
