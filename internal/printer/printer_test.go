package printer

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"ibm3486/internal/board"
)

// TestFirmwareSetupIsNotAJob: what the real 3477 sends its printer at
// every start (code page, spacing, downloaded characters: no text) is
// not a print job.
func TestFirmwareSetupIsNotAJob(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("boots the firmware")
	}
	b, err := board.New(&board.Model3477, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	b.Run(600_000_000)
	out := b.IO.PrinterOutput()
	if len(out) < 100 {
		t.Fatalf("only %d bytes sent to the printer at start-up", len(out))
	}
	if d := Interpret(out); d.HasText() {
		t.Fatalf("set-up taken for a job: %q", d.Text())
	}
}

// job builds a stream with the firmware's set-up commands in front.
func job(body ...[]byte) []byte {
	setup := []byte{0x00, 0x1B, '[', 'T', 0x05, 0x00, 0x00, 0x00, 0x03, 0x52, 0x00, // code page 850
		0x1B, 'A', 0x0C, 0x1B, '2', 0x12, 0x1B, 'W', 0x00, 0x1B, 'I', 0x00,
		0x1B, '=', 0x03, 0x00, 0x14, 0x21, 0x00} // a downloaded character, skipped
	return append(setup, bytes.Join(body, nil)...)
}

func TestTextAndPages(t *testing.T) {
	d := Interpret(job(
		[]byte("HELLO WORLD\r\n"),
		[]byte{'G', 'r', 0x81, 0xE1, 'e', '\r', '\n'}, // Grüße in code page 850
		[]byte("\r\n   INDENTED\r\n\f"),
		[]byte("PAGE 2\r\n\f"),
	))
	if !d.HasText() || len(d.Pages) != 2 {
		t.Fatalf("pages %d, text %v", len(d.Pages), d.HasText())
	}
	want := "HELLO WORLD\nGrüße\n\n   INDENTED\n\f\nPAGE 2\n"
	if got := d.Text(); got != want {
		t.Fatalf("text %q\nwant %q", got, want)
	}
}

func TestStyles(t *testing.T) {
	d := Interpret(job(
		[]byte{0x1B, 'W', 0x01}, []byte("WIDE"), []byte{0x1B, 'W', 0x00},
		[]byte{0x0F}, []byte("small"), []byte{0x12},
		[]byte{0x1B, 'E'}, []byte("bold"), []byte{0x1B, 'F'},
		[]byte{0x1B, '-', 0x01}, []byte("under"), []byte("\r\n"),
	))
	r := d.Pages[0].Runs
	if len(r) != 4 || !r[0].Wide || r[1].CPI != 17.1 || !r[2].Bold || !r[3].Underline {
		t.Fatalf("runs %+v", r)
	}
	// WIDE takes 4 double-width columns at 10 cpi: small starts at 0.8"
	if x := r[1].X; x < 0.79 || x > 0.81 {
		t.Fatalf("condensed text at %.3f\", want 0.8", x)
	}
}

func TestPageLengthA4(t *testing.T) {
	// ESC C 0 n: page length n inches; 12" is not A4, 11.69" cannot be
	// given in whole inches, so check the lines form: 70 lines at 6 lpi
	d := Interpret(job([]byte{0x1B, 'C', 70}, []byte("X\r\n")))
	if d.Height < 11.6 || d.Height > 11.7 || d.Width != 8.27 {
		t.Fatalf("page %.2f x %.2f, want A4", d.Width, d.Height)
	}
}

// TestPDFStructure: the PDF has one page object per page, the text, and
// a cross-reference table whose offsets point at the objects.
func TestPDFStructure(t *testing.T) {
	d := Interpret(job([]byte("HELLO (1)\r\n\fPAGE 2\r\n")))
	pdf := d.PDF()
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatal("not a PDF file")
	}
	if n := bytes.Count(pdf, []byte("/Type /Page ")); n != 2 {
		t.Fatalf("%d pages, want 2", n)
	}
	if !bytes.Contains(pdf, []byte(`(HELLO \(1\))`)) {
		t.Fatal("text missing or not escaped")
	}
	m := regexp.MustCompile(`startxref\n(\d+)`).FindSubmatch(pdf)
	xref, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(pdf[xref:], []byte("xref")) {
		t.Fatal("startxref does not point at the xref table")
	}
	entries := strings.Split(string(pdf[xref:]), "\n")[3:]
	for i, e := range entries {
		if !strings.HasSuffix(e, " n ") {
			break
		}
		off, _ := strconv.Atoi(e[:10])
		if want := strconv.Itoa(i+1) + " 0 obj"; !bytes.HasPrefix(pdf[off:], []byte(want)) {
			t.Fatalf("xref entry %d points at %q", i+1, pdf[off:off+10])
		}
	}
}

func TestTextDocument(t *testing.T) {
	d := TextDocument("A\n\nB\n\fC\n\f")
	if len(d.Pages) != 2 || d.Text() != "A\n\nB\n\f\nC\n" {
		t.Fatalf("pages %d text %q", len(d.Pages), d.Text())
	}
}

// ebc decodes the EBCDIC letters, digits and blank used in the tests.
func ebc(c byte) rune {
	switch {
	case c >= 0xC1 && c <= 0xC9:
		return rune('A' + c - 0xC1)
	case c >= 0xD1 && c <= 0xD9:
		return rune('J' + c - 0xD1)
	case c >= 0xE2 && c <= 0xE9:
		return rune('S' + c - 0xE2)
	case c >= 0xF0 && c <= 0xF9:
		return rune('0' + c - 0xF0)
	}
	return ' '
}

// TestSCSText: SCS controls of host spool files: format controls are
// skipped, print positions move, transparent data prints, form feeds
// make pages.
func TestSCSText(t *testing.T) {
	scs := []byte{
		0x2B, 0xD2, 0x03, 0x2D, 0x00, // SPC: a format control (skipped)
		0x2B, 0xC1, 0x02, 0x84, // SHF: 132 print positions (skipped)
		0xC1, 0xC2, 0x15, // AB NL
		0x34, 0xC0, 0x05, 0xC3, 0x15, // PP to column 5, C, NL
		0x35, 0x02, 0xC4, 0xC5, 0x15, // TRN: DE
		0x0C,             // FF
		0xC6, 0x25, 0xC7, // F LF G (same column)
		0x0C,
	}
	want := "AB\n    C\nDE\n\fF\n G\n"
	if got := SCSText(scs, ebc); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// TestTextColumns: double-width and condensed runs keep their places on
// a line (each character placed by its own position).
func TestTextColumns(t *testing.T) {
	d := Interpret(job([]byte("AB"), []byte{0x0E}, []byte("CD"), []byte{0x14}, []byte("\r\n")))
	if got := d.Text(); got != "ABC D\n" {
		t.Errorf("SO line: %q, want %q", got, "ABC D\n")
	}
	d = Interpret(job([]byte{0x0F}, []byte("abcdefghij"), []byte{0x12}, []byte("X\r\n")))
	got := strings.TrimRight(d.Text(), "\n")
	if !strings.HasPrefix(got, "abcdefghij") || !strings.HasSuffix(got, "X") {
		t.Errorf("condensed then 10 cpi: %q", got)
	}
}

// TestSCSTextBounded: relative moves cannot make SCSText allocate without
// limit: a long run of line and column moves stays small.
func TestSCSTextBounded(t *testing.T) {
	var scs []byte
	for i := 0; i < 100000; i++ {
		scs = append(scs, 0x34, 0x4C, 0xFF, 0x34, 0xC8, 0xFF, 0x05)
	}
	scs = append(scs, 0xC1)
	if got := SCSText(scs, ebc); len(got) > 1<<20 {
		t.Fatalf("output of %d bytes", len(got))
	}
}

// FuzzSCSText and FuzzInterpret: no input makes the parsers panic.
func FuzzSCSText(f *testing.F) {
	f.Add([]byte{0x2B, 0xD2, 0x03, 0x2D, 0x00, 0xC1, 0x15, 0x0C})
	f.Add([]byte{0x34, 0xC4, 0x00, 0x35, 0xFF, 0x03, 0x2B})
	f.Fuzz(func(t *testing.T, b []byte) { SCSText(b, ebc) })
}

func FuzzInterpret(f *testing.F) {
	f.Add(job([]byte("HELLO\r\n\f")))
	f.Add([]byte{0x1B, '[', 'T', 0xFF, 0xFF, 0x1B, '=', 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		d := Interpret(b)
		d.Text()
		d.PDF()
	})
}
