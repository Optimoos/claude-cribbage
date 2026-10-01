package server

import (
	"sync"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// Conn is one client connection. Its reader runs in ServeConn; a separate
// writer goroutine drains the outbound queue so that slow clients never
// block game actors.
type Conn struct {
	srv  *Server
	t    Transport
	out  chan *pb.ServerMessage
	done chan struct{}
	once sync.Once

	// user is set once by the reader goroutine during authentication, before
	// the connection is registered or used in any game command.
	user *store.User
}

func (c *Conn) send(m *pb.ServerMessage) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.out <- m:
	default:
		c.srv.log.Warn("outbound queue full, dropping connection", "conn", c.t.String())
		c.close()
	}
}

func (c *Conn) sendError(id uint32, code pb.ErrorCode, gameID, msg string) {
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Error{Error: &pb.Error{
		Code: code, Message: msg, GameId: gameID,
	}}})
}

func (c *Conn) writeLoop() {
	for {
		select {
		case m := <-c.out:
			if err := c.t.Write(m); err != nil {
				c.close()
				return
			}
			// Batch: only flush once the queue is drained.
			if len(c.out) == 0 {
				if err := c.t.Flush(); err != nil {
					c.close()
					return
				}
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.t.Close()
	})
}

func (c *Conn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
