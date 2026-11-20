package cpu

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The SingleStepTests 8088 v2 suite (hardware-captured from an AMD D8088)
// lives in testdata/8088; fetch it with tools/fetch_cpu_tests.sh.
const suiteDir = "testdata/8088"

// testBus is a flat 1 MB RAM that remembers which bytes the CPU wrote,
// so that unexpected writes can be detected and cheaply undone.
type testBus struct {
	mem     [1 << 20]uint8
	written []uint32
}

func (b *testBus) Read8(a uint32) uint8 { return b.mem[a&0xFFFFF] }
func (b *testBus) Write8(a uint32, v uint8) {
	b.mem[a&0xFFFFF] = v
	b.written = append(b.written, a&0xFFFFF)
}

// The test rig returns 0xFF for every I/O read.
func (b *testBus) In8(uint16) uint8   { return 0xFF }
func (b *testBus) Out8(uint16, uint8) {}

type ssState struct {
	Regs map[string]int `json:"regs"`
	RAM  [][2]uint32    `json:"ram"`
}

type ssTest struct {
	Name    string  `json:"name"`
	Bytes   []int   `json:"bytes"`
	Initial ssState `json:"initial"`
	Final   ssState `json:"final"`
	Idx     int     `json:"idx"`
}

type opMeta struct {
	Status    string            `json:"status"`
	FlagsMask *int              `json:"flags-mask"`
	Reg       map[string]opMeta `json:"reg"`
}

type suiteMeta struct {
	Opcodes map[string]opMeta `json:"opcodes"`
}

var regNames = []string{"ax", "cx", "dx", "bx", "sp", "bp", "si", "di"}
var segNames = []string{"es", "cs", "ss", "ds"}

func loadMeta(t *testing.T) suiteMeta {
	t.Helper()
	f, err := os.Open(filepath.Join(suiteDir, "metadata.json"))
	if err != nil {
		msg := fmt.Sprintf("SingleStepTests 8088 suite NOT RUN: %s/metadata.json missing (%v).\n"+
			"Fetch it (~760 MB) with: sh tools/fetch_cpu_tests.sh\n"+
			"Set %s=1 to make its absence a failure.", suiteDir, err, requireSuiteEnv)
		if os.Getenv(requireSuiteEnv) != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	defer f.Close()
	var m suiteMeta
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// requireSuiteEnv names the environment variable that turns a missing
// suite from a skip into a failure.
const requireSuiteEnv = "CPU_SUITE_REQUIRED"

// lookup returns the metadata for a test file key such as "00" or "80.3".
// A few opcodes (8F, C6, C7) carry a per-reg table in the metadata but
// are captured as one whole-opcode file; for those the file runs with the
// status of /0 and the AND of the flag masks of all defined reg values.
func (m suiteMeta) lookup(key string) opMeta {
	op, reg, grouped := strings.Cut(key, ".")
	om := m.Opcodes[op]
	if grouped {
		return om.Reg[reg]
	}
	if om.Reg == nil {
		return om
	}
	whole := opMeta{Status: om.Reg["0"].Status}
	for _, rm := range om.Reg {
		if rm.Status == "undefined" || rm.FlagsMask == nil {
			continue
		}
		mask := *rm.FlagsMask
		if whole.FlagsMask != nil {
			mask &= *whole.FlagsMask
		}
		whole.FlagsMask = &mask
	}
	return whole
}

func loadTests(path string) ([]ssTest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var tests []ssTest
	err = json.NewDecoder(gz).Decode(&tests)
	return tests, err
}

// runOne executes a single test case and returns a description of the
// first mismatch, or "" when the CPU's final state matches the hardware.
func runOne(bus *testBus, tc *ssTest, flagsMask uint16) string {
	// Undo the previous test's memory writes
	for _, a := range bus.written {
		bus.mem[a] = 0
	}
	bus.written = bus.written[:0]
	init := map[uint32]uint8{}
	for _, e := range tc.Initial.RAM {
		bus.mem[e[0]] = uint8(e[1])
		init[e[0]] = uint8(e[1])
		bus.written = append(bus.written, e[0]) // so it is cleared next time
	}
	c := &CPU{Bus: bus}
	for i, n := range regNames {
		c.R[i] = uint16(tc.Initial.Regs[n])
	}
	for i, n := range segNames {
		c.S[i] = uint16(tc.Initial.Regs[n])
	}
	c.IP = uint16(tc.Initial.Regs["ip"])
	c.Flags = uint16(tc.Initial.Regs["flags"])
	firstWrite := len(bus.written)

	c.execute()

	want := func(n string) uint16 {
		if v, ok := tc.Final.Regs[n]; ok {
			return uint16(v)
		}
		return uint16(tc.Initial.Regs[n])
	}
	var errs []string
	for i, n := range regNames {
		if c.R[i] != want(n) {
			errs = append(errs, fmt.Sprintf("%s=%04X want %04X", n, c.R[i], want(n)))
		}
	}
	for i, n := range segNames {
		if c.S[i] != want(n) {
			errs = append(errs, fmt.Sprintf("%s=%04X want %04X", n, c.S[i], want(n)))
		}
	}
	if c.IP != want("ip") {
		errs = append(errs, fmt.Sprintf("ip=%04X want %04X", c.IP, want("ip")))
	}
	if c.Flags&flagsMask != want("flags")&flagsMask {
		errs = append(errs, fmt.Sprintf("flags=%04X want %04X (mask %04X)",
			c.Flags, want("flags"), flagsMask))
	}
	// Memory: every listed final byte must match, and every byte the CPU
	// wrote that is not listed must still hold its initial value.
	// Divide-error traps (DIV/IDIV/AAM): the FLAGS word pushed by INT 0
	// holds intermediate values from the 8088's division microcode. They
	// are architecturally undefined and not modelled, so those two stack
	// bytes are exempt; the pushed CS:IP and all registers are checked.
	exempt := map[uint32]bool{}
	if isDivOp(tc.Bytes) && uint16(c.IP) == uint16(int(bus.mem[0])|int(bus.mem[1])<<8) &&
		c.S[CS] == uint16(int(bus.mem[2])|int(bus.mem[3])<<8) {
		fl := phys(c.S[SS], c.R[SP]+4)
		exempt[fl], exempt[(fl+1)&0xFFFFF] = true, true
	}
	final := map[uint32]uint8{}
	for _, e := range tc.Final.RAM {
		final[e[0]] = uint8(e[1])
		if exempt[e[0]] {
			continue
		}
		if bus.mem[e[0]] != uint8(e[1]) {
			errs = append(errs, fmt.Sprintf("mem[%05X]=%02X want %02X",
				e[0], bus.mem[e[0]], e[1]))
		}
	}
	for _, a := range bus.written[firstWrite:] {
		if _, ok := final[a]; ok {
			continue
		}
		if bus.mem[a] != init[a] {
			errs = append(errs, fmt.Sprintf("unexpected write mem[%05X]=%02X", a, bus.mem[a]))
		}
	}
	return strings.Join(errs, ", ")
}

// isDivOp reports whether an instruction (after prefixes) is DIV/IDIV
// (F6/F7 /6 /7) or AAM (D4), the instructions that can raise INT 0.
func isDivOp(b []int) bool {
	i := 0
	for i < len(b) && (b[i] == 0x26 || b[i] == 0x2E || b[i] == 0x36 || b[i] == 0x3E ||
		b[i] == 0xF0 || b[i] == 0xF1 || b[i] == 0xF2 || b[i] == 0xF3) {
		i++
	}
	if i >= len(b) {
		return false
	}
	if b[i] == 0xD4 {
		return true
	}
	return (b[i] == 0xF6 || b[i] == 0xF7) && i+1 < len(b) && b[i+1]>>3&7 >= 6
}

// TestSingleStep8088 runs the hardware-captured suite. With -short only
// the first 300 cases of each opcode file run.
func TestSingleStep8088(t *testing.T) {
	meta := loadMeta(t)
	files, _ := filepath.Glob(filepath.Join(suiteDir, "*.json.gz"))
	sort.Strings(files)
	for _, path := range files {
		key := strings.TrimSuffix(filepath.Base(path), ".json.gz")
		om := meta.lookup(key)
		switch om.Status {
		case "undefined", "fpu", "prefix":
			continue // not meaningful for this firmware
		case "":
			t.Errorf("%s: no metadata for this test file; it would be skipped", key)
			continue
		}
		mask := uint16(0xFFFF)
		if om.FlagsMask != nil {
			mask = uint16(*om.FlagsMask)
		}
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			tests, err := loadTests(path)
			if err != nil {
				t.Fatal(err)
			}
			if testing.Short() && len(tests) > 300 {
				tests = tests[:300]
			}
			bus := &testBus{}
			fails := 0
			for i := range tests {
				if msg := runOne(bus, &tests[i], mask); msg != "" {
					if fails < 3 {
						t.Errorf("#%d %q bytes=% X: %s", tests[i].Idx,
							tests[i].Name, tests[i].Bytes, msg)
					}
					fails++
				}
			}
			if fails > 0 {
				t.Errorf("%s: %d/%d failed", key, fails, len(tests))
			}
			t.Logf("%s: %d cases", key, len(tests))
		})
	}
}
