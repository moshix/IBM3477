// Package board models the IBM InfoWindow logic board around the 8088:
// memory map, ROM loading/verification, and the memory-mapped gate
// arrays. Everything the firmware touches that is not modelled yet is
// recorded, so the hardware can be reverse-engineered iteratively.
package board

import (
	"fmt"
	"io/fs"
	"sort"

	"ibm3486/internal/cpu"
)

// Range is a half-open physical address range [Start, End).
type Range struct{ Start, End uint32 }

// ROMFile is one ROM image and the physical address it is mapped at.
type ROMFile struct {
	Name string
	Base uint32
	Size int
}

// Model describes one terminal model's board.
type Model struct {
	Name string
	ROMs []ROMFile
	RAM  []Range
	// Straps is the value read from the I/O gate array's strap register
	// (0x2000A). Bits 1-3 select the display type (the 3477 POST accepts
	// 4, C or E there, F000:044D); bits 4-5 select special start modes
	// (0 = normal boot, F000:0793).
	Straps uint8
	// NVRAM is the battery-backed part of RAM that keeps the Setup
	// configuration across power cycles (nvram.go); DefaultNVRAM is the
	// image a new terminal starts with (nil = all zero, i.e. no valid
	// configuration).
	NVRAM        Range
	DefaultNVRAM []uint8
	// Verify checks the assembled ROM space (indexed by physical address)
	// with the firmware's own checksum rule.
	Verify func(mem *[1 << 20]uint8) error
}

// page kinds, at 4 KB granularity
const (
	pgUnmapped = iota
	pgRAM
	pgROM
	pgMMIO
)

// mmioBase is the 4 KB page holding both gate arrays (0x20000-0x201FF).
const mmioBase = 0x20000

// Access is one traced hardware access (unmapped or unmodelled MMIO).
type Access struct {
	Addr  uint32
	Write bool
	CS    uint16
	IP    uint16
}

// Board is a complete machine: CPU, memory and devices.
type Board struct {
	Model  *Model
	CPU    *cpu.CPU
	IO     *IOGA
	Video  *Video
	Twinax *Twinax
	// I2C is the monitor I2C bus on 0x2012F bits 0-1 (i2c.go)
	I2C *I2C

	mem  [1 << 20]uint8
	page [256]uint8

	// Accesses counts unmapped/unmodelled accesses by site
	Accesses map[Access]int
	// LastVal records the last value written per traced address
	LastVal map[uint32]uint8
}

// New builds a board for model m, loading its ROMs from roms (see
// ROMSource for where they usually come from).
func New(m *Model, roms fs.FS) (*Board, error) {
	b := &Board{
		Model:    m,
		Accesses: map[Access]int{},
		LastVal:  map[uint32]uint8{},
	}
	for _, r := range m.ROMs {
		data, err := fs.ReadFile(roms, r.Name)
		if err != nil {
			return nil, fmt.Errorf("%s ROM: %w", m.Name, err)
		}
		if len(data) != r.Size {
			return nil, fmt.Errorf("%s ROM %s: size %d, want %d",
				m.Name, r.Name, len(data), r.Size)
		}
		copy(b.mem[r.Base:], data)
		for a := r.Base; a < r.Base+uint32(r.Size); a += 0x1000 {
			b.page[a>>12] = pgROM
		}
	}
	if m.Verify != nil {
		if err := m.Verify(&b.mem); err != nil {
			return nil, fmt.Errorf("%s ROM check failed: %w", m.Name, err)
		}
	}
	for _, r := range m.RAM {
		for a := r.Start; a < r.End; a += 0x1000 {
			b.page[a>>12] = pgRAM
		}
	}
	b.page[mmioBase>>12] = pgMMIO
	if m.DefaultNVRAM != nil {
		// the default is the start of the range (the rest stays zero)
		if len(m.DefaultNVRAM) > int(m.NVRAM.End-m.NVRAM.Start) {
			return nil, fmt.Errorf("%s default setup larger than its NVRAM", m.Name)
		}
		copy(b.mem[m.NVRAM.Start:m.NVRAM.End], m.DefaultNVRAM)
	}
	b.IO = newIOGA(b)
	b.I2C = newI2C()
	b.Video = newVideo(b)
	b.Twinax = newTwinax(b)
	b.CPU = cpu.New(b)
	b.CPU.Intr = b.IO
	return b, nil
}

// trace records an access the board does not model.
func (b *Board) trace(addr uint32, write bool, v uint8) {
	k := Access{Addr: addr, Write: write}
	if b.CPU != nil {
		k.CS, k.IP = b.CPU.S[cpu.CS], b.CPU.IP
	}
	b.Accesses[k]++
	if write {
		b.LastVal[addr] = v
	}
}

// Read8 implements cpu.Bus.
func (b *Board) Read8(a uint32) uint8 {
	switch b.page[a>>12] {
	case pgRAM, pgROM:
		return b.mem[a]
	case pgMMIO:
		return b.mmioRead(a)
	}
	b.trace(a, false, 0)
	return 0xFF
}

// Write8 implements cpu.Bus.
func (b *Board) Write8(a uint32, v uint8) {
	switch b.page[a>>12] {
	case pgRAM:
		b.mem[a] = v
		return
	case pgMMIO:
		b.mmioWrite(a, v)
		return
	}
	b.trace(a, true, v) // ROM or unmapped
}

// In8/Out8: the firmware uses no I/O ports; record any use.
func (b *Board) In8(p uint16) uint8 {
	b.trace(0x100000|uint32(p), false, 0)
	return 0xFF
}

func (b *Board) Out8(p uint16, v uint8) { b.trace(0x100000|uint32(p), true, v) }

func (b *Board) mmioRead(a uint32) uint8 {
	off := a - mmioBase
	if off < 0x100 {
		return b.IO.Read(off)
	}
	if off < 0x200 {
		return b.Video.Read(off - 0x100)
	}
	b.trace(a, false, 0)
	return 0xFF
}

func (b *Board) mmioWrite(a uint32, v uint8) {
	off := a - mmioBase
	switch {
	case off < 0x100:
		b.IO.Write(off, v)
	case off < 0x200:
		b.Video.Write(off-0x100, v)
	default:
		b.trace(a, true, v)
	}
}

// Mem gives direct access to physical memory (for devices and tests).
func (b *Board) Mem() *[1 << 20]uint8 { return &b.mem }

// Run executes until at least `cycles` CPU cycles have elapsed, ticking
// the devices as time passes.
func (b *Board) Run(cycles uint64) {
	end := b.CPU.Cycles + cycles
	for b.CPU.Cycles < end {
		b.Step()
	}
}

// Step executes one instruction, ticks the devices, and returns the
// cycles used.
func (b *Board) Step() int {
	n := b.CPU.Step()
	b.Video.Tick(n)
	b.IO.Tick(n)
	return n
}

// TraceReport lists traced accesses, most frequent first.
func (b *Board) TraceReport(max int) []string {
	type kv struct {
		k Access
		n int
	}
	var all []kv
	for k, n := range b.Accesses {
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].k.Addr < all[j].k.Addr
	})
	var out []string
	for i, e := range all {
		if i >= max {
			break
		}
		rw := "R"
		if e.k.Write {
			rw = fmt.Sprintf("W(last %02X)", b.LastVal[e.k.Addr])
		}
		out = append(out, fmt.Sprintf("%6d  %05X %-12s at %04X:%04X",
			e.n, e.k.Addr, rw, e.k.CS, e.k.IP))
	}
	return out
}

// PatchROM overwrites one ROM byte. It exists only for the reverse-
// engineering probe (e.g. to step past a failing POST test); the
// emulator itself always runs the ROM unmodified.
func (b *Board) PatchROM(addr uint32, v uint8) { b.mem[addr&0xFFFFF] = v }
