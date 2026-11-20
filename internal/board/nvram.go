package board

import "fmt"

// Non-volatile setup memory.
//
// The 3477 keeps its Setup configuration in ordinary RAM that survives
// power-off (battery-backed): physical 0x001A0-0x00A9F. The firmware
// treats exactly this range as persistent:
//
//   - POST test 1 (RAM test, table F000:1C24) tests 0000-019F and
//     0B00-1FFFF but skips 01A0-0AFF; 0AA0-0AFF (POST state) is tested
//     at reset before anything is stored there.
//   - POST test 6 (F000:2DFE) tests 01A0-0A9F word by word without
//     destroying it: it saves each word and its address at 0A22/0A20,
//     and on the next power-on restores that word if the previous test
//     was interrupted (marker ABCD in 0A20 when done).
//   - POST test 7 (F000:2EFE) checks the stored configuration and
//     records failures in 0AA0 (see the NVRAM notes); the run time
//     enters Setup when the result is not clean (C000:08D4).
//
// The emulator therefore just preserves this RAM range: the host
// application saves SaveNVRAM() when it shuts down and passes it back to
// LoadNVRAM() before the next power-on.

// NVRAMRange returns the model's non-volatile RAM range (empty if none).
func (b *Board) NVRAMRange() Range { return b.Model.NVRAM }

// LoadNVRAM copies a saved image into the non-volatile RAM range. An
// image of any other size than the range (a truncated or foreign file)
// is rejected and the memory left as it is.
func (b *Board) LoadNVRAM(p []uint8) error {
	r := b.Model.NVRAM
	if n := int(r.End - r.Start); len(p) != n {
		return fmt.Errorf("nvram image is %d bytes, want %d", len(p), n)
	}
	copy(b.mem[r.Start:r.End], p)
	return nil
}

// SaveNVRAM returns a copy of the non-volatile RAM range.
func (b *Board) SaveNVRAM() []uint8 {
	r := b.Model.NVRAM
	return append([]uint8(nil), b.mem[r.Start:r.End]...)
}
