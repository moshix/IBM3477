package kbdlayout

import "testing"

// TestFunctionsDistinctOnTwinax: every function reaches the controller
// as its own twinax code / Shift combination and decodes back to itself,
// and no function key also types a character.
func TestFunctionsDistinctOnTwinax(t *testing.T) {
	type key struct {
		seq   string
		shift bool
	}
	seen := map[key]string{}
	for _, f := range Functions {
		seq, ok := ToTwinax[f.Code]
		if !ok {
			if f.Name != "Setup" { // Setup stays inside the terminal
				t.Errorf("%s: key %02X sends nothing to the controller", f.Name, f.Code)
			}
			continue
		}
		k := key{string(seq), f.Shift}
		if code, shift, ok := OnTwinax(f); ok {
			k = key{string([]uint8{code}), shift}
		}
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s both send % X shift=%v", f.Name, other, seq, f.Shift)
		}
		seen[k] = f.Name
		if _, isChar := Chars[f.Code]; isChar {
			t.Errorf("%s uses character key %02X", f.Name, f.Code)
		}
		code, shift, ok := OnTwinax(f)
		if !ok {
			continue // PF keys: Cmd sequences, decoded by the controller
		}
		if fn, _, ok := DecodeTwinax(code, shift); !ok || fn != f.Name {
			t.Errorf("DecodeTwinax(%02X, %v) = %q, want %q", code, shift, fn, f.Name)
		}
	}
	if fn, _, _ := DecodeTwinax(0x7E, true); fn != "Reset" {
		t.Errorf("Shift+Reset should fall back to Reset, got %q", fn)
	}
}

// TestCharsDistinctOnTwinax: each character key reaches the controller
// as one code no other key sends, and decodes to its characters.
func TestCharsDistinctOnTwinax(t *testing.T) {
	owner := map[uint8]uint8{}
	for code, ch := range Chars {
		seq := ToTwinax[code]
		if len(seq) != 1 {
			t.Errorf("character key %02X (%q) sends % X", code, ch[0], seq)
			continue
		}
		if o, dup := owner[seq[0]]; dup {
			t.Errorf("keys %02X and %02X both send %02X", o, code, seq[0])
		}
		owner[seq[0]] = code
		if _, got, _ := DecodeTwinax(seq[0], false); got != ch[0] {
			t.Errorf("DecodeTwinax(%02X) = %q, want %q", seq[0], got, ch[0])
		}
		if _, got, _ := DecodeTwinax(seq[0], true); got != ch[1] {
			t.Errorf("DecodeTwinax(%02X, shift) = %q, want %q", seq[0], got, ch[1])
		}
	}
}
