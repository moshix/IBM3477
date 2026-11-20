package printer

import (
	"math"
	"sort"
	"strings"
)

// maxTextColumns bounds a text line (a 13.2" form at 17.1 cpi is 226).
const maxTextColumns = 400

// Text renders the document as plain text: one line per printed line
// (blank lines where the paper advanced further), pages separated by a
// form feed. Each run is placed on a character grid of its own pitch;
// where text was printed over other text, the later character wins
// unless it is a blank.
func (d *Document) Text() string {
	var out strings.Builder
	for pi, p := range d.Pages {
		if pi > 0 {
			out.WriteString("\f\n")
		}
		// group runs by line (baseline)
		lines := map[int][]Run{}
		var keys []int
		for _, r := range p.Runs {
			k := int(math.Round(r.Y * 1000))
			if _, ok := lines[k]; !ok {
				keys = append(keys, k)
			}
			lines[k] = append(lines[k], r)
		}
		sort.Ints(keys)
		prev := 0.0 // baseline of the previous line, inches
		for li, k := range keys {
			y := float64(k) / 1000
			// blank lines for paper advanced beyond one line (at 6 lpi)
			gap := 1
			if li == 0 {
				gap = int(math.Round(y * 6))
			} else {
				gap = int(math.Round((y - prev) * 6))
			}
			for g := 1; g < gap; g++ {
				out.WriteByte('\n')
			}
			prev = y
			// one grid per line, at its finest pitch; every character is
			// placed by its own position, so runs of different pitch or
			// double width keep their places (a wide character is
			// followed by a blank column)
			grid := 0.0
			for _, r := range lines[k] {
				if p := 1 / r.CPI; grid == 0 || p < grid {
					grid = p
				}
			}
			var row []rune
			for _, r := range lines[k] {
				adv := 1 / r.CPI
				if r.Wide {
					adv *= 2
				}
				for i, c := range r.Text {
					col := int(math.Round((r.X + float64(i)*adv) / grid))
					if col < 0 || col > maxTextColumns {
						continue
					}
					for len(row) <= col {
						row = append(row, ' ')
					}
					if c != ' ' || row[col] == ' ' {
						row[col] = c
					}
				}
			}
			out.WriteString(strings.TrimRight(string(row), " "))
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// TextDocument lays plain text out as printed pages without a printer in
// between: 10 characters and 6 lines per inch on Letter paper (66 lines
// a page), a form feed (\f) starting a new page. Used for a screen copy
// when the terminal has no printer of its own.
func TextDocument(text string) *Document {
	d := &Document{Width: 8.5, Height: 11}
	page := Page{}
	line := 0
	flush := func() {
		d.Pages = append(d.Pages, page)
		page, line = Page{}, 0
	}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for strings.HasPrefix(l, "\f") {
			flush()
			l = l[1:]
		}
		if line == 66 {
			flush()
		}
		line++
		if strings.TrimSpace(l) != "" {
			page.Runs = append(page.Runs, Run{X: 0, Y: float64(line) / 6, Text: []rune(l), CPI: 10})
		}
	}
	if len(page.Runs) > 0 || len(d.Pages) == 0 {
		d.Pages = append(d.Pages, page)
	}
	return d
}
