package board

// Twinax models the 3477's twinax station engine: a small microcoded
// receiver/transmitter per station that runs programs stored in RAM.
// Reverse-engineered from POST test 3 (F000:1FA4) and the runtime driver;
// see TWINAX.md. Confidence tags there: [C] confirmed, [L] likely, [G] guess.
//
// Two channels (one per station address / display session). Channel c:
//
//	RAM 0x180+0x10*c   control block (+0/+1 data DMA address, +2/+3 count,
//	                   +4 mode, +5 receive count, +6/+7 receive pointer,
//	                   +8 station address, +B polled flag, +C poll status,
//	                   +D scan code for the next poll response)
//	0x20000+c          channel register: write 0x80 = (re)start
//	0x20002+c          interrupt status, write 1 to clear (bit0 poll
//	                   response sent, bit1 command received, bit2 data
//	                   transfer done)
//	RAM 0x80+0x80*c    four 32-byte microprograms, one per command class
//	RAM 0x60           32-entry class table: class = table[cmd&0x1F] >> 6
//
// Received commands are run through the class program; the micro-ops
// store the command and data bytes by DMA, transfer bulk data, set status
// bits and send poll responses.
type Twinax struct {
	b  *Board
	ch [2]twinaxChan
	// POST wrap test: a 0->1 edge on 0x20009 bit7 arms one wrap transfer,
	// fired by the next 0x20015 write with bit6 set (F000:2645).
	armed  bool
	last09 uint8
}

type twinaxChan struct {
	unacked bool // a poll response was sent and not yet acknowledged
	toggle  bool // flips on every poll response
	parity  bool // line parity error latched
	// fresh: the firmware put a new byte into +D (it writes 0x10 to the
	// channel register after storing it, C000:6B12). Only then does a
	// poll response carry +D; once acknowledged the data is old. When its
	// key queue is empty the firmware leaves the last key in +D, so
	// reporting +D on every poll would repeat that key forever.
	fresh bool
	// rearm: an End of Queue was received and the firmware has not yet
	// re-armed the receiver (it writes 0x80 to the channel register in
	// its receive set-up, C000:726D, once the queue is executed). Until
	// then the station is busy: some queues (Load Cursor) finish only
	// after the poll status bit has been cleared, and a queue sent in
	// that window is lost, which put typed characters in the wrong place
	// on the screen.
	rearm bool
	// settle: the receiver was just re-armed; the firmware still finishes
	// its set-up after the 0x80 write (C000:7275 on), and a queue sent in
	// that same instant was dropped (the station then stayed busy for
	// good). The station reports busy for one more poll, a margin a real
	// controller's slower polling gives anyway.
	settle bool
}

// cmdEndOfQueue is the twinax End of Queue command.
const cmdEndOfQueue = 0x62

// Response is what a station sent back to the controller for one message.
type Response struct {
	Polled bool    // a poll response was produced
	Status uint8   // poll response status byte
	Scan   uint8   // poll response data byte (+D: keyboard scan code)
	Data   []uint8 // Read Activate data
}

func newTwinax(b *Board) *Twinax { return &Twinax{b: b} }

// stationAddr returns channel c's 3-bit station address (0x20008: bits
// 0-2 channel 0, bits 3-5 channel 1).
func (t *Twinax) stationAddr(c int) uint8 {
	return t.b.IO.regs[regStationAddr] >> (3 * uint(c)) & 7
}

// Deliver presents one controller message (command frame plus data
// frames, as addressed on the line) to the station. It returns the
// station's response and false if no channel accepted the message.
func (t *Twinax) Deliver(addr, cmd uint8, data []uint8) (Response, bool) {
	ctl := t.b.IO.regs[regTwinaxCtl]
	if ctl&0x80 == 0 {
		return Response{}, false // engine not running
	}
	for c := 0; c < 2; c++ {
		enable := uint8(0x02) << uint(c)
		if ctl&enable != 0 && t.stationAddr(c) == addr {
			resp := t.run(c, cmd, data, false)
			if cmd == cmdEndOfQueue {
				t.ch[c].rearm = true
			}
			return resp, true
		}
	}
	return Response{}, false
}

// run interprets channel c's class program for one message.
func (t *Twinax) run(c int, cmd uint8, data []uint8, badParity bool) Response {
	mem := &t.b.mem
	io := t.b.IO
	ch := &t.ch[c]
	blk := uint32(0x180 + 0x10*c)
	if badParity {
		ch.parity = true
	}
	cls := mem[0x60+uint32(cmd&0x1F)] >> 6
	pc := uint32(0x80+0x80*c) + uint32(cls)*0x20
	var resp Response
	// h[] are the engine's hardware registers; h[0..1] = DMA address,
	// h[2..3] = DMA count, h[6] = poll status, h[7] = poll data.
	var h [16]uint8
	var acc uint8
	dmaAddr := func() uint32 { return uint32(h[1])<<8 | uint32(h[0]) }
	setAddr := func(a uint32) { h[0], h[1] = uint8(a), uint8(a>>8) }
	count := func() uint16 { return uint16(h[3])<<8 | uint16(h[2]) }
	setCount := func(n uint16) { h[2], h[3] = uint8(n), uint8(n>>8) }
	// store writes one byte at the DMA address (DMA addresses are
	// offsets in the low 64K, where the receive buffers live)
	store := func(v uint8) {
		if count() == 0 {
			return
		}
		a := dmaAddr()
		mem[a] = v
		setAddr(a + 1)
		setCount(count() - 1)
	}
	di := 0 // next data frame to receive
	for steps := 0; steps < 32; steps++ {
		op := mem[pc]
		pc++
		switch {
		case op >= 0x20 && op <= 0x2F: // h[n] = acc
			h[op&15] = acc
		case op >= 0x30 && op <= 0x3F: // block[n] = acc
			mem[blk+uint32(op&15)] = acc
		case op >= 0x40 && op <= 0x4F: // acc = h[n]
			acc = h[op&15]
		case op >= 0x50 && op <= 0x5F: // acc = block[n]
			acc = mem[blk+uint32(op&15)]
		case op == 0x84: // end (0x84 first = program disabled)
			return resp
		case op == 0x88: // end if no transfer was set up
			if acc == 0 {
				return resp
			}
		case op == 0x82: // store the command byte, "command received"
			store(cmd)
			io.regs[regTwinaxStatus+c] |= 0x02
		case op == 0xA2: // receive the data frames
			for ; di < len(data); di++ {
				store(data[di])
			}
		case op == 0xA1: // transmit count bytes from memory
			for count() > 0 {
				a := dmaAddr()
				resp.Data = append(resp.Data, mem[a])
				setAddr(a + 1)
				setCount(count() - 1)
			}
		case op == 0x90: // data transfer complete
			io.regs[regTwinaxStatus+c] |= 0x04
		case op == 0xC0: // send the poll response (class 2 only)
			if cls != 2 {
				break
			}
			if cmd&0x40 != 0 { // poll with reset-parity modifier
				ch.parity = false
			}
			if cmd&0x20 != 0 { // Ack: the last response was received
				ch.unacked = false
				ch.fresh = false // its key reached the controller
			}
			st := h[6]
			if ch.unacked || ch.rearm || ch.settle || io.regs[regTwinaxStatus+c]&0x02 != 0 {
				st |= 0x80 // busy: a command queue is being processed
			}
			if ch.parity {
				st |= 0x40
			}
			ch.toggle = !ch.toggle
			if ch.toggle {
				st |= 0x20
			}
			if cmd&0x20 == 0 {
				ch.unacked = true
				ch.settle = false // reported busy on this poll
			}
			resp.Polled, resp.Status = true, st
			if ch.fresh {
				resp.Scan = h[7]
			}
			io.regs[regTwinaxStatus+c] |= 0x01 // poll response sent
		}
	}
	return resp
}

// write handles gate-array writes that belong to the twinax engine and
// reports whether it consumed them.
func (t *Twinax) write(off uint32, v uint8) bool {
	io := t.b.IO
	switch off {
	case regChannel, regChannel + 1: // channel (re)start
		c := int(off - regChannel)
		if v&0x80 != 0 {
			t.ch[c].unacked, t.ch[c].toggle = false, false
			if t.ch[c].rearm {
				t.ch[c].settle = true
			}
			t.ch[c].rearm = false
		}
		if v&0x10 != 0 { // new poll data in +D (C000:6B12)
			t.ch[c].fresh = true
		}
		return true // reads return 0
	case regTwinaxCtl:
		if t.last09&0x80 == 0 && v&0x80 != 0 {
			t.armed = true
		}
		t.last09 = v
		io.regs[off] = v
		return true
	case regWrapMode:
		io.regs[off] = v
		if v&0x40 != 0 && t.armed {
			t.armed = false
			t.wrap(v)
		}
		return true
	}
	return false
}

// wrap performs one POST wrap-test transfer (F000:2139-23A2): the
// gate array loops a frame built from 0x20024-0x20027 back into the
// selected channel, or (mode bit7) reads memory by DMA, and leaves the
// result in 0x2002B.
func (t *Twinax) wrap(mode uint8) {
	io := t.b.IO
	c := 0
	if io.regs[regTwinaxCtl]&0x02 == 0 {
		c = 1
	}
	if mode&0x80 != 0 { // memory wrap: last byte of a DMA read
		a := uint32(io.regs[0x20]) | uint32(io.regs[0x21])<<8
		n := uint32(io.regs[0x22]) | uint32(io.regs[0x23])<<8
		io.regs[regWrapResult] = t.b.mem[(a+n-1)&0xFFFF]
		return
	}
	addr := io.regs[0x24] & 7
	if addr != t.stationAddr(c) {
		return
	}
	r := t.run(c, io.regs[0x26], []uint8{io.regs[0x27]}, mode&0x10 != 0)
	if r.Polled {
		io.regs[regWrapResult] = r.Status
	}
}
