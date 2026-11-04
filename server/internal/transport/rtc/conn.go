// Package rtc carries protocol messages over a WebRTC data channel, with a
// WebSocket used only to exchange the SDP offer/answer and ICE candidates.
//
// Text data-channel messages are protojson (convenient in a browser);
// binary messages are protobuf (compact, for Godot and Go clients).
package rtc

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Codec selects how messages are encoded on the data channel.
type Codec int

const (
	// Auto (server side) answers in whatever encoding the client sent first.
	Auto Codec = iota
	Binary
	JSON
)

// Label is the data channel label clients must use.
const Label = "cribbage"

// maxBuffered bounds unsent data per channel before writes fail; a client
// that stops reading is disconnected instead of growing memory forever.
const maxBuffered = 1 << 20

var (
	jsonOut = protojson.MarshalOptions{EmitUnpopulated: true}
	jsonIn  = protojson.UnmarshalOptions{DiscardUnknown: true}
)

type inbound struct {
	data []byte
	text bool
}

// Conn is a message connection over a data channel. Read and Write may be
// used concurrently with each other.
type Conn struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	in     chan inbound
	closed chan struct{}
	once   sync.Once
	// release, if set, is called once the peer connection has closed.
	release func()

	mu    sync.Mutex
	codec Codec
	// wrote: something was sent, so Close lingers for it.
	wrote atomic.Bool
}

func newConn(pc *webrtc.PeerConnection, dc *webrtc.DataChannel, codec Codec, release func()) *Conn {
	c := &Conn{pc: pc, dc: dc, in: make(chan inbound, 64), closed: make(chan struct{}), codec: codec, release: release}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if len(m.Data) > maxMessage {
			c.Close() // no protocol message is this big
			return
		}
		select {
		case c.in <- inbound{data: m.Data, text: m.IsString}:
		case <-c.closed:
		}
	})
	dc.OnClose(func() { c.Close() })
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			c.Close()
		}
	})
	return c
}

// Read decodes the next message into m. Messages that arrived before the
// connection closed are still returned.
func (c *Conn) Read(m proto.Message) error {
	var msg inbound
	select {
	case msg = <-c.in:
	case <-c.closed:
		select {
		case msg = <-c.in:
		default:
			return io.EOF
		}
	}
	c.mu.Lock()
	if c.codec == Auto {
		c.codec = Binary
		if msg.text {
			c.codec = JSON
		}
	}
	c.mu.Unlock()
	if msg.text {
		return jsonIn.Unmarshal(msg.data, m)
	}
	return proto.Unmarshal(msg.data, m)
}

// Write sends m immediately.
func (c *Conn) Write(m proto.Message) error {
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	default:
	}
	if c.dc.BufferedAmount() > maxBuffered {
		return errors.New("rtc: peer is not reading")
	}
	c.mu.Lock()
	codec := c.codec
	c.mu.Unlock()
	if codec == JSON {
		b, err := jsonOut.Marshal(m)
		if err != nil {
			return err
		}
		c.wrote.Store(true)
		return c.dc.SendText(string(b))
	}
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	c.wrote.Store(true)
	return c.dc.Send(b)
}

// Flush is a no-op: Write sends immediately.
func (c *Conn) Flush() error { return nil }

// lingerFor bounds how long Close waits for sent messages to be delivered,
// and lingerAfter is the extra moment it gives them once sent.
const (
	lingerFor   = 2 * time.Second
	lingerAfter = 300 * time.Millisecond
)

// Close tears down the peer connection, once what was written has been
// delivered (or lingerFor has passed), so that a last message such as an
// error before closing reaches the client.
func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		// Closing may block briefly, so never do it from a pion callback.
		go func() {
			deadline := time.Now().Add(lingerFor)
			for c.dc.BufferedAmount() > 0 && c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected &&
				time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			// An empty buffer means the data was handed to SCTP, not that
			// it reached the peer: give the last packets a moment to arrive
			// before tearing the association down.
			if c.wrote.Load() && c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
				time.Sleep(lingerAfter)
			}
			c.pc.Close()
			if c.release != nil {
				c.release()
			}
		}()
	})
	return nil
}

func (c *Conn) String() string { return "webrtc:" + c.RemoteIP() }

// RemoteIP is the client's address on the ICE candidate pair in use: where
// its packets actually come from, even when signaling went through a proxy.
func (c *Conn) RemoteIP() string {
	sctp := c.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return ""
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Remote == nil {
		return ""
	}
	return pair.Remote.Address
}

// Done is closed when the connection closes.
func (c *Conn) Done() <-chan struct{} { return c.closed }
