package board

// The 3477's printer port (gate-array registers 0x20030-0x20031, found by
// tracing the printer code in D000, Display-Printer mode):
//
//	0x20030 W  data byte for the printer (D000:DA96)
//	0x20031 W  control: bit 0 is the strobe, pulsed 0 then 1 for each
//	           byte (D000:DAA6-DAB5); the other bits keep their value
//	0x20031 R  status: the firmware reads it twice until both agree and
//	           dispatches on the low four bits (D000:D723, table
//	           D000:DE0E)
//
// Work in progress: the status values are being determined.
const (
	regPrnData = 0x30
	regPrnCtl  = 0x31
	// prnReady is the status of a printer that is on line and ready: the
	// firmware then initialises it and sends it data (with any other
	// value it sends a single byte and stops).
	prnReady = 0x03
)

// PrinterOutput returns the bytes the firmware sent to the printer since
// the last call.
func (g *IOGA) PrinterOutput() []uint8 {
	out := g.prn.out
	g.prn.out = nil
	return out
}

// printerPort collects the bytes the firmware strobes to the printer.
type printerPort struct {
	status uint8 // value read back from 0x20031
	data   uint8 // last byte written to 0x20030
	ctl    uint8 // last control value written to 0x20031
	out    []uint8
}

func (p *printerPort) write(off uint32, v uint8) {
	switch off {
	case regPrnData:
		p.data = v
	case regPrnCtl:
		if p.ctl&1 == 0 && v&1 != 0 { // strobe ends: the byte is taken
			p.out = append(p.out, p.data)
		}
		p.ctl = v
	}
}
