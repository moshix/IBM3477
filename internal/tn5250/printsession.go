package tn5250

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// PrintClient is a TN5250E printer session (RFC 2877; ported from
// tn5250's lib5250/printsession.c and lp5250d): the host sees a printer
// device and sends it spooled output as SCS print data.
//
//   - Negotiation: terminal type IBM-3812-1; NEW-ENVIRON DEVNAME (the
//     printer device on the host), IBMFONT 12 and IBMTRANSFORM 0 (no host
//     print transform: the data stays SCS) and CODEPAGE.
//   - The first record is the startup response; its 4-character code
//     (I901, I902, I906 = started; 8902 = device not available, ...)
//     says whether the session started.
//   - Every later record is answered with Print Complete (flow 0x12,
//     opcode 1). A Clear record (opcode 2) drops what was received; a
//     17-byte record ends the job; other records carry SCS data after
//     the record header.
type PrintClient struct {
	device   string
	codepage string

	mu         sync.Mutex
	conn       io.ReadWriteCloser
	st         *stream
	job        []byte
	hungUp     bool               // Disconnect was called
	cancelDial context.CancelFunc // aborts a dial in progress

	// OnJob receives each finished job's SCS data (from Run's goroutine).
	OnJob func(scs []byte)
	// OnStarted receives the startup response: code and its meaning.
	OnStarted func(code, text string)
}

// Record opcodes of a printer session.
const (
	opPrintComplete = 1
	opPrintClear    = 2
	flowClientOut   = 0x12
	jobEndLength    = 0x11 // a record of this length ends the job
	maxPrintJobSCS  = 8 << 20
)

// ErrPrinterRefused ends a printer session the host did not start.
var ErrPrinterRefused = errors.New("printer session refused")

// NewPrintClient returns a printer session for host device name device
// (required: the host binds the session to that printer device).
func NewPrintClient(device, codepage string) *PrintClient {
	return &PrintClient{device: device, codepage: codepage}
}

// Connect dials host:port (optionally over TLS) for the printer session.
// A Disconnect before or during the dial aborts it: Connect then returns
// ErrHungUp and Run has nothing to read (without this, a session hung up
// while dialling connected anyway and waited forever for print data).
func (p *PrintClient) Connect(host, port string, useTLS bool) error {
	p.mu.Lock()
	if p.hungUp {
		p.mu.Unlock()
		return ErrHungUp
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelDial = cancel
	p.mu.Unlock()
	defer cancel()

	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if useTLS {
		td := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true, // AS/400 hosts often use self-signed certs
		}}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	p.mu.Lock()
	p.cancelDial = nil
	hungUp := p.hungUp
	p.mu.Unlock()
	if err != nil {
		if hungUp {
			return ErrHungUp
		}
		return err
	}
	if !p.attach(conn) {
		return ErrHungUp
	}
	return nil
}

// attach binds a connection (also used by tests); after a Disconnect it
// closes the connection instead and returns false.
func (p *PrintClient) attach(conn io.ReadWriteCloser) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hungUp {
		_ = conn.Close()
		return false
	}
	p.conn = conn
	st := newStream(conn, "IBM-3812-1", p.codepage, p.device, "")
	// a printer: no keyboard type; the font and no host print transform
	env := st.env[:0]
	for _, e := range st.env {
		if e.name != "KBDTYPE" {
			env = append(env, e)
		}
	}
	st.env = append(env,
		envEntry{name: "IBMFONT", value: "12"},
		envEntry{name: "IBMTRANSFORM", value: "0"},
	)
	p.st = st
	return true
}

// Disconnect ends the session: it aborts a dial in progress, closes the
// connection (ending Run), and makes later Connect/attach calls fail.
func (p *PrintClient) Disconnect() {
	p.mu.Lock()
	p.hungUp = true
	conn, cancel := p.conn, p.cancelDial
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// startupCodes are the startup response codes (lib5250/printsession.c).
var startupCodes = map[string]struct {
	ok   bool
	text string
}{
	"I901": {true, "Virtual device has less function than source device"},
	"I902": {true, "Session successfully started"},
	"I906": {true, "Automatic sign-on requested, but not allowed"},
	"2702": {false, "Device description not found"},
	"2703": {false, "Controller description not found"},
	"2777": {false, "Damaged device description"},
	"8901": {false, "Device not varied on"},
	"8902": {false, "Device not available"},
	"8903": {false, "Device not valid for session"},
	"8906": {false, "Session initiation failed"},
	"8907": {false, "Session failure"},
	"8910": {false, "Controller not valid for session"},
	"8916": {false, "No matching device found"},
	"8917": {false, "Not authorized to object"},
	"8918": {false, "Job cancelled"},
	"8920": {false, "Object partially damaged"},
	"8921": {false, "Communications error"},
	"8922": {false, "Negative response received"},
	"8923": {false, "Start-up record built incorrectly"},
	"8925": {false, "Creation of device failed"},
	"8928": {false, "Change of device failed"},
	"8929": {false, "Vary on or vary off failed"},
	"8930": {false, "Message queue does not exist"},
	"8934": {false, "Start up for S/36 WSF received"},
	"8935": {false, "Session rejected"},
	"8936": {false, "Security failure on session attempt"},
	"8937": {false, "Automatic sign-on rejected"},
	"8940": {false, "Automatic configuration failed or not allowed"},
	"I904": {false, "Source system at incompatible release"},
}

// recordData returns the bytes after a record's header: 2-byte length,
// record type 12A0, 2 reserved bytes, then a variable header whose
// length is byte 6.
func recordData(rec []byte) []byte {
	if len(rec) < 7 || 6+int(rec[6]) > len(rec) {
		return nil
	}
	return rec[6+int(rec[6]):]
}

// startupCode reads the 4-character response code of the startup
// response record (EBCDIC digits and letters, at data offset 5).
func startupCode(rec []byte) string {
	d := recordData(rec)
	if len(d) < 9 {
		return ""
	}
	code := make([]rune, 4)
	for i := range code {
		code[i] = EBCDICToRune("37", d[5+i])
	}
	return string(code)
}

// Run reads the host's records until the connection ends: first the
// startup response, then print data, acknowledging each record.
func (p *PrintClient) Run() error {
	p.mu.Lock()
	st := p.st
	p.mu.Unlock()
	if st == nil {
		return errors.New("tn5250: printer session not connected")
	}
	rec, err := st.readRecord()
	if err != nil {
		return err
	}
	code := startupCode(rec)
	info, known := startupCodes[code]
	if p.OnStarted != nil {
		p.OnStarted(code, info.text)
	}
	if !known || !info.ok {
		p.Disconnect()
		return fmt.Errorf("%w: %s %s", ErrPrinterRefused, code, info.text)
	}
	for {
		rec, err := st.readRecord()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !is5250Record(rec) {
			p.Disconnect()
			return ErrNot5250Host
		}
		if len(rec) >= 10 && rec[9] == opPrintClear {
			p.job = nil // the host clears the print buffers
			continue
		}
		if err := st.sendPacket(flowClientOut, 0, opPrintComplete, nil); err != nil {
			return err
		}
		if len(rec) == jobEndLength {
			if len(p.job) > 0 && p.OnJob != nil {
				p.OnJob(p.job)
			}
			p.job = nil
			continue
		}
		if len(p.job) < maxPrintJobSCS {
			p.job = append(p.job, recordData(rec)...)
		}
	}
}
