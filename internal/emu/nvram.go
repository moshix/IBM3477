package emu

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// loadNVRAM loads the saved setup memory into the board, or creates the
// file from the built-in default (already in the board) when there is
// none yet. It returns the path to keep saving to ("" = not persisted).
func (m *Machine) loadNVRAM(model string) string {
	if m.b.NVRAMRange().End == 0 {
		return "" // model without non-volatile memory
	}
	path, err := NVRAMPath(model)
	if err != nil {
		log.Printf("nvram: %v (setup will not be saved)", err)
		return ""
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		m.b.LoadNVRAM(data)
	case os.IsNotExist(err):
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
// last save. Called from the machine goroutine.
func (m *Machine) saveNVRAM() {
	if m.nvPath == "" {
		return
	}
	cur := m.b.SaveNVRAM()
	if string(cur) == string(m.lastNV) {
		return
	}
	if err := writeNVRAM(m.nvPath, cur); err != nil {
		log.Printf("nvram: %v", err)
		return
	}
	m.lastNV = cur
}

// writeNVRAM replaces the file atomically (write a temporary file, then
// rename), so a crash never leaves a half-written configuration.
func writeNVRAM(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
