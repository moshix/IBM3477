package board

import (
	"bytes"
	"os"
	"testing"
)

// TestKeyboardReplyOrder: a command's reply goes ahead of key codes
// still queued, but never between F0 and the key code of a break.
func TestKeyboardReplyOrder(t *testing.T) {
	var k Keyboard
	k.setDefaults()
	k.enabledForTest()
	k.out = []uint8{0x1C}
	k.Command(0xEE)
	if !bytes.Equal(k.queued(), []uint8{0xEE, 0x1C}) {
		t.Fatalf("echo reply: out % X, want EE 1C", k.queued())
	}
	k.resp = nil
	k.out, k.last = []uint8{0x1C}, 0xF0 // F0 already sent, 1C to follow
	k.Command(0xEE)
	if !bytes.Equal(k.queued(), []uint8{0x1C, 0xEE}) {
		t.Fatalf("reply inside a break: out % X, want 1C EE", k.queued())
	}
	// next() sends them in that order
	for _, want := range []uint8{0x1C, 0xEE} {
		if b, ok := k.next(); !ok || b != want {
			t.Fatalf("next = %02X %v, want %02X", b, ok, want)
		}
	}
}

// enabledForTest leaves a fresh keyboard as after power-on.
func (k *Keyboard) enabledForTest() { k.enabled = true }

// TestKeyboardEchoDuringTyping: a key pressed while the firmware's
// periodic Echo (EE, about every 5 s) is on the line is accepted by the
// firmware (it clicks; with no controller attached keys are not queued
// for the line), and the keyboard is not reset. Before the fix
// the key code arrived where the EE reply was expected: the firmware
// reset and reprogrammed the keyboard and the key was lost.
func TestKeyboardEchoDuringTyping(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("boots the firmware")
	}
	b, err := New(&Model3477, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	b.Run(600_000_000)
	const ms = cpuHz / 1000
	p := &b.IO.kbd
	for i := 0; i < 20000; i++ {
		b.Run(ms)
		if !(p.txActive && p.txByte == 0xEE) {
			continue
		}
		before := b.IO.Sound().Clicks
		logn := len(p.kbd.Log)
		p.kbd.Key(0x1C) // 'a', queued while the echo is on the line
		b.Run(100 * ms)
		if bytes.Contains(p.kbd.Log[logn:], []uint8{0xFF}) {
			t.Fatalf("keyboard reset after the echo: % X", p.kbd.Log[logn:])
		}
		if got := b.IO.Sound().Clicks - before; got != 1 {
			t.Fatalf("key lost: %d clicks, want 1", got)
		}
		return
	}
	t.Fatal("no echo seen in 20 s")
}

// TestKeyboardBufferCapped: a flood of key events (a client sending key
// messages as fast as it can) fills the keyboard's buffer and the rest
// is dropped, like a real keyboard overrun; releases of keys whose make
// was dropped are dropped too, so the terminal never sees an unpaired
// break, and a release of a held Shift is never lost.
func TestKeyboardBufferCapped(t *testing.T) {
	var k Keyboard
	k.setDefaults()
	k.Key(0x12) // Shift down (make/break)
	for i := 0; i < 100000; i++ {
		k.Key(0x1C, 0xF0, 0x1C) // 'a' tapped
	}
	if n := len(k.queued()); n > keyBufferSize {
		t.Fatalf("%d bytes queued, want at most %d", n, keyBufferSize)
	}
	k.Key(0xF0, 0x12) // Shift up must still get through
	q := k.queued()
	if len(q) < 2 || q[len(q)-2] != 0xF0 || q[len(q)-1] != 0x12 {
		t.Fatalf("shift release lost: % X", q)
	}
	// a release of a key that is not held sends nothing
	n := len(k.queued())
	k.Key(0xF0, 0x59)
	if len(k.queued()) != n {
		t.Fatal("release of a key that is not down was sent")
	}
	// replies are not capped and go first
	k.Command(0xEE)
	if b, _ := k.next(); b != 0xEE {
		t.Fatalf("first byte %02X, want the EE reply", b)
	}
}

// BenchmarkKeyboardReply: answering a command must not copy the key
// queue (it used to be O(queue) per command).
func BenchmarkKeyboardReply(b *testing.B) {
	var k Keyboard
	k.setDefaults()
	for i := 0; i < keyBufferSize; i++ {
		k.Key(0x1C)
		k.held[0x1C] = false
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		k.Command(0xEE)
		k.resp = k.resp[:0]
	}
}
