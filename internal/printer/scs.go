package printer

import "strings"

// scsMaxPos is the largest print position and line of a form: the PP and
// SVF/SHF values are single bytes.
const scsMaxPos = 255

// SCSText renders SCS print data (what an AS/400 sends a 5250 printer;
// IBM's 5250 attachment manual, chapter 3.6) as plain text: one line per
// printed line, a form feed (\f) between pages. decode maps an EBCDIC
// byte to its character in the session's codepage.
//
// Handled: New Line (15), Interchange Record Separator (1E), Line Feed
// (25), Carriage Return (0D), Form Feed (0C), Required Form Feed (3A),
// Backspace (16), Horizontal Tab (05), Print Position (34 ff vv), Required
// and Numeric Space (41, E1), Transparent (35 nn data: printed as data),
// ASCII Transparent (03 nn data: skipped) and the Format controls (2B cc
// nn ..., where nn counts the remaining bytes including itself: skipped).
// Other control codes (below 40) print nothing.
func SCSText(scs []byte, decode func(byte) rune) string {
	var out strings.Builder
	var page [][]rune
	line, col := 0, 0
	put := func(r rune) {
		if col >= scsMaxPos {
			return // beyond the widest form: not printed
		}
		for len(page) <= line {
			page = append(page, nil)
		}
		for len(page[line]) <= col {
			page[line] = append(page[line], ' ')
		}
		page[line][col] = r
		col++
	}
	endPage := func() {
		if out.Len() > 0 {
			out.WriteString("\f")
		}
		for _, l := range page {
			out.WriteString(strings.TrimRight(string(l), " "))
			out.WriteByte('\n')
		}
		page, line, col = nil, 0, 0
	}
	for i := 0; i < len(scs); i++ {
		c := scs[i]
		// moves are bounded to the largest form (255 positions and
		// lines): a stream of relative moves must not allocate without
		// limit; past the last line the text goes on on the next page
		if col > scsMaxPos {
			col = scsMaxPos
		}
		if line >= scsMaxPos {
			endPage()
		}
		switch c {
		case 0x15, 0x1E: // NL, IRS
			line, col = line+1, 0
		case 0x25: // LF
			line++
		case 0x0D: // CR
			col = 0
		case 0x0C, 0x3A: // FF, RFF
			endPage()
		case 0x16: // BS
			if col > 0 {
				col--
			}
		case 0x05: // HT: next tab stop, every 8 columns
			col = col/8*8 + 8
		case 0x34: // PP ff vv
			if i+2 < len(scs) {
				f, v := scs[i+1], int(scs[i+2])
				switch f {
				case 0xC0: // absolute horizontal (1-based)
					if v > 0 {
						col = v - 1
					}
				case 0xC8: // relative horizontal
					col += v
				case 0xC4: // absolute vertical: a line above means the next page
					if v > 0 {
						if v-1 < line {
							endPage()
						}
						line = v - 1
					}
				case 0x4C: // relative vertical
					line += v
				}
			}
			i += 2
		case 0x35: // TRN nn: nn bytes printed as data
			if i+1 < len(scs) {
				n := int(scs[i+1])
				for j := 0; j < n && i+2+j < len(scs); j++ {
					put(decode(scs[i+2+j]))
				}
				i += 1 + n
			}
		case 0x03: // ATRN nn: ASCII for 3197-attached printers, skipped
			if i+1 < len(scs) {
				i += 1 + int(scs[i+1])
			}
		case 0x2B: // FMT cc nn ...: nn counts itself and what follows
			if i+2 < len(scs) {
				n := int(scs[i+2])
				if n < 1 {
					n = 1
				}
				i += 1 + n
			} else {
				i = len(scs)
			}
		case 0x41, 0xE1: // required space, numeric space
			put(' ')
		default:
			if c >= 0x40 {
				put(decode(c))
			}
		}
	}
	if len(page) > 0 {
		endPage()
	}
	return out.String()
}
