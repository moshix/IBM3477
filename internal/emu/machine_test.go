package emu

import (
	"os"
	"strings"
	"testing"
	"time"

	"ibm3486/internal/tn5250"
)

func TestScanCodes(t *testing.T) {
	if got := ScanCodes("KeyA", true); len(got) != 1 || got[0] != 0x1C {
		t.Fatalf("KeyA make = % X, want 1C", got)
	}
	if got := ScanCodes("KeyA", false); len(got) != 2 || got[0] != 0xF0 || got[1] != 0x1C {
		t.Fatalf("KeyA break = % X, want F0 1C", got)
	}
	if ScanCodes("MediaPlayPause", true) != nil {
		t.Fatal("unknown key should produce no scan codes")
	}
	if got := ScanCodes("fn:Reset", true); len(got) != 1 || got[0] != 0x11 {
		t.Fatalf("fn:Reset = % X, want 11", got)
	}
	// PF13 is the 122-key keyboard's F13 key (the terminal sends Shift +
	// F1 to the controller itself)
	if got := ScanCodes("fn:PF13", true); len(got) != 1 || got[0] != 0x08 {
		t.Fatalf("fn:PF13 = % X, want 08 (F13)", got)
	}
	if got := ScanCodes("fn:PF13", false); len(got) != 2 || got[0] != 0xF0 || got[1] != 0x08 {
		t.Fatalf("fn:PF13 release = % X, want F0 08", got)
	}
	if got := ScanCodes("fn:Setup", true); len(got) != 1 || got[0] != 0x01 {
		t.Fatalf("fn:Setup = % X, want 01", got)
	}
	if ScanCodes("fn:Hex", true) != nil {
		t.Fatal("fn:Hex is not on the layout yet")
	}
	if got := ScanCodes("fn:BackTab", true); len(got) != 2 || got[0] != 0x12 || got[1] != 0x0D {
		t.Fatalf("fn:BackTab = % X, want 12 0D", got)
	}
	if ScanCodes("ControlLeft", true) != nil {
		t.Fatal("Ctrl must not be passed through (it is Reset on the 122-key keyboard)")
	}
}

// TestMachinePublishesFont boots the 3477 in real time and expects a
// snapshot carrying the font the firmware builds in RAM during POST.
func TestMachinePublishesFont(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	snaps := make(chan *Snapshot, 64)
	m.Start(func(s *Snapshot) {
		select {
		case snaps <- s:
		default:
		}
	})
	defer m.Stop()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case s := <-snaps:
			// glyph 'A' (EBCDIC C1) has pixels once the font is built
			if s.Font != nil && s.Font[0xC1*32+8] != 0 {
				if s.Rows == 0 || len(s.Cells) != s.Rows*s.Cols*3 {
					t.Fatalf("bad geometry %dx%d, %d cell bytes", s.Rows, s.Cols, len(s.Cells))
				}
				return
			}
		case <-deadline:
			t.Fatal("no snapshot with a built font within 10 s")
		}
	}
}

func TestUnknownModel(t *testing.T) {
	if _, err := New("9999", os.DirFS("../..")); err == nil {
		t.Fatal("expected an error for an unknown model")
	}
}

// TestSetupKeyThroughMachine sends the Setup key the way the SETUP link
// does (press, release 150 ms later) to a running terminal and expects
// the firmware's Online Setup.
func TestSetupKeyThroughMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the terminal for about 30 s in real time")
	}
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	m, err := New("3477", os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	m.Start(func(*Snapshot) {})
	time.Sleep(25 * time.Second) // POST and logo, in real time
	m.Key(ScanCodes("fn:Setup", true))
	time.Sleep(150 * time.Millisecond)
	m.Key(ScanCodes("fn:Setup", false))
	time.Sleep(3 * time.Second)
	m.Stop() // the machine goroutine has exited: the board can be read
	_, cols, _, cells := m.b.Video.Screen()
	var row0 []rune
	for c := 0; c < cols; c++ {
		row0 = append(row0, rune(cells[c*3]))
	}
	got := ""
	for _, e := range row0 {
		got += string(tn5250.EBCDICToRune("37", byte(e)))
	}
	if !strings.Contains(got, "Online Setup") {
		t.Fatalf("row 0 = %q, want Online Setup", got)
	}
}
