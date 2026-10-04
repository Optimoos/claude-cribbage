// Package rtc carries protocol messages over a WebRTC data channel, with a
// WebSocket used only to exchange the SDP offer/answer and ICE candidates.
//
// Text data-channel messages are protojson (convenient in a browser);
// binary messages are protobuf (compact, for Godot and Go clients).
package rtc

import (
	"errors"
	"fmt"
	"io"
	"sync"

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
const maxBuffered = 8 << 20

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
}

func newConn(pc *webrtc.PeerConnection, dc *webrtc.DataChannel, codec Codec, release func()) *Conn {
	c := &Conn{pc: pc, dc: dc, in: make(chan inbound, 256), closed: make(chan struct{}), codec: codec, release: release}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
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

// Read decodes the next message into m.
func (c *Conn) Read(m proto.Message) error {
	select {
	case msg := <-c.in:
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
	case <-c.closed:
		return io.EOF
	}
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
		return c.dc.SendText(string(b))
	}
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return c.dc.Send(b)
}

// Flush is a no-op: Write sends immediately.
func (c *Conn) Flush() error { return nil }

// Close tears down the peer connection.
func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		// Closing may block briefly, so never do it from a pion callback.
		go func() {
			c.pc.Close()
			if c.release != nil {
				c.release()
			}
		}()
	})
	return nil
}

func (c *Conn) String() string { return fmt.Sprintf("webrtc:%s", c.dc.Label()) }

// Done is closed when the connection closes.
func (c *Conn) Done() <-chan struct{} { return c.closed }
