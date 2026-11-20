package tn5250

import (
	"io"
	"sync/atomic"
)

// countingConn counts the bytes that pass through a host connection, for
// the TX/RX display. It sits below the telnet layer, so negotiation and
// all 5250 records are included.
type countingConn struct {
	io.ReadWriteCloser
	tx, rx *atomic.Uint64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.rx.Add(uint64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.tx.Add(uint64(n))
	return n, err
}

// Traffic returns the bytes sent to (tx) and received from (rx) the host.
func (c *Client) Traffic() (tx, rx uint64) { return c.tx.Load(), c.rx.Load() }
