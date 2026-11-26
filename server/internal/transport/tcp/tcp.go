// Package tcp is a low-overhead transport for headless clients: each
// message is a 4-byte big-endian length followed by a binary protobuf.
package tcp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"google.golang.org/protobuf/proto"
)

// MaxFrame bounds the size of a single message.
const MaxFrame = 1 << 20

// Conn reads and writes length-prefixed protobuf messages. Read and Write
// may be used concurrently with each other, but not with themselves.
type Conn struct {
	nc   net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	rbuf []byte
	wbuf []byte
}

// NewConn wraps a network connection.
func NewConn(nc net.Conn) *Conn {
	if tc, ok := nc.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &Conn{nc: nc, r: bufio.NewReaderSize(nc, 64<<10), w: bufio.NewWriterSize(nc, 64<<10)}
}

// Dial connects to a server.
func Dial(addr string) (*Conn, error) {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return NewConn(nc), nil
}

// Read decodes the next message into m.
func (c *Conn) Read(m proto.Message) error {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	if cap(c.rbuf) < int(n) {
		// Grow as the payload arrives, not to what the header claims, so
		// a peer cannot make us allocate MaxFrame by sending 4 bytes.
		b := bytes.NewBuffer(c.rbuf[:0])
		if _, err := io.CopyN(b, c.r, int64(n)); err != nil {
			return err
		}
		c.rbuf = b.Bytes()
		return proto.Unmarshal(c.rbuf, m)
	}
	buf := c.rbuf[:n]
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	return proto.Unmarshal(buf, m)
}

// Write buffers m; call Flush to send.
func (c *Conn) Write(m proto.Message) error {
	var err error
	c.wbuf, err = proto.MarshalOptions{}.MarshalAppend(c.wbuf[:0], m)
	if err != nil {
		return err
	}
	if len(c.wbuf) > MaxFrame {
		return fmt.Errorf("frame too large: %d bytes", len(c.wbuf))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(c.wbuf)))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = c.w.Write(c.wbuf)
	return err
}

// Flush sends buffered messages.
func (c *Conn) Flush() error { return c.w.Flush() }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.nc.Close() }

func (c *Conn) String() string { return "tcp:" + c.nc.RemoteAddr().String() }

// RemoteIP is the client's IP address.
func (c *Conn) RemoteIP() string {
	host, _, err := net.SplitHostPort(c.nc.RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}
