package emu

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// The terminal's non-volatile setup memory (battery-backed RAM on the
// real board) is kept in ~/.config/ibm<model>/nvram.bin, or under
// $XDG_CONFIG_HOME when that is set. The file is created with the
// built-in default configuration the first time a terminal starts, is
// loaded at every power-on, and is rewritten whenever the firmware
// changes the memory (e.g. after the user saves in Setup).

// NVRAMPath returns the nvram.bin path for a model.
func NVRAMPath(model string) (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ibm"+model, "nvram.bin"), nil
}

// nvMu serialises every read and write of the nvram.bin files: all
// terminals of a model in this process share one file (on the public
// server one visitor's Setup is everyone's), so concurrent power-ons and
// saves must neither see a half-written file nor trip over each other's
// temporary files.
var nvMu sync.Mutex

// Power-on counter: POST (F000:01D1-01EB) adds 1 to the 32-bit word at
// 0xA90/0xA92 at every power-on (it stops at 00FF:FFFF). Nothing in the
// firmware reads it back for display or decisions that we know of; it
// only lives in the non-volatile range. A change of the counter alone is
// therefore not saved: otherwise every power-on of every terminal would
// rewrite the shared file. It is saved along with any real change.
const (
	nvCounterAddr = 0xA90
	nvCounterLen  = 4
)

// loadNVRAM loads the saved setup memory into the board, or creates the
// file from the built-in default (already in the board) when there is
// none yet. A file of the wrong size (damaged, or from another build) is
// replaced by the default; the old file is kept as nvram.bin.bad. It
// returns the path to keep saving to ("" = not persisted).
func (m *Machine) loadNVRAM(model string) string {
	if m.b.NVRAMRange().End == 0 {
		return "" // model without non-volatile memory
	}
	path, err := NVRAMPath(model)
	if err != nil {
		log.Printf("nvram: %v (setup will not be saved)", err)
		return ""
	}
	nvMu.Lock()
	defer nvMu.Unlock()
	data, err := os.ReadFile(path)
	if err == nil {
		if lerr := m.b.LoadNVRAM(data); lerr != nil {
			// keep the old file (an older build's or a damaged one) as
			// nvram.bin.bad instead of losing that setup for good
			bad := path + ".bad"
			if rerr := os.Rename(path, bad); rerr != nil {
				log.Printf("nvram: %s: %v; cannot keep it as %s: %v (setup will not be saved)",
					path, lerr, bad, rerr)
				m.lastNV = m.b.SaveNVRAM()
				return "" // never overwrite a file we could not set aside
			}
			log.Printf("nvram: %s: %v (kept as %s, using the default setup)", path, lerr, bad)
			err = os.ErrNotExist // write the default
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if err := writeNVRAM(path, m.b.SaveNVRAM()); err != nil {
			log.Printf("nvram: %v (setup will not be saved)", err)
			return ""
		}
	default:
		log.Printf("nvram: %v (using the default setup)", err)
	}
	m.lastNV = m.b.SaveNVRAM()
	return path
}

// saveNVRAM writes the setup memory if the firmware changed it since the
// last save (the power-on counter alone does not count). Called from the
// machine goroutine.
func (m *Machine) saveNVRAM() {
	if m.nvPath == "" {
		return
	}
	cur := m.b.SaveNVRAM()
	if !nvChanged(m.b.NVRAMRange().Start, m.lastNV, cur) {
		return
	}
	nvMu.Lock()
	err := writeNVRAM(m.nvPath, cur)
	nvMu.Unlock()
	if err != nil {
		log.Printf("nvram: %v", err)
		return
	}
	m.lastNV = cur
}

// nvChanged reports whether two images of the range starting at base
// differ in more than the power-on counter.
func nvChanged(base uint32, old, cur []byte) bool {
	if len(old) != len(cur) {
		return true
	}
	lo, hi := int(nvCounterAddr)-int(base), int(nvCounterAddr)-int(base)+nvCounterLen
	for i := range cur {
		if cur[i] != old[i] && (i < lo || i >= hi) {
			return true
		}
	}
	return false
}

// writeNVRAM replaces the file atomically: the data goes to a temporary
// file of its own in the same directory, is synced to disk, and is then
// renamed over the old file, so a reader or a crash never sees a
// half-written configuration. Callers hold nvMu.
func writeNVRAM(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "nvram-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
