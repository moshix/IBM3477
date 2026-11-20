// Command probe boots a terminal model headless for reverse engineering:
// it reports where the CPU spends its time, which hardware accesses are
// not modelled yet, and the POST status (LED/error register 0x20010).
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"sort"
	"strings"

	"ibm3486/internal/board"
	"ibm3486/internal/cpu"
	"ibm3486/internal/tn5250"
)

func main() {
	model := flag.String("model", "3477", "terminal model (3477 or 3486)")
	romDir := flag.String("romdir", ".", "directory holding the ROM files")
	cycles := flag.Uint64("cycles", 50_000_000, "CPU cycles to run")
	top := flag.Int("top", 25, "entries to show per report")
	dump := flag.String("dump", "", "hex range to dump at the end, e.g. 0-200")
	keys := flag.String("keys", "", "hex scan code bytes to type, e.g. 11,F0,11")
	keyAt := flag.Uint64("keyat", 100_000_000, "cycle at which -keys are typed")
	showScreen := flag.Bool("screen", false, "print the screen text at the end")
	pngOut := flag.String("png", "", "write the screen, drawn with the font RAM, to this PNG file")
	showIVT := flag.Bool("ivt", false, "log changes to interrupt vectors 00-1F")
	ncalls := flag.Int("calls", 0, "show the last N calls (call site -> target)")
	patch := flag.String("patch", "", "comma-separated ROM patches addr=byte "+
		"(physical hex), e.g. F03E5=90 to skip the POST failure HLT")
	seeds := flag.String("seeds", "", "append executed CS:IP addresses to this file "+
		"(feed to tools/disasm3486.py --seeds)")
	i2cLog := flag.Bool("i2c", false, "print the I2C bus transfer log (display type 6 EEPROM/monitor chip)")
	nvIn := flag.String("nvram", "", "load the non-volatile setup RAM (0x1A0-0xA9F) from this file")
	nvOut := flag.String("nvsave", "", "save the non-volatile setup RAM to this file at the end")
	flag.Parse()

	m, ok := board.Models[*model]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown model", *model)
		os.Exit(2)
	}
	b, err := board.New(m, os.DirFS(*romDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	patchROM(b, *patch)
	if *nvIn != "" {
		data, err := os.ReadFile(*nvIn)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := b.LoadNVRAM(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// Sample CS:IP after every instruction to find hot loops
	hot := map[uint32]int{}
	// Most recent distinct calls: a call is a CALL opcode (E8/9A/FF) after
	// which SP dropped by 2 (near) or 4 (far). Keyed by (site, target) and
	// ordered by the instruction count of their last occurrence.
	type call struct{ from, to uint32 }
	last := map[call]uint64{}
	var steps uint64
	var ivt [32]uint32
	var ivtLog []string
	end := b.CPU.Cycles + *cycles
	typed := false
	for b.CPU.Cycles < end {
		if !typed && *keys != "" && b.CPU.Cycles >= *keyAt {
			typed = true
			var codes []uint8
			for _, k := range strings.Split(*keys, ",") {
				var v uint32
				fmt.Sscanf(k, "%x", &v)
				codes = append(codes, uint8(v))
			}
			b.IO.Keyboard().Key(codes...)
		}
		c := b.CPU
		from := uint32(c.S[cpu.CS])<<16 | uint32(c.IP)
		op := b.Mem()[(uint32(c.S[cpu.CS])<<4+uint32(c.IP))&0xFFFFF]
		sp := c.R[cpu.SP]
		b.Step()
		steps++
		to := uint32(c.S[cpu.CS])<<16 | uint32(c.IP)
		hot[to]++
		// Interrupt vector table watch (vectors 0-31)
		for v := 0; v < 32; v++ {
			lo := uint16(b.Mem()[v*4]) | uint16(b.Mem()[v*4+1])<<8
			hi := uint16(b.Mem()[v*4+2]) | uint16(b.Mem()[v*4+3])<<8
			w := uint32(hi)<<16 | uint32(lo)
			if w != ivt[v] {
				ivt[v] = w
				ivtLog = append(ivtLog, fmt.Sprintf("vector %02X := %04X:%04X (by %04X:%04X)",
					v, hi, lo, from>>16, from&0xFFFF))
			}
		}
		if *ncalls > 0 && (op == 0xE8 || op == 0x9A || op == 0xFF) &&
			(sp-c.R[cpu.SP] == 2 || sp-c.R[cpu.SP] == 4) {
			last[call{from, to}] = steps
		}
	}
	if *ncalls > 0 {
		var cs []call
		for k := range last {
			cs = append(cs, k)
		}
		sort.Slice(cs, func(i, j int) bool { return last[cs[i]] < last[cs[j]] })
		if len(cs) > *ncalls {
			cs = cs[len(cs)-*ncalls:]
		}
		fmt.Println("most recent distinct calls (site -> target), oldest first:")
		for _, e := range cs {
			fmt.Printf("  %04X:%04X -> %04X:%04X\n",
				e.from>>16, e.from&0xFFFF, e.to>>16, e.to&0xFFFF)
		}
	}

	if *showIVT {
		fmt.Println("interrupt vector table writes:")
		for _, l := range ivtLog {
			fmt.Println(" ", l)
		}
	}
	c := b.CPU
	fmt.Printf("after %d cycles: CS:IP=%04X:%04X halted=%v flags=%04X\n",
		c.Cycles, c.S[cpu.CS], c.IP, c.Halted, c.Flags)
	fmt.Printf("AX=%04X BX=%04X CX=%04X DX=%04X SI=%04X DI=%04X BP=%04X SP=%04X\n",
		c.R[cpu.AX], c.R[cpu.BX], c.R[cpu.CX], c.R[cpu.DX],
		c.R[cpu.SI], c.R[cpu.DI], c.R[cpu.BP], c.R[cpu.SP])
	fmt.Printf("DS=%04X ES=%04X SS=%04X  LED/error reg 20010=%02X\n",
		c.S[cpu.DS], c.S[cpu.ES], c.S[cpu.SS], b.IO.Read(0x10))

	type kv struct {
		k uint32
		n int
	}
	var hs []kv
	for k, n := range hot {
		hs = append(hs, kv{k, n})
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].n > hs[j].n })
	fmt.Println("\nhottest instruction addresses:")
	for i, e := range hs {
		if i >= *top {
			break
		}
		fmt.Printf("%9d  %04X:%04X\n", e.n, e.k>>16, e.k&0xFFFF)
	}
	fmt.Println("\nunmodelled hardware accesses:")
	for _, l := range b.TraceReport(*top) {
		fmt.Println(l)
	}
	if l := b.IO.Keyboard().Log; len(l) > 0 {
		fmt.Printf("bytes sent to the keyboard: % X\n", l)
	}
	if *i2cLog {
		fmt.Println("I2C transfers:")
		for _, l := range b.I2C.Log {
			fmt.Println(" ", l)
		}
	}
	if *nvOut != "" {
		if err := os.WriteFile(*nvOut, b.SaveNVRAM(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if *showScreen {
		rows, cols, ch, cells := b.Video.Screen()
		fmt.Printf("screen %dx%d, cell height %d, start %04X, status row %04X\n",
			rows, cols, ch, b.Video.Reg16(0x30), b.Video.Reg16(0x34))
		for r := 0; r < rows; r++ {
			line := make([]rune, cols)
			for c := 0; c < cols; c++ {
				e := cells[(r*cols+c)*3]
				ru := tn5250.EBCDICToRune("37", e)
				if e == 0 || ru < 0x20 {
					ru = ' '
				}
				line[c] = ru
			}
			fmt.Printf("%2d|%s|\n", r, strings.TrimRight(string(line[:min(cols, 76)]), " "))
		}
	}
	if *pngOut != "" {
		if err := writePNG(b, *pngOut); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if *seeds != "" {
		// One "SSSS:OOOO" per line for every executed instruction start.
		// hot holds the address after each step, i.e. each next start.
		f, err := os.OpenFile(*seeds, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for k := range hot {
			fmt.Fprintf(f, "%04X:%04X\n", k>>16, k&0xFFFF)
		}
		f.Close()
	}
	if *dump != "" {
		var lo, hi uint32
		fmt.Sscanf(*dump, "%x-%x", &lo, &hi)
		mem := b.Mem()
		for a := lo; a < hi; a += 16 {
			fmt.Printf("%05X:", a)
			for i := uint32(0); i < 16; i++ {
				fmt.Printf(" %02X", mem[a+i])
			}
			fmt.Println()
		}
	}
}

// patchROM applies "addr=byte" patches and keeps every touched 64K
// segment's 16-bit word sum unchanged (the POST ROM test checks it) by
// adjusting a word inside a run of 0xCC filler in the same segment.
func patchROM(b *board.Board, spec string) {
	mem := b.Mem()
	sum := func(base uint32) uint16 {
		var s uint16
		for a := base; a < base+0x10000; a += 2 {
			s += uint16(mem[a]) | uint16(mem[a+1])<<8
		}
		return s
	}
	before := map[uint32]uint16{}
	for _, p := range strings.Split(spec, ",") {
		var a, v uint32
		if n, _ := fmt.Sscanf(p, "%x=%x", &a, &v); n != 2 {
			continue
		}
		seg := a &^ 0xFFFF
		if _, ok := before[seg]; !ok {
			before[seg] = sum(seg)
		}
		b.PatchROM(a, uint8(v))
	}
	for seg, want := range before {
		delta := want - sum(seg)
		if delta == 0 {
			continue
		}
		// find an aligned word inside 16+ bytes of 0xCC filler
		for a := seg; a < seg+0xFFE0; a += 2 {
			fill := true
			for i := uint32(0); i < 16; i++ {
				if mem[a+i] != 0xCC {
					fill = false
					break
				}
			}
			if fill {
				w := uint16(mem[a+8]) | uint16(mem[a+9])<<8
				w += delta
				b.PatchROM(a+8, uint8(w))
				b.PatchROM(a+9, uint8(w>>8))
				break
			}
		}
	}
}

// writePNG draws the screen the way the browser does: glyphs from font RAM
// (font 2 for attribute bit 7), the CRTC cell size, normal/high green,
// reverse (attribute bits 0+5), underline (bit 2) and the cursor.
func writePNG(b *board.Board, path string) error {
	rows, cols, ch, cells := b.Video.Screen()
	cw := b.Video.CellWidth(cols)
	font := b.Video.Font()
	crow, ccol, con := b.Video.Cursor()
	img := image.NewRGBA(image.Rect(0, 0, cols*cw, rows*ch))
	normal := color.RGBA{0x2A, 0xC8, 0x55, 0xFF}
	high := color.RGBA{0x77, 0xFF, 0x99, 0xFF}
	draw.Draw(img, img.Bounds(), &image.Uniform{color.Black}, image.Point{}, draw.Src)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := (r*cols + c) * 3
			code, attr := int(cells[i]), cells[i+1]
			base := 0
			if attr&0x80 != 0 {
				base = 0x4000
			}
			// same rules as the browser (terminal.js attrStyle): the 5250
			// attribute table on the low 5 bits, no column separators,
			// status row plain with a fixed line along its top
			statusRow := r == rows-1
			st := monoAttr[attr&0x1F]
			if cells[i+2]&0x80 != 0 { // non-display field (password)
				st = "n"
			}
			if statusRow {
				st = ""
			}
			if st == "n" {
				continue
			}
			fg := normal
			if strings.Contains(st, "h") {
				fg = high
			}
			reverse := strings.Contains(st, "r")
			for y := 0; y < ch && y < 32; y++ {
				bits := uint16(font[base+code*32+y])<<8 | uint16(font[base+0x2000+code*32+y])
				if code == 0x40 || code == 0 {
					bits = 0
				}
				if strings.Contains(st, "u") && y == ch-2 {
					bits = 0xFFFF
				}
				if statusRow && y == 0 { // fixed status-row separator line
					bits = 0xFFFF
				}
				if con && r == crow && c == ccol && y >= ch-3 && y < ch-1 {
					bits = 0xFFFF
				}
				for x := 0; x < cw; x++ {
					on := bits&(0x8000>>x) != 0
					if on != reverse {
						img.Set(c*cw+x, r*ch+y, fg)
					}
				}
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// monoAttr is the 5250 attribute table used by the browser renderer
// (terminal.js MONO_ATTR): h high, r reverse, u underline, b blink,
// n non-display; column separators are not drawn.
var monoAttr = [32]string{
	"", "r", "h", "hr", "u", "ur", "hu", "n",
	"", "r", "b", "br", "u", "ur", "ub", "n",
	"", "r", "h", "hr", "u", "ur", "hu", "n",
	"", "r", "", "r", "u", "ur", "u", "n",
}
