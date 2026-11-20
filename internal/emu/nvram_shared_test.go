package emu

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestNVRAMShortFileRejected: a short (partly written) nvram.bin is not
// loaded: the terminal starts with the default setup and the file is
// replaced by a complete default image.
func TestNVRAMShortFileRejected(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := NVRAMPath("3477")
	m0, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	def := m0.b.SaveNVRAM()
	short := bytes.Repeat([]byte{0xA5}, 100)
	if err := os.WriteFile(path, short, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.b.SaveNVRAM(), def) {
		t.Fatal("short nvram.bin was loaded")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, def) {
		t.Fatalf("nvram.bin not replaced by the default (%d bytes)", len(got))
	}
	// the rejected file is kept, not lost
	if got, err := os.ReadFile(path + ".bad"); err != nil || !bytes.Equal(got, short) {
		t.Fatalf("rejected nvram.bin not kept as nvram.bin.bad: %v (%d bytes)", err, len(got))
	}
}

// TestNVRAMConcurrentMachines: many terminals powering on and saving at
// the same time (one shared file on the server) never fail, never read
// a partial file, and leave no temporary files behind.
func TestNVRAMConcurrentMachines(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := NVRAMPath("3477")
	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				m, err := New("3477", os.DirFS("../.."))
				if err != nil {
					t.Error(err)
					return
				}
				// a Setup change: every save is a real write
				m.b.Mem()[0x1A0+0x300] = byte(g*10 + i + 1)
				m.saveNVRAM()
			}
		}(g)
	}
	wg.Wait()
	if s := logBuf.String(); s != "" {
		t.Fatalf("nvram errors:\n%s", s)
	}
	if got, _ := os.ReadFile(path); len(got) != 0x900 {
		t.Fatalf("nvram.bin is %d bytes", len(got))
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if e.Name() != "nvram.bin" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

// TestNVRAMCounterOnlyNotSaved: once the setup has been through a
// power-on, a further power-on changes only the power-on counter at
// 0xA90; that alone does not rewrite the shared file, while a real change
// (with the counter) is saved.
func TestNVRAMCounterOnlyNotSaved(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := NVRAMPath("3477")
	first, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	// the first power-on from the built-in default completes the image
	// (POST fills in more than the counter): that is saved
	first.b.Run(600_000_000)
	first.saveNVRAM()
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	m.b.Run(600_000_000) // POST and the start of the run time
	after := m.b.SaveNVRAM()
	var diff []int
	for i := range after {
		if after[i] != before[i] {
			diff = append(diff, 0x1A0+i)
		}
	}
	for _, a := range diff {
		if a < 0xA90 || a > 0xA93 {
			t.Fatalf("power-on changed %03X (changes at % X), not only the counter", a, diff)
		}
	}
	if len(diff) == 0 {
		t.Fatal("power-on counter did not change")
	}
	m.saveNVRAM()
	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Fatal("a counter-only change rewrote nvram.bin")
	}
	m.b.Mem()[0x1A0+0x300] ^= 0xFF
	m.saveNVRAM()
	if got, _ := os.ReadFile(path); !bytes.Equal(got, m.b.SaveNVRAM()) {
		t.Fatal("a real change was not saved")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
