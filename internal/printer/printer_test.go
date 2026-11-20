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
