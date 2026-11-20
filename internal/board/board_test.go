package board

import (
	"errors"
	"math/bits"
	"os"
	"strings"
	"testing"

	"ibm3486/internal/roms"
	"ibm3486/internal/tn5250"
)

// The ROM images live in the repository root (they are not committed).
const romDir = "../.."

func needROM(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Stat(romDir + "/" + name); err != nil {
		t.Skipf("ROM %s not present: %v", name, err)
	}
}

func TestModel3477ROMVerifies(t *testing.T) {
	needROM(t, "ibm3477_part1.bin")
	if _, err := New(&Model3477, os.DirFS(romDir)); err != nil {
		t.Fatal(err)
	}
}

func TestModel3486PartialDumpRefused(t *testing.T) {
	needROM(t, "3486.bin")
	_, err := New(&Model3486, os.DirFS(romDir))
	if !errors.Is(err, ErrPartial3486) {
		t.Fatalf("partial 3486 dump: got %v, want ErrPartial3486", err)
	}
}

// TestKeyboardPortReset drives the keyboard port the way the firmware
// does (F000:0CC3 send, F000:0BF2 receive) and checks the reset reply.
func TestKeyboardPortReset(t *testing.T) {
	var p kbdPort     // no power-on bytes queued: only the reset reply
	p.writeControl(3) // inhibit
	p.writeData(0xFF) // reset command
	p.writeControl(2) // release: transmission starts
	run := func() {
		for i := 0; i < 4*byteCycles && !p.irq; i += 100 {
			p.tick(100)
		}
	}
	run()
	if !p.irq {
		t.Fatal("no interrupt after sending the reset command")
	}
	p.ack()
	for _, want := range []uint8{0xFA, 0xAA, 0xBF, 0xBF} {
		run()
		if !p.irq || p.rx != want {
			t.Fatalf("got irq=%v rx=%02X, want %02X", p.irq, p.rx, want)
		}
		st := p.status()
		// odd parity over data + parity bit (checked at F000:0C72)
		if (bits.OnesCount8(p.rx)+int(st&1))&1 != 1 {
			t.Fatalf("parity bit wrong for %02X (status %02X)", p.rx, st)
		}
		if st&0x04 == 0 {
			t.Fatalf("receive-complete bit not set (status %02X)", st)
		}
		p.ack()
		if p.irq || p.status()&0x06 != 0 {
			t.Fatal("acknowledge did not clear the pending byte")
		}
	}
}

// TestBoot3477PassesPOST checks that all nine POST tests pass (RAM, ROM,
// twinax wrap, video, keyboard wrap and keyboard ID): POST clears the
// LED/error register 0x20010 and leaves the test table behind. The test
// number lives at 00AA:0017 (F000:0360).
func TestBoot3477PassesPOST(t *testing.T) {
	needROM(t, "ibm3477_part1.bin")
	b, err := New(&Model3477, os.DirFS(romDir))
	if err != nil {
		t.Fatal(err)
	}
	b.Run(400_000_000)
	if n := b.Mem()[0xAA0+0x17]; n < 9 {
		t.Fatalf("POST stopped at test %d, want all 9", n)
	}
	if b.CPU.Halted && b.CPU.Flags&0x0200 == 0 {
		t.Fatalf("CPU halted with interrupts off at %04X:%04X (POST failure)",
			b.CPU.S[1], b.CPU.IP)
	}
}

func TestROMSource(t *testing.T) {
	needROM(t, "ibm3477_part1.bin")
	if _, where, err := ROMSource(romDir, "3477"); err != nil || where != romDir {
		t.Fatalf("explicit dir: %q %v", where, err)
	}
	if _, _, err := ROMSource("/nonexistent", "3477"); err == nil {
		t.Fatal("missing ROM directory accepted")
	}
	if _, _, err := ROMSource("", "9999"); err == nil {
		t.Fatal("unknown model accepted")
	}
}

// TestEmbeddedROMs: when the build embedded the 3477 ROMs, they verify.
func TestEmbeddedROMs(t *testing.T) {
	emb := roms.FS()
	if !hasROMs(emb, &Model3477) {
		t.Skip("ROMs not embedded in this build (run build.bash)")
	}
	if _, err := New(&Model3477, emb); err != nil {
		t.Fatal(err)
	}
	if _, where, err := ROMSource("", "3477"); err != nil || where != "built-in" {
		t.Fatalf("default source: %q %v", where, err)
	}
}

// TestTwinaxPollAndCommand runs messages through a channel with the
// firmware's runtime microprograms layout: a poll gets a response built
// from the control block, and an ordinary command is stored at the
// receive pointer with "command received" (status bit1) set.
func TestTwinaxPollAndCommand(t *testing.T) {
	b, err := New(&Model3477, os.DirFS(romDir))
	if err != nil {
		t.Skip(err)
	}
	mem := b.Mem()
	// class table: commands 0x10 (Poll) -> class 2, others class 0
	mem[0x60+0x10] = 0x80
	// prog0 (ordinary command) for channel 0 at 0x80:
	//   acc=+6 -> h0; acc=+7 -> h1; acc=+5 -> h2; store cmd; data; end
	copy(mem[0x80:], []uint8{0x56, 0x20, 0x57, 0x21, 0x55, 0x22, 0x82, 0xA2, 0x84})
	// prog2 (poll) at 0xC0: acc=+C -> h6; acc=+D -> h7; respond; end
	copy(mem[0xC0:], []uint8{0x5C, 0x26, 0x5D, 0x27, 0xC0, 0x84})
	blk := mem[0x180:0x190]
	blk[5], blk[6], blk[7] = 0x10, 0x10, 0x1F // 16 bytes at 0x1F10
	blk[0xC], blk[0xD] = 0x0E, 0x00           // exception 7: power on
	b.IO.regs[regStationAddr] = 0x00
	b.IO.regs[regTwinaxCtl] = 0x82 // run + channel 0 enabled

	r, ok := b.Twinax.Deliver(0, 0x10, nil)
	if !ok || !r.Polled || r.Status&0x0E != 0x0E {
		t.Fatalf("poll: ok=%v %+v", ok, r)
	}
	if b.IO.regs[regTwinaxStatus]&0x01 == 0 {
		t.Fatal("poll did not set status bit0")
	}
	if _, ok := b.Twinax.Deliver(0, 0x15, []uint8{0x01, 0x23}); !ok {
		t.Fatal("command not accepted")
	}
	if mem[0x1F10] != 0x15 || mem[0x1F11] != 0x01 || mem[0x1F12] != 0x23 {
		t.Fatalf("receive buffer % X", mem[0x1F10:0x1F13])
	}
	if b.IO.regs[regTwinaxStatus]&0x02 == 0 {
		t.Fatal("command did not set status bit1")
	}
	if _, ok := b.Twinax.Deliver(3, 0x10, nil); ok {
		t.Fatal("message for another station address accepted")
	}
}

// TestKeyboardPortWrap: the POST loopback test (F000:3116) shifts a byte
// out with 11 control pulses and expects it back with parity and an IRQ.
func TestKeyboardPortWrap(t *testing.T) {
	var p kbdPort
	p.writeData(0x5A)
	for i := 0; i < wrapFrameClocks; i++ {
		p.writeControl(1)
		p.writeControl(3)
	}
	if !p.irq || p.rx != 0x5A {
		t.Fatalf("irq=%v rx=%02X, want the byte 5A back", p.irq, p.rx)
	}
	if (bits.OnesCount8(p.rx)+int(p.status()&1))&1 != 1 {
		t.Fatal("parity bit wrong")
	}
	if p.txQueued {
		t.Fatal("looped-back byte must not also go to the keyboard")
	}
}

// TestKeyboardKeyModes: after the firmware's set-up (F9, FB arrows, FC
// shift), letters send no release code and do not repeat, arrows repeat,
// and Shift sends its release.
func TestKeyboardKeyModes(t *testing.T) {
	var k Keyboard
	for _, c := range []uint8{0xFF, 0xF9, 0xFB, 0x61, 0x6A, 0x60, 0x63, 0xF3, 0x2C,
		0xFC, 0x59, 0x12, 0x14, 0x39, 0x19, 0xF4} {
		k.Command(c)
	}
	k.out, k.resp = k.out[:0], k.resp[:0]
	k.Key(0x1C)       // a down
	k.Key(0x1C)       // auto-repeat: dropped (make-only key)
	k.Key(0xF0, 0x1C) // a up: no release code
	k.Key(0x61)       // left arrow
	k.Key(0x61)       // repeat: passed (typematic)
	k.Key(0xF0, 0x61) // no release code
	k.Key(0x12)       // shift down
	k.Key(0xF0, 0x12) // shift up: release code
	want := []uint8{0x1C, 0x61, 0x61, 0x12, 0xF0, 0x12}
	if string(k.out) != string(want) {
		t.Fatalf("sent % X, want % X", k.out, want)
	}
}

// TestSetupMenuKey boots to the Offline Setup menu, presses Down arrow
// (make and release) and expects the highlight on the second item
// (screen row 7) and no "Invalid key is pressed." message - the release
// code of a make-only key must not reach the firmware.
func TestSetupMenuKey(t *testing.T) {
	needROM(t, "ibm3477_part1.bin")
	b, err := New(&Model3477, os.DirFS(romDir))
	if err != nil {
		t.Fatal(err)
	}
	// erased setup memory: the firmware resets it and opens Offline Setup
	b.LoadNVRAM(make([]uint8, 0x900))
	b.Run(200_000_000)
	b.IO.Keyboard().Key(0x60)
	b.IO.Keyboard().Key(0xF0, 0x60)
	b.Run(100_000_000)
	rows, cols, _, cells := b.Video.Screen()
	text := func(r int) string {
		var s []rune
		for c := 0; c < cols; c++ {
			s = append(s, tn5250.EBCDICToRune("37", cells[(r*cols+c)*3]))
		}
		return string(s)
	}
	if !strings.Contains(text(7), "Update serial number") {
		t.Fatalf("row 7 = %q, not the setup menu", text(7))
	}
	if attr := cells[(7*cols+2)*3+1]; attr&0x21 != 0x21 {
		t.Errorf("second item not highlighted (attr %02X)", attr)
	}
	for r := 0; r < rows; r++ {
		if strings.Contains(text(r), "Invalid key") {
			t.Fatalf("row %d: %q", r, text(r))
		}
	}
}
