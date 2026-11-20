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
	if !bytes.Equal(k.out, []uint8{0xEE, 0x1C}) {
		t.Fatalf("echo reply: out % X, want EE 1C", k.out)
	}
	k.out, k.last = []uint8{0x1C}, 0xF0 // F0 already sent, 1C to follow
	k.Command(0xEE)
	if !bytes.Equal(k.out, []uint8{0x1C, 0xEE}) {
		t.Fatalf("reply inside a break: out % X, want 1C EE", k.out)
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
