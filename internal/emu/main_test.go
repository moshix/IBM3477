package emu

import (
	"os"
	"testing"
)

// TestMain points the terminal's nvram.bin at a temporary directory, so
// tests never read or write the user's ~/.config.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ibm3477-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
