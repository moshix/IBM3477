package board

// The 3477's speaker is driven by two gate-array registers (found by
// tracing the firmware):
//
//	0x20010 W  tone control. Bit 7 = tone on, bits 0-4 = volume (the
//	           alarm forces 0x1F, C000:5972), bits 5-6 unknown (cleared
//	           for the alarm, possibly the pitch [G]). The timer interrupt
//	           (60 Hz) switches the tone on for the requested number of
//	           ticks, ss:[0x45] (C000:5938-59CD): 50 for the host alarm,
//	           13 for local error beeps. POST writes 0x9B (a short beep at
//	           power-on) and then 0. The low bits keep the volume of the
//	           Setup's click/alarm choice (ss:[0x382] alarm, ss:[0x383]
//	           click).
//	0x20011 W  key click: the firmware writes 4 after each key
//	           (C000:5D08), with the click volume just written to 0x20010.
const (
	regSound = 0x10
	regClick = 0x11
)

// Sound is the speaker state for the browser.
type Sound struct {
	ToneOn bool   // a tone is sounding
	Volume uint8  // 0-31, of the tone, or of the last click
	Clicks uint32 // key clicks so far (each one is a short tick)
}

// sound records writes to the speaker registers.
type sound struct {
	ctl    uint8 // last value written to 0x20010
	clicks uint32
}

func (s *sound) write(off uint32, v uint8) {
	switch off {
	case regSound:
		s.ctl = v
	case regClick:
		s.clicks++
	}
}

// Sound returns the speaker state.
func (g *IOGA) Sound() Sound {
	return Sound{ToneOn: g.snd.ctl&0x80 != 0, Volume: g.snd.ctl & 0x1F, Clicks: g.snd.clicks}
}
