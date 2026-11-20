// Package printer turns the data the 3477 sends to its printer port into
// documents (PDF and plain text).
//
// The 3477 drives a PC-style printer with IBM Proprinter / PPDS commands
// (captured from the firmware: it selects code page 850 with ESC [ T,
// sets 6 lines per inch with ESC A 0C / ESC 2, 10 characters per inch
// with DC2, downloads characters with ESC =, ...). Interpret reads such
// a stream into pages of positioned text runs; pdf.go and text.go render
// them. Commands that do not change the printed text (download of
// characters, bit images, tabs set-up, quality, ...) are parsed only to
// skip their parameters, so the stream never gets out of step.
package printer

// Control characters of the Proprinter command set.
const (
	ctlBS  = 0x08 // backspace
	ctlHT  = 0x09 // horizontal tab
	ctlLF  = 0x0A // line feed
	ctlVT  = 0x0B // vertical tab (treated as a line feed)
	ctlFF  = 0x0C // form feed: next page
	ctlCR  = 0x0D // carriage return
	ctlSO  = 0x0E // double width for the rest of the line
	ctlSI  = 0x0F // condensed: 17.1 characters per inch
	ctlDC2 = 0x12 // cancel condensed: 10 characters per inch
	ctlDC4 = 0x14 // cancel double width set by SO
	ctlCAN = 0x18 // cancel: drop the line buffer (ignored)
	ctlESC = 0x1B
)

// Run is a piece of text printed in one style at one position. X and Y
// are in inches from the top left corner of the page; Y is the baseline.
type Run struct {
	X, Y      float64
	Text      []rune
	CPI       float64 // characters per inch (before double width)
	Wide      bool    // double width
	Tall      bool    // double height
	Bold      bool
	Underline bool
}

// Page is one printed page.
type Page struct{ Runs []Run }

// Document is an interpreted print job.
type Document struct {
	Pages         []Page
	Width, Height float64 // page size in inches
}

// HasText reports whether anything visible was printed: the 3477 sends
// set-up commands to its printer at every start (624 bytes, no text),
// which is not a print job.
func (d *Document) HasText() bool {
	for _, p := range d.Pages {
		for _, r := range p.Runs {
			for _, c := range r.Text {
				if c != ' ' {
					return true
				}
			}
		}
	}
	return false
}

// state is the printer's settings while interpreting a stream.
type state struct {
	doc        *Document
	page       *Page
	x, y       float64 // print position in inches (y: baseline)
	lineHeight float64 // inches per line feed
	pendingA   float64 // spacing set by ESC A, used from ESC 2 on
	cpi        float64 // 10, 12 or 17.1
	condensed  bool
	wide       bool // ESC W 1
	wideLine   bool // SO: until the end of the line
	tall       bool
	bold       bool
	underline  bool
	pageLen    float64 // inches
	leftMargin float64
	codepage   [128]rune
	run        *Run // the run being extended
}

// Interpret reads a Proprinter data stream into a document. Pages are
// Letter width (8.5") unless the stream sets an A4 page length.
func Interpret(data []byte) *Document {
	s := &state{
		doc:        &Document{Width: 8.5, Height: 11},
		lineHeight: 1.0 / 6,
		pendingA:   12.0 / 72,
		cpi:        10,
		pageLen:    11,
		codepage:   cp850,
	}
	s.newPage()
	s.y = s.lineHeight // the first line's baseline
	i := 0
	for i < len(data) {
		c := data[i]
		i++
		switch c {
		case ctlESC:
			i = s.escape(data, i)
		case ctlCR:
			s.x = s.leftMargin
			s.run = nil
		case ctlLF, ctlVT:
			s.lineFeed(s.lineHeight)
		case ctlFF:
			s.formFeed()
		case ctlBS:
			s.x -= s.charWidth()
			if s.x < s.leftMargin {
				s.x = s.leftMargin
			}
			s.run = nil
		case ctlHT:
			// default tabs every 8 columns
			col := int((s.x-s.leftMargin)/s.charWidth()+0.5)/8*8 + 8
			s.x = s.leftMargin + float64(col)*s.charWidth()
			s.run = nil
		case ctlSO:
			s.wideLine = true
			s.run = nil
		case ctlDC4:
			s.wideLine = false
			s.run = nil
		case ctlSI:
			s.condensed = true
			s.run = nil
		case ctlDC2:
			s.condensed = false
			s.cpi = 10
			s.run = nil
		case 0x00, 0x07, 0x11, 0x13, ctlCAN, 0x7F:
			// NUL, BEL, DC1/DC3 (select/deselect), CAN, DEL: nothing printed
		default:
			if c < 0x20 {
				continue // other control characters print nothing
			}
			s.print(s.decode(c))
		}
	}
	// drop a trailing empty page (a job ends with a form feed)
	if n := len(s.doc.Pages); n > 1 && len(s.doc.Pages[n-1].Runs) == 0 {
		s.doc.Pages = s.doc.Pages[:n-1]
	}
	return s.doc
}

// decode maps a printer byte to its character in the selected code page.
func (s *state) decode(c byte) rune {
	if c < 0x80 {
		return rune(c)
	}
	return s.codepage[c-0x80]
}

// effCPI is the characters per inch actually used (condensed, 12 cpi).
func (s *state) effCPI() float64 {
	if s.condensed {
		return 17.1
	}
	return s.cpi
}

// charWidth is the advance of one character in inches.
func (s *state) charWidth() float64 {
	w := 1 / s.effCPI()
	if s.wide || s.wideLine {
		w *= 2
	}
	return w
}

// print puts one character at the print position, extending the
// current run while the style stays the same.
func (s *state) print(r rune) {
	if s.run == nil || s.run.CPI != s.effCPI() || s.run.Wide != (s.wide || s.wideLine) ||
		s.run.Tall != s.tall || s.run.Bold != s.bold || s.run.Underline != s.underline {
		s.page.Runs = append(s.page.Runs, Run{X: s.x, Y: s.y, CPI: s.effCPI(),
			Wide: s.wide || s.wideLine, Tall: s.tall, Bold: s.bold, Underline: s.underline})
		s.run = &s.page.Runs[len(s.page.Runs)-1]
	}
	s.run.Text = append(s.run.Text, r)
	s.x += s.charWidth()
}

func (s *state) newPage() {
	s.doc.Pages = append(s.doc.Pages, Page{})
	s.page = &s.doc.Pages[len(s.doc.Pages)-1]
	s.run = nil
}

// lineFeed moves down; past the end of the form the text continues on
// the next page (continuous forms).
func (s *state) lineFeed(dy float64) {
	// LF only advances the paper; CR returns the carriage
	s.wideLine = false
	s.y += dy
	s.run = nil
	if s.y > s.pageLen {
		s.newPage()
		s.y -= s.pageLen
		if s.y < s.lineHeight {
			s.y = s.lineHeight
		}
	}
}

func (s *state) formFeed() {
	s.newPage()
	s.x = s.leftMargin
	s.y = s.lineHeight
	s.wideLine = false
}

// escape handles ESC and its parameters starting at data[i]; it returns
// the index after the command.
func (s *state) escape(data []byte, i int) int {
	if i >= len(data) {
		return i
	}
	cmd := data[i]
	i++
	arg := func(n int) []byte { // the next n parameter bytes (fewer at the end)
		if i+n > len(data) {
			n = len(data) - i
		}
		p := data[i : i+n]
		i += n
		return p
	}
	byteArg := func() byte {
		if p := arg(1); len(p) == 1 {
			return p[0]
		}
		return 0
	}
	// length-prefixed data: n1 n2 (little-endian), then that many bytes
	skipLen := func() []byte {
		p := arg(2)
		if len(p) < 2 {
			return nil
		}
		return arg(int(p[0]) | int(p[1])<<8)
	}
	untilNUL := func() {
		for i < len(data) && data[i] != 0 {
			i++
		}
		if i < len(data) {
			i++
		}
	}
	s.run = nil
	switch cmd {
	case '0': // 1/8 inch line spacing
		s.lineHeight = 1.0 / 8
	case '1': // 7/72 inch
		s.lineHeight = 7.0 / 72
	case '2': // start the spacing set by ESC A (default 1/6 inch)
		s.lineHeight = s.pendingA
	case '3': // n/216 inch
		if n := byteArg(); n > 0 {
			s.lineHeight = float64(n) / 216
		}
	case 'A': // n/72 inch, effective after ESC 2
		if n := byteArg(); n > 0 {
			s.pendingA = float64(n) / 72
		}
	case 'J': // one line feed of n/216 inch
		s.lineFeed(float64(byteArg()) / 216)
	case 'C': // page length: n lines, or 0 then n inches
		if n := byteArg(); n > 0 {
			s.setPageLength(float64(n) * s.lineHeight)
		} else if in := byteArg(); in > 0 {
			s.setPageLength(float64(in))
		}
	case ':': // 12 characters per inch
		s.cpi, s.condensed = 12, false
	case 'W': // double width on/off
		s.wide = byteArg()&1 != 0
	case 'E', 'G': // emphasized, double strike: bold
		s.bold = true
	case 'F', 'H':
		s.bold = false
	case '-': // underline on/off
		s.underline = byteArg()&1 != 0
	case 'X': // left and right margins in columns (1-based)
		if p := arg(2); len(p) == 2 && p[0] > 0 {
			s.leftMargin = float64(p[0]-1) / s.effCPI()
			if s.x < s.leftMargin {
				s.x = s.leftMargin
			}
		}
	case '^': // print the next byte as a character, even a control code
		if p := arg(1); len(p) == 1 {
			s.print(s.decode(p[0]))
		}
	case '\\': // print n bytes as characters
		for _, b := range skipLen() {
			s.print(s.decode(b))
		}
	case '=', 'K', 'L', 'Y', 'Z': // download characters, bit images
		skipLen()
	case 'B', 'D': // vertical / horizontal tab stops, ended by NUL
		untilNUL()
	case '5', 'I', 'N', 'P', 'Q', 'S', 'U', '_', 'x', 'e', 'f', 'k', 'r':
		byteArg() // one parameter that does not change the text
	case '[':
		sub := byteArg()
		p := skipLen()
		switch sub {
		case 'T': // code page: ... last two bytes are the code page number
			if len(p) >= 2 {
				switch int(p[len(p)-2])<<8 | int(p[len(p)-1]) {
				case 437:
					s.codepage = cp437
				case 850:
					s.codepage = cp850
				}
			}
		case '@': // double height and line spacing: byte 3 bit 0-3 = height
			if len(p) >= 4 {
				s.tall = p[3]&0x0F == 2 || p[3]&0x0F == 3
			}
		}
	default:
		// ESC 4/6/7/8/9/O/R/T/j and others: no parameters
	}
	return i
}

// setPageLength sets the form length; an A4 length (about 11.69") also
// selects the A4 width.
func (s *state) setPageLength(in float64) {
	if in < 1 || in > 22 {
		return
	}
	s.pageLen = in
	s.doc.Height = in
	if in > 11.5 && in < 11.9 {
		s.doc.Width = 8.27
	}
}

// cp437 and cp850 are the upper halves (0x80-0xFF) of the PC code pages.
var cp437 = [128]rune{
	'Ç', 'ü', 'é', 'â', 'ä', 'à', 'å', 'ç', 'ê', 'ë', 'è', 'ï', 'î', 'ì', 'Ä', 'Å',
	'É', 'æ', 'Æ', 'ô', 'ö', 'ò', 'û', 'ù', 'ÿ', 'Ö', 'Ü', '¢', '£', '¥', '₧', 'ƒ',
	'á', 'í', 'ó', 'ú', 'ñ', 'Ñ', 'ª', 'º', '¿', '⌐', '¬', '½', '¼', '¡', '«', '»',
	'░', '▒', '▓', '│', '┤', '╡', '╢', '╖', '╕', '╣', '║', '╗', '╝', '╜', '╛', '┐',
	'└', '┴', '┬', '├', '─', '┼', '╞', '╟', '╚', '╔', '╩', '╦', '╠', '═', '╬', '╧',
	'╨', '╤', '╥', '╙', '╘', '╒', '╓', '╫', '╪', '┘', '┌', '█', '▄', '▌', '▐', '▀',
	'α', 'ß', 'Γ', 'π', 'Σ', 'σ', 'µ', 'τ', 'Φ', 'Θ', 'Ω', 'δ', '∞', 'φ', 'ε', '∩',
	'≡', '±', '≥', '≤', '⌠', '⌡', '÷', '≈', '°', '∙', '·', '√', 'ⁿ', '²', '■', ' ',
}

var cp850 = [128]rune{
	'Ç', 'ü', 'é', 'â', 'ä', 'à', 'å', 'ç', 'ê', 'ë', 'è', 'ï', 'î', 'ì', 'Ä', 'Å',
	'É', 'æ', 'Æ', 'ô', 'ö', 'ò', 'û', 'ù', 'ÿ', 'Ö', 'Ü', 'ø', '£', 'Ø', '×', 'ƒ',
	'á', 'í', 'ó', 'ú', 'ñ', 'Ñ', 'ª', 'º', '¿', '®', '¬', '½', '¼', '¡', '«', '»',
	'░', '▒', '▓', '│', '┤', 'Á', 'Â', 'À', '©', '╣', '║', '╗', '╝', '¢', '¥', '┐',
	'└', '┴', '┬', '├', '─', '┼', 'ã', 'Ã', '╚', '╔', '╩', '╦', '╠', '═', '╬', '¤',
	'ð', 'Ð', 'Ê', 'Ë', 'È', 'ı', 'Í', 'Î', 'Ï', '┘', '┌', '█', '▄', '¦', 'Ì', '▀',
	'Ó', 'ß', 'Ô', 'Ò', 'õ', 'Õ', 'µ', 'þ', 'Þ', 'Ú', 'Û', 'Ù', 'ý', 'Ý', '¯', '´',
	'­', '±', '‗', '¾', '¶', '§', '÷', '¸', '°', '¨', '·', '¹', '³', '²', '■', ' ',
}
