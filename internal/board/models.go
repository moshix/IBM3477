package board

import (
	"errors"
	"fmt"
)

// wordSum is the firmware's ROM check: the 16-bit little-endian word sum
// of a memory range.
func wordSum(mem *[1 << 20]uint8, start, end uint32) uint16 {
	var s uint16
	for a := start; a < end; a += 2 {
		s += uint16(mem[a]) | uint16(mem[a+1])<<8
	}
	return s
}

// Model3477 is the IBM 3477 InfoWindow: two 128K ROMs at C0000-FFFFF
// (part2 below part1) and 128K of RAM at 0-1FFFF (POST test 1, table
// F000:1C24, checks 0000:0000, 0000:0B00, 0000:8000 and 1000:0000).
var Model3477 = Model{
	Name: "3477",
	ROMs: []ROMFile{
		{Name: "ibm3477_part2.bin", Base: 0xC0000, Size: 0x20000},
		{Name: "ibm3477_part1.bin", Base: 0xE0000, Size: 0x20000},
	},
	RAM:    []Range{{0x00000, 0x20000}},
	Straps: 0x04,
	// Setup configuration, POST error log and power-on counter
	NVRAM:        Range{0x001A0, 0x00AA0},
	DefaultNVRAM: Default3477Setup,
	// Each 64K segment of the 3477 ROM is padded to a fixed word sum
	Verify: func(mem *[1 << 20]uint8) error {
		want := []uint16{0x1234, 0x5678, 0x9ABC, 0xDEF0}
		for i, w := range want {
			base := 0xC0000 + uint32(i)*0x10000
			if got := wordSum(mem, base, base+0x10000); got != w {
				return fmt.Errorf("segment %04X word sum %04X, want %04X",
					base>>4, got, w)
			}
		}
		return nil
	},
}

// Default3477Setup is the start of the 3477's non-volatile RAM (physical
// 0x1A0-0x1FF) as the firmware's own Setup writes it (captured by driving
// the Setup screens in the emulator, see the NVRAM notes). The rest of
// the range starts out zero. Choices are stored as index+1 (FF = none):
//
//	1A0/1A1 Display Address 1 = 0, check byte (pair sum must be EA)
//	1A2/1A3 Display Address 2 = 0, check byte
//	1A4/1A5 Printer Address   = 0, check byte
//	1A6     Terminal Mode     = One-Display
//	1A7/1A8 Display Address 1/2 = 0 (index form)
//	1A9     Character Set     = Country Specific
//	1AA     Printer Address   = 0
//	1AB     Printer character set = PC
//	1AD     Printer Emulation = 5256
//	1B6-1BE serial number in index form (0-9A-Z -> 1..36)
//	1A6-1DD all other choices: IBM defaults
//	1DE     check byte: sum(1A6-1DD) + 1DE = EA (POST test 7, F000:14B2)
//	1F6-1FE machine serial number "78-1234567" in EBCDIC
//	1FF     check byte: sum(1F6-1FE) + 1FF = EA (F000:147E)
var Default3477Setup = []uint8{
	0x00, 0xEA, 0x00, 0xEA, 0x00, 0xEA, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0xFF, 0x01, 0x01, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x08, 0x09, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x04,
	0x01, 0x04, 0x01, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01, 0x02, 0x01, 0xFF, 0x02, 0x03, 0x04, 0xFF,
	0x01, 0x01, 0x01, 0x01, 0x02, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x20, 0x6C, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF7, 0xF8, 0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0x4F,
}

// Model3486 is the IBM 3486 InfoWindow II: one 512K ROM at 80000-FFFFF.
// POST (table F000:2B97) requires the word sum of 80000-FFFFF to be
// 0x619A. The available dump fails this (A17 was stuck high while it
// was read), so this model refuses to start until a correct dump exists.
var Model3486 = Model{
	Name: "3486",
	ROMs: []ROMFile{{Name: "3486.bin", Base: 0x80000, Size: 0x80000}},
	RAM: []Range{
		{0x00000, 0x20000},
		{0x38000, 0x50000},
	},
	Verify: func(mem *[1 << 20]uint8) error {
		if got := wordSum(mem, 0x80000, 0x100000); got != 0x619A {
			return fmt.Errorf("word sum of 80000-FFFFF is %04X, want 619A: "+
				"%w", got, ErrPartial3486)
		}
		return nil
	},
}

// ErrPartial3486 explains how to fix a 3486 dump that fails its check.
var ErrPartial3486 = errors.New("the 3486 ROM dump is incomplete; " +
	"re-read the chip so that chip A17 is driven (the known bad dump had " +
	"A17 stuck high and contains only segments A000/B000/E000/F000)")

// Models lists the supported models by name.
var Models = map[string]*Model{
	"3477": &Model3477,
	"3486": &Model3486,
}
