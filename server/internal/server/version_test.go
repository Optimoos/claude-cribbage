package server

import (
	"io"
	"testing"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/transport/tcp"
)

// A client built for another protocol version is told to update, and the
// connection is closed.
func TestProtocolVersion(t *testing.T) {
	_, addr := startServer(t)
	for _, v := range []uint32{0, protocolVersion + 1} {
		c, err := tcp.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		m := &pb.ClientMessage{RequestId: 3, ProtocolVersion: v, Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{}}}
		if err := c.Write(m); err != nil {
			t.Fatal(err)
		}
		c.Flush()
		var r pb.ServerMessage
		if err := c.Read(&r); err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
		if r.RequestId != 3 || r.GetError().GetCode() != pb.ErrorCode_ERROR_CODE_UPGRADE_REQUIRED {
			t.Fatalf("version %d: %v", v, &r)
		}
		if err := c.Read(&r); err != io.EOF {
			t.Fatalf("version %d: connection still open: %v", v, err)
		}
	}
	// The current version is served.
	c := anon(t, addr)
	if c.next(t, replyTo(c.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{}}}))).GetPong() == nil {
		t.Fatal("current version refused")
	}
}
