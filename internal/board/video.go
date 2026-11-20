package board

import "ibm3486/internal/cpu"

// Video is the display gate array (CRTC) at 0x20100-0x201FF.
type Video struct {
	b     *Board
	regs  [0x100]uint8
	known [0x100]bool

	// frame timing: a 60 Hz frame at the CPU clock, with vertical sync
	// asserted for the last vsyncCycles of each frame
	frameCycles int
	vsyncCycles int
	pos         int

	// engineBusy is set while a range operation started through
	// 0x20138 is in progress (see engineStart)
	engineBusy bool
}

// cpuHz is the 8088 clock. The board crystal is 59.8185 MHz (MAME notes);
// an N8088-2 at crystal/8 runs at about 7.48 MHz.
const cpuHz = 59818500 / 8

// statusReg is the CRTC status register (0x2012F); bit 3 toggles with
// vertical sync (the firmware waits on it before touching video RAM).
const statusReg = 0x2F

func newVideo(b *Board) *Video {
	v := &Video{b: b, frameCycles: cpuHz / 60}
	// vertical sync lasts about 0.25 ms: POST test 5 needs it to stay high
	// for only 10-30 polling loops after starting a measurement
	v.vsyncCycles = cpuHz / 4000
	v.known[statusReg] = true
	return v
}

// POST test 5 (F000:26FA-2D33) checks the video circuitry with a
// video-signal analyser: after drawing each test pattern it starts the
// analyser (0x2012F bit7) and reads a 4-bit signature from 0x2012F bits
// 4-7, comparing it with the expected value at [BP+11h] of its stack
// frame (F000:2A37; 29D1 accepts any nonzero value). The analyser itself
// is not modelled: while test 5 runs, the register returns the signature
// an intact display produces, i.e. the expected one. The test otherwise
// runs exactly as on real hardware, including everything it draws.
const (
	videoTestSeg   = 0xF000
	videoTestStart = 0x26FA
	videoTestEnd   = 0x2D34
)

// analyserSignature returns the analyser reading for the running test 5
// sub-test, or false outside test 5.
func (v *Video) analyserSignature() (uint8, bool) {
	c := v.b.CPU
	if c.S[cpu.CS] != videoTestSeg || c.IP < videoTestStart || c.IP >= videoTestEnd {
		return 0, false
	}
	frame := (uint32(c.S[cpu.SS])<<4 + uint32(c.R[cpu.BP]+0x11)) & 0xFFFFF
	return v.b.mem[frame] & 0x0F, true
}

// Read returns a register value.
func (v *Video) Read(off uint32) uint8 {
	if off == statusReg {
		// bits 0-1 are the I2C bus lines (i2c.go), bit 3 vertical sync
		r := v.regs[off]&^0x0B | v.b.I2C.Lines()
		if v.pos >= v.frameCycles-v.vsyncCycles {
			r |= 0x08
		}
		if sig, ok := v.analyserSignature(); ok {
			r = r&0x0F | sig<<4
		}
		v.b.trace(mmioBase+0x100+off, false, 0)
		return r
	}
	if !v.known[off] {
		v.b.trace(mmioBase+0x100+off, false, 0)
	}
	return v.regs[off]
}

// Write stores a register value.
func (v *Video) Write(off uint32, val uint8) {
	if !v.known[off] {
		v.b.trace(mmioBase+0x100+off, true, val)
	}
	v.regs[off] = val
	switch off {
	case statusReg:
		v.b.I2C.Drive(val)
	case regEngineStart:
		v.engineBusy = true
	case regEngineAck:
		v.b.IO.videoIRQ = false
	}
}

// Video range engine (C000:5717): the run time writes a range of video
// RAM in 4-byte units to 0x2013B/3A (end, high/low) and 0x20139/38
// (start, high/low; writing 0x20138 starts it), sets SS:563 bit 1 and
// waits until the video interrupt (vector 0Ch, C000:5702) acknowledges
// with 0x20136 = FF and clears that bit. Before this was modelled the
// run time hung at C000:9D1A after leaving Setup. What the engine does
// to the range is not known (guess: it reformats the cells of the
// 4-byte-per-cell buffer at [1317] for display); only its completion is
// modelled, at the next vertical sync.
const (
	regEngineAck   = 0x36
	regEngineStart = 0x38
)

// Tick advances the raster position by n CPU cycles. Entering vertical
// sync raises the frame timer interrupt (see IOGA.FrameTimer).
func (v *Video) Tick(n int) {
	inSync := v.pos >= v.frameCycles-v.vsyncCycles
	v.pos += n
	for v.pos >= v.frameCycles {
		v.pos -= v.frameCycles
	}
	if !inSync && v.pos >= v.frameCycles-v.vsyncCycles {
		v.b.IO.FrameTimer()
		if v.engineBusy {
			v.engineBusy = false
			v.b.IO.videoIRQ = true
		}
	}
}

// Screen geometry, decoded from the CRTC as programmed by the mode-set
// routine F000:15AB (records at F000:16BA-1830, pointer table F000:19D8):
//
//	0x20130 (word)  screen start, offset in video segment 0800 (4200 ->
//	                0xC200)
//	0x20134 (word)  status-row start (6E00 -> 0xEE00)
//	0x20112         character cell height - 1 (13 / 10 / 0B -> 20/17/12)
//	0x20115         displayed rows (18 / 1B / 2B -> 24 / 27 / 43)
//
// Each cell is 3 bytes (EBCDIC character, attribute, extra) and rows are
// stored back to back: 240 bytes apart in 80-column modes, 396 in
// 132-column modes. The register selecting 80 or 132 displayed columns
// is not identified yet, so 80 columns are assumed (PROVISIONAL).
const (
	videoSeg    = 0x8000
	cellBytes   = 3
	regStart    = 0x30
	regStatus   = 0x34
	regCellH    = 0x12
	regRows     = 0x15
	defaultRows = 24
	defaultCols = 80
)

// Font RAM: two fonts of 256 glyphs, up to 16 pixels wide. Font 1: the
// left half of glyph c, row r is at 0x18000 + c*32 + r and the right half
// 0x2000 higher (bit 7 = leftmost pixel). Font 2 (attribute bit 7) is the
// same at 0x1C000 (e.g. glyph 0C there is a horizontal rule).
const (
	fontBase = 0x18000
	fontSize = 0x8000 // font 1 (left, right) + font 2 (left, right)
)

// Cell width in dots: the horizontal totals of the mode records (104
// characters in 80-column modes, 174 in 132-column modes) give 15-dot
// cells at 80 columns and 9-dot cells at 132 (font 2's rule glyph is
// exactly 15 dots wide).
const (
	cellWidth80  = 15
	cellWidth132 = 9
)

// Cursor registers: 0x20117 column, 0x20118 row, 0x20127 bit 6 = cursor
// on (C000:2360-2381).
const (
	regCursorCol = 0x17
	regCursorRow = 0x18
	regCursorCtl = 0x27
)

// Cursor returns the cursor position and whether it is shown.
func (v *Video) Cursor() (row, col int, on bool) {
	return int(v.regs[regCursorRow]), int(v.regs[regCursorCol]), v.regs[regCursorCtl]&0x40 != 0
}

// CellWidth returns the character cell width in dots for the current
// column count.
func (v *Video) CellWidth(cols int) int {
	if cols > 80 {
		return cellWidth132
	}
	return cellWidth80
}

// Screen returns the current geometry (rows include the status row at
// the bottom), the character cell height in scan lines, and a copy of the
// cells (3 bytes each).
func (v *Video) Screen() (rows, cols, cellHeight int, cells []byte) {
	mem := v.b.Mem()
	word := func(r int) uint32 { return uint32(v.regs[r]) | uint32(v.regs[r+1])<<8 }
	start := videoSeg + word(regStart)
	status := videoSeg + word(regStatus)
	n := int(v.regs[regRows])
	if n == 0 || n > 60 {
		n = defaultRows // CRTC not programmed yet
	}
	cellHeight = int(v.regs[regCellH]) + 1
	if cellHeight < 8 || cellHeight > 32 {
		cellHeight = 20
	}
	cols = defaultCols
	rows = n + 1
	cells = make([]byte, rows*cols*cellBytes)
	row := func(dst int, src uint32) {
		if src+uint32(cols*cellBytes) <= uint32(len(mem)) {
			copy(cells[dst*cols*cellBytes:(dst+1)*cols*cellBytes], mem[src:])
		}
	}
	stride := uint32(cols * cellBytes)
	for r := 0; r < n; r++ {
		row(r, start+uint32(r)*stride)
	}
	row(n, status)
	return rows, cols, cellHeight, cells
}

// Font returns a copy of the character-generator RAM (both fonts).
func (v *Video) Font() []byte {
	f := make([]byte, fontSize)
	copy(f, v.b.Mem()[fontBase:fontBase+fontSize])
	return f
}

// Reg16 reads a 16-bit CRTC register pair without side effects (tools).
func (v *Video) Reg16(off uint32) uint16 {
	return uint16(v.regs[off]) | uint16(v.regs[off+1])<<8
}
