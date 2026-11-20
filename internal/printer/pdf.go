package printer

import (
	"bytes"
	"fmt"
	"strconv"
)

// PDF renders the document as a PDF file. It uses the PDF base fonts
// Courier and Courier-Bold (built into every PDF reader, nothing is
// embedded) with WinAnsi encoding. A run's font size follows its
// characters per inch: Courier is 0.6 em wide, so 10 cpi is 12 pt.
// Double width and double height scale the text matrix.
func (d *Document) PDF() []byte {
	var b bytes.Buffer
	var offsets []int // byte offset of each object, for the xref table
	obj := func(body string) int {
		offsets = append(offsets, b.Len())
		n := len(offsets)
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
		return n
	}
	b.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	// objects 1-4 are fixed: catalog, page tree, the two fonts; the page
	// tree lists pages created after it, so its kids are known up front
	npages := len(d.Pages)
	first := 5 // page i is object first+2i, its contents first+2i+1
	kids := make([]byte, 0, npages*8)
	for i := 0; i < npages; i++ {
		kids = append(kids, []byte(strconv.Itoa(first+2*i)+" 0 R ")...)
	}
	w, h := d.Width*72, d.Height*72
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, npages))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Courier-Bold /Encoding /WinAnsiEncoding >>")
	for i := range d.Pages {
		content := d.pageContent(&d.Pages[i])
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] "+
			"/Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			w, h, first+2*i+1))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

// physicalLeft is the distance of the printer's first column from the
// left edge of the paper, in inches.
const physicalLeft = 0.25

// pageContent is the content stream of one page.
func (d *Document) pageContent(p *Page) []byte {
	var c bytes.Buffer
	ph := d.Height * 72
	for _, r := range p.Runs {
		size := 72 / (r.CPI * 0.6) // points
		sx, sy := size, size
		if r.Wide {
			sx *= 2
		}
		if r.Tall {
			sy *= 2
		}
		// a printer cannot print at the very edge of the paper: its
		// first column is about a quarter inch in
		x, y := (r.X+physicalLeft)*72, ph-r.Y*72
		font := "F1"
		if r.Bold {
			font = "F2"
		}
		fmt.Fprintf(&c, "BT /%s 1 Tf %.3f 0 0 %.3f %.2f %.2f Tm (%s) Tj ET\n",
			font, sx, sy, x, y, pdfString(r.Text))
		if r.Underline {
			width := float64(len(r.Text)) * sx * 0.6
			fmt.Fprintf(&c, "%.2f %.2f %.2f 0.6 re f\n", x, y-2, width)
		}
	}
	return c.Bytes()
}

// pdfString encodes text for a PDF string in WinAnsi encoding, escaping
// ( ) \ and writing bytes above 0x7E as octal escapes.
func pdfString(text []rune) string {
	var b bytes.Buffer
	for _, r := range text {
		c := winAnsi(r)
		switch {
		case c == '(' || c == ')' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c > 0x7E:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// winAnsi maps a character to its WinAnsi (Windows-1252) byte. Latin-1
// characters keep their code; box-drawing and block characters, which
// Courier lacks, become - | + #; anything else becomes ?.
func winAnsi(r rune) byte {
	switch {
	case r < 0x80:
		return byte(r)
	case r >= 0xA0 && r <= 0xFF:
		return byte(r)
	}
	if c, ok := winAnsiExtra[r]; ok {
		return c
	}
	switch {
	case r == '─' || r == '═' || r == '‗':
		return '-'
	case r == '│' || r == '║':
		return '|'
	case r >= 0x2500 && r <= 0x257F: // other box drawing: corners, crossings
		return '+'
	case r >= 0x2580 && r <= 0x25A0: // blocks and shades
		return '#'
	}
	return '?'
}

// winAnsiExtra are the WinAnsi characters in 0x80-0x9F.
var winAnsiExtra = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87,
	'ˆ': 0x88, '‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E, '‘': 0x91,
	'’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97, '˜': 0x98,
	'™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
	'∙': 0x95, // bullet operator: bullet
}
