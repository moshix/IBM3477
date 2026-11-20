package emu

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNVRAMFileCreatedAndKept: the first start writes the default setup
// to $XDG_CONFIG_HOME/ibm3477/nvram.bin; a later start loads that file.
func TestNVRAMFileCreatedAndKept(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := NVRAMPath("3477")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nvram.bin not created: %v", err)
	}
	// the file holds 0x1A0-0xA9F: serial number byte 0x1F6 is at 0x56
	if len(first) != 0x900 || first[0x1F6-0x1A0] != 0xF7 {
		t.Fatalf("unexpected default image (%d bytes, serial byte %02X)", len(first), first[0x1F6-0x1A0])
	}
	if filepath.Base(filepath.Dir(path)) != "ibm3477" {
		t.Fatalf("path %s, want .../ibm3477/nvram.bin", path)
	}
	// a changed image on disk is what the next machine starts with
	changed := append([]byte(nil), first...)
	changed[0x10] = 0x5A
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	m2, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.b.SaveNVRAM(); !bytes.Equal(got, changed) {
		t.Fatal("saved nvram.bin was not loaded")
	}
	// running and stopping writes the firmware's changes back
	m.Start(func(*Snapshot) {})
	time.Sleep(1500 * time.Millisecond)
	m.Stop()
	after, _ := os.ReadFile(path)
	if len(after) != 0x900 {
		t.Fatalf("nvram.bin after run: %d bytes", len(after))
	}
}
