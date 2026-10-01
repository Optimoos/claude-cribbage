// Package client is a Go client for the cribbage server, over TCP or WebRTC.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/transport/rtc"
	"cribbage/internal/transport/tcp"
)

// Transport carries messages; see server.Transport.
type Transport interface {
	Read(proto.Message) error
	Write(proto.Message) error
	Flush() error
	Close() error
}

// Client is a connection to the server. Send may be called from any
// goroutine; Recv must be called from a single goroutine.
type Client struct {
	conn   Transport
	nextID atomic.Uint32
	out    chan *pb.ClientMessage
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

// Dial connects using the address's scheme: "ws://host:port/rtc" (or wss)
// uses WebRTC with binary messages; anything else is a TCP host:port.
func Dial(addr string) (*Client, error) {
	if strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://") {
		return DialWebRTC(context.Background(), addr, rtc.Binary)
	}
	return DialTCP(addr)
}

// DialTCP connects over the TCP transport.
func DialTCP(addr string) (*Client, error) {
	conn, err := tcp.Dial(addr)
	if err != nil {
		return nil, err
	}
	return New(conn), nil
}

var (
	rtcOnce sync.Once
	rtcAPI  *rtc.API
	rtcErr  error
)

// DialWebRTC connects over a WebRTC data channel, signaling via url.
func DialWebRTC(ctx context.Context, url string, codec rtc.Codec) (*Client, error) {
	rtcOnce.Do(func() { rtcAPI, rtcErr = rtc.NewAPI(rtc.Options{}) })
	if rtcErr != nil {
		return nil, rtcErr
	}
	conn, err := rtcAPI.Dial(ctx, url, codec)
	if err != nil {
		return nil, err
	}
	return New(conn), nil
}

// New wraps a connected transport.
func New(t Transport) *Client {
	c := &Client{conn: t, out: make(chan *pb.ClientMessage, 4096), done: make(chan struct{})}
	go c.writeLoop()
	return c
}

func (c *Client) writeLoop() {
	for {
		select {
		case m := <-c.out:
			if err := c.conn.Write(m); err != nil {
				c.fail(err)
				return
			}
			if len(c.out) == 0 {
				if err := c.conn.Flush(); err != nil {
					c.fail(err)
					return
				}
			}
		case <-c.done:
			return
		}
	}
}

func (c *Client) fail(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
	c.Close()
}

// ErrClosed is returned by Send after the connection is closed.
var ErrClosed = errors.New("connection closed")

// Send queues m, assigning a request id if it has none, and returns the id.
func (c *Client) Send(m *pb.ClientMessage) (uint32, error) {
	if m.RequestId == 0 {
		m.RequestId = c.nextID.Add(1)
	}
	select {
	case c.out <- m:
		return m.RequestId, nil
	case <-c.done:
		return 0, ErrClosed
	}
}

// Recv returns the next message from the server.
func (c *Client) Recv() (*pb.ServerMessage, error) {
	m := new(pb.ServerMessage)
	if err := c.conn.Read(m); err != nil {
		c.errMu.Lock()
		defer c.errMu.Unlock()
		if c.err != nil {
			return nil, c.err
		}
		return nil, err
	}
	return m, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
	return nil
}

// ServerError is an Error message returned by the server.
type ServerError struct {
	Code    pb.ErrorCode
	Message string
	GameID  string
}

func (e ServerError) Error() string { return fmt.Sprintf("%v: %s", e.Code, e.Message) }

// call sends m and waits for the reply with the same request id. Only use
// before starting a separate receive loop.
func (c *Client) call(m *pb.ClientMessage) (*pb.ServerMessage, error) {
	id, err := c.Send(m)
	if err != nil {
		return nil, err
	}
	for {
		r, err := c.Recv()
		if err != nil {
			return nil, err
		}
		if r.RequestId != id {
			continue
		}
		if e := r.GetError(); e != nil {
			return nil, ServerError{Code: e.Code, Message: e.Message, GameID: e.GameId}
		}
		return r, nil
	}
}

// Login authenticates with a username and password. If register is true
// and the account does not exist, it is created.
func (c *Client) Login(username, password, nickname string, register bool) (*pb.AuthOk, error) {
	r, err := c.call(&pb.ClientMessage{Body: &pb.ClientMessage_Login{Login: &pb.Login{Username: username, Password: password}}})
	var se ServerError
	if register && errors.As(err, &se) && se.Code == pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS {
		r, err = c.call(&pb.ClientMessage{Body: &pb.ClientMessage_Register{Register: &pb.Register{
			Username: username, Password: password, Nickname: nickname,
		}}})
	}
	if err != nil {
		return nil, err
	}
	return r.GetAuthOk(), nil
}

// Resume authenticates with a session token.
func (c *Client) Resume(token string) (*pb.AuthOk, error) {
	r, err := c.call(&pb.ClientMessage{Body: &pb.ClientMessage_Resume{Resume: &pb.Resume{Token: token}}})
	if err != nil {
		return nil, err
	}
	return r.GetAuthOk(), nil
}
