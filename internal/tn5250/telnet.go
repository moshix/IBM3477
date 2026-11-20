// Copyright 2026 by moshix. All rights reserved.

package tn5250

import (
	"bufio"
	"errors"
	"io"
	"sync"
)

// telnet.go ports lib5250's transport layer: the IAC/telnet state machine and
// TN5250E negotiation (telnetstr.c), the GDS record framing (stream.c) and the
// record byte-cursor (record.c). The C stream is an abstract object over a
// socket; here it wraps any io.ReadWriteCloser so tests can drive it with an
// in-memory pipe.

// Telnet commands and options (telnetstr.c).
const (
	tnEOR  = 239 // end-of-record marker (after IAC)
	tnSE   = 240
	tnSB   = 250
	tnWILL = 251
	tnWONT = 252
	tnDO   = 253
	tnDONT = 254
	tnIAC  = 255

	optTransmitBinary = 0
	optTimingMark     = 6
	optTerminalType   = 24
	optEndOfRecord    = 25
	optNewEnviron     = 39
)

// NEW-ENVIRON / TERMINAL-TYPE sub-negotiation bytes (telnetstr.c).
const (
	sbIS       = 0
	sbSEND     = 1
	envVAR     = 0
	envVALUE   = 1
	envUSERVAR = 3
)

// IAC state-machine states (telnetstr.c).
const (
	stNoData = iota
	stData
	stHaveIAC
	stHaveVerb
	stHaveSB
	stHaveSBIAC
)

// eorMarker is returned by getByte in place of the C's -END_OF_RECORD.
const eorMarker = -1000

// envEntry is one NEW-ENVIRON variable in our IS reply.
type envEntry struct {
	name    string
	value   string
	userVar bool // USERVAR (3) vs VAR (0)
}

// stream is the Go analogue of Tn5250Stream for the telnet transport.
type stream struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader

	state int
	verb  byte
	sbBuf []byte

	termType string     // e.g. "IBM-3179-2"
	env      []envEntry // NEW-ENVIRON vars sent in the IS reply

	wmu sync.Mutex // serialises writes to conn (Run vs SendAID goroutines)

	// notFive250 is set when the host shows it is a 3270 (SNA/mainframe)
	// host, not a 5250 one; the read loop then ends the session.
	notFive250 error
}

// ErrNot5250Host ends a session with a host that speaks 3270 (an IBM
// mainframe/SNA host behind TN3270), not 5250: this terminal cannot be
// driven by it.
var ErrNot5250Host = errors.New("a 3270 (SNA) host, not 5250")

// optTN3270E is the TN3270E telnet option (RFC 2355). Only 3270 servers
// ask for it; a 5250 host never does.
const optTN3270E = 40

// is5250Record reports whether rec has the GDS header every 5250 record
// starts with: 2-byte length, record type 12A0 (sendPacket builds the
// same). 3270 records start with a 3270 command instead (e.g. F5).
func is5250Record(rec []byte) bool {
	return len(rec) >= 4 && rec[2] == 0x12 && rec[3] == 0xA0
}

// newStream wraps a connection and seeds the negotiation replies. termType is
// reported to TERMINAL-TYPE; codepage feeds the CODEPAGE NEW-ENVIRON value.
// deviceName, when non-empty, is sent as DEVNAME to bind a specific device; an
// empty deviceName is OMITTED (matching tn5250, so the host auto-assigns).
// kbdType is the KBDTYPE (defaults to "USB").
func newStream(conn io.ReadWriteCloser, termType, codepage, deviceName, kbdType string) *stream {
	codePage := codepageInfo(codepage)
	if kbdType == "" {
		kbdType = "USB"
	}
	// tn5250 emits every CONFIGURED NEW-ENVIRON variable with the VAR (0) tag
	// (telnet_stream_sb_var_value, telnetstr.c:570-576) and OMITS an unconfigured
	// DEVNAME. We do NOT send CHARSET — lib5250 never sends it; CODEPAGE (CCSID)
	// is the authoritative value.
	env := make([]envEntry, 0, 3)
	if deviceName != "" {
		env = append(env, envEntry{name: "DEVNAME", value: deviceName, userVar: false})
	}
	env = append(env,
		envEntry{name: "KBDTYPE", value: kbdType, userVar: false},
		envEntry{name: "CODEPAGE", value: codePage, userVar: false},
	)
	return &stream{
		conn:     conn,
		r:        bufio.NewReader(conn),
		state:    stData,
		termType: termType,
		env:      env,
	}
}

// codepageInfo maps our codepage selector to the CODEPAGE (CCSID) advertised in
// NEW-ENVIRON. lib5250 sends only CODEPAGE (never CHARSET/GCSGID), and the CCSID
// number is the authoritative value, so we send only this.
func codepageInfo(codepage string) (codePage string) {
	switch codepage {
	case "500", "cp500":
		return "500"
	case "273", "cp273":
		return "273"
	case "277", "cp277":
		return "277"
	case "278", "cp278":
		return "278"
	case "280", "cp280":
		return "280"
	case "284", "cp284":
		return "284"
	case "285", "cp285":
		return "285"
	case "297", "cp297":
		return "297"
	case "871", "cp871":
		return "871"
	case "870", "cp870":
		return "870"
	default: // 37 and variants
		return "037"
	}
}

// ── raw byte I/O ─────────────────────────────────────────────────────────

func (s *stream) readRaw() (byte, error) { return s.r.ReadByte() }

func (s *stream) writeRaw(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.conn.Write(b)
	return err
}

// ── IAC state machine (telnet_stream_get_byte) ──────────────────────────

// getByte returns the next 5250 data byte, transparently answering telnet
// negotiation as it goes. It returns eorMarker at an IAC EOR, or an error on
// disconnect.
func (s *stream) getByte() (int, error) {
	for {
		if s.state == stNoData {
			s.state = stData
		}
		b, err := s.readRaw()
		if err != nil {
			return 0, err
		}
		switch s.state {
		case stData:
			if b == tnIAC {
				s.state = stHaveIAC
			} else {
				return int(b), nil
			}
		case stHaveIAC:
			switch b {
			case tnIAC:
				// Escaped 0xFF: a literal data byte.
				s.state = stData
				return int(tnIAC), nil
			case tnDO, tnDONT, tnWILL, tnWONT:
				s.verb = b
				s.state = stHaveVerb
			case tnSB:
				s.state = stHaveSB
				s.sbBuf = s.sbBuf[:0]
			case tnEOR:
				s.state = stData
				return eorMarker, nil
			default:
				s.state = stNoData // best-effort recovery, as in the C
			}
		case stHaveVerb:
			s.doVerb(s.verb, b)
			s.state = stNoData
			if s.notFive250 != nil {
				// stop at once: a TN3270E server now waits for an answer
				// to its option, no data would follow
				return 0, s.notFive250
			}
		case stHaveSB:
			if b == tnIAC {
				s.state = stHaveSBIAC
			} else {
				s.sbBuf = append(s.sbBuf, b)
			}
		case stHaveSBIAC:
			switch b {
			case tnIAC:
				s.sbBuf = append(s.sbBuf, tnIAC)
			case tnSE:
				s.doSB(s.sbBuf)
				s.state = stNoData
			default:
				s.state = stHaveSB
			}
		}
	}
}

// doVerb answers a DO/DONT/WILL/WONT (telnet_stream_do_verb). We agree to the
// four options the AS/400 needs and refuse everything else.
func (s *stream) doVerb(verb, what byte) {
	reply := []byte{tnIAC, 0, what}
	if what == optTN3270E && (verb == tnDO || verb == tnWILL) {
		s.notFive250 = ErrNot5250Host // a TN3270E server
	}
	switch verb {
	case tnDO:
		switch what {
		case optTerminalType, optEndOfRecord, optTransmitBinary, optNewEnviron:
			reply[1] = tnWILL
		default:
			reply[1] = tnWONT
		}
	case tnWILL:
		switch what {
		case optTerminalType, optEndOfRecord, optTransmitBinary, optNewEnviron:
			reply[1] = tnDO
		default:
			reply[1] = tnDONT
		}
	case tnDONT, tnWONT:
		// Don't reply to DONT/WONT. (The C's do_verb technically falls through
		// to an unconditional 3-byte send with an uninitialized verb byte — a
		// latent bug there; not replying is the correct, safe behavior.)
		return
	}
	_ = s.writeRaw(reply)
}

// doSB answers a sub-negotiation (telnet_stream_sb): TERMINAL-TYPE SEND and
// NEW-ENVIRON SEND.
func (s *stream) doSB(sb []byte) {
	if len(sb) <= 0 {
		return
	}
	switch sb[0] {
	case optTerminalType:
		if len(sb) < 2 || sb[1] != sbSEND {
			return
		}
		out := []byte{tnIAC, tnSB, optTerminalType, sbIS}
		out = append(out, []byte(s.termType)...)
		out = append(out, tnIAC, tnSE)
		_ = s.writeRaw(out)

	case optNewEnviron:
		// The host's SEND request may list specific variables; like the C we
		// ignore the request body and reply with everything we have.
		out := []byte{tnIAC, tnSB, optNewEnviron, sbIS}
		for _, e := range s.env {
			if e.userVar {
				out = append(out, envUSERVAR)
			} else {
				out = append(out, envVAR)
			}
			out = append(out, []byte(e.name)...)
			out = append(out, envVALUE)
			out = append(out, []byte(e.value)...)
		}
		out = append(out, tnIAC, tnSE)
		_ = s.writeRaw(out)
	}
}

// ── record framing ──────────────────────────────────────────────────────

// readRecord reads bytes through the IAC machine until an IAC EOR, returning the
// complete GDS record (header + 5250 data). Empty EORs are skipped, matching
// telnet_stream_handle_receive.
func (s *stream) readRecord() ([]byte, error) {
	var rec []byte
	for {
		c, err := s.getByte()
		if err != nil {
			return nil, err
		}
		if c == eorMarker {
			if len(rec) > 0 {
				return rec, nil
			}
			continue
		}
		rec = append(rec, byte(c))
	}
}

// sendPacket frames a 5250 packet and writes it, prepending the 10-byte GDS
// header, escaping IACs, and terminating with IAC EOR (telnet_stream_send_packet).
func (s *stream) sendPacket(flowtype int, flags, opcode byte, data []byte) error {
	length := len(data) + 10
	out := make([]byte, 0, length+4)
	out = append(out,
		byte(length>>8), byte(length&0xff), // total length
		0x12, 0xa0, // record type = GDS
		byte(flowtype>>8), byte(flowtype&0xff),
		4,      // variable-header length
		flags,  // flags
		0,      // reserved
		opcode, // opcode
	)
	out = append(out, data...)

	// Escape naturally-occurring IAC (0xFF) bytes.
	escaped := make([]byte, 0, len(out)+8)
	for _, b := range out {
		escaped = append(escaped, b)
		if b == tnIAC {
			escaped = append(escaped, tnIAC)
		}
	}
	escaped = append(escaped, tnIAC, tnEOR)
	return s.writeRaw(escaped)
}

func (s *stream) close() error { return s.conn.Close() }

// ── record byte cursor (record.c) ───────────────────────────────────────

// record wraps one received GDS record with a read cursor. The cursor starts at
// offset 6+data[6] (== 10) so the first getByte() yields the first byte of the
// 5250 command stream, exactly like tn5250_stream_get_record.
type record struct {
	data   []byte
	curPos int
}

func newRecord(data []byte) *record {
	offset := 10
	if len(data) > 6 {
		offset = 6 + int(data[6])
	}
	return &record{data: data, curPos: offset}
}

func (r *record) getByte() byte {
	// Degrade gracefully on a malformed/truncated record rather than panicking:
	// the C uses TN5250_ASSERT (record.c), which is compiled out under NDEBUG so
	// production C reads past the end instead of crashing. We return 0 and pin
	// the cursor at the end so the order-processing loop sees chain-end and
	// stops — a bad record can't take down the read-loop goroutine (and with it
	// the whole server).
	if r.curPos >= len(r.data) {
		r.curPos = len(r.data)
		return 0
	}
	b := r.data[r.curPos]
	r.curPos++
	return b
}

func (r *record) ungetByte() {
	if r.curPos > 0 {
		r.curPos--
	}
}
func (r *record) isChainEnd() bool { return r.curPos >= len(r.data) }
func (r *record) skipToEnd()       { r.curPos = len(r.data) }
func (r *record) opcode() byte {
	if len(r.data) > 9 {
		return r.data[9]
	}
	return 0
}
func (r *record) flags() byte {
	if len(r.data) > 7 {
		return r.data[7]
	}
	return 0
}
func (r *record) attention() bool  { return r.flags()&recHATN != 0 }
func (r *record) sysRequest() bool { return r.flags()&recHSRQ != 0 }
