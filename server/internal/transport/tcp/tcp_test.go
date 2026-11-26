package tcp

import (
	"net"
	"testing"

	"cribbage/internal/protocol/pb"
)

// A header claiming a large frame does not allocate it before the bytes
// come; a frame larger than the buffer so far still reads whole.
func TestReadGrowsWithPayload(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		w := NewConn(b)
		big := &pb.ClientMessage{RequestId: 7, Body: &pb.ClientMessage_SendInvite{SendInvite: &pb.SendInvite{Username: string(make([]byte, 200<<10))}}}
		_ = w.Write(big)
		_ = w.Flush()
		_, _ = b.Write([]byte{0, 0x0f, 0xff, 0xff}) // claims ~1 MiB, sends nothing
		b.Close()
	}()
	r := NewConn(a)
	var m pb.ClientMessage
	if err := r.Read(&m); err != nil || m.RequestId != 7 {
		t.Fatalf("read: %v, %v", m.RequestId, err)
	}
	if err := r.Read(&m); err == nil {
		t.Fatal("read a frame that never came")
	}
	if cap(r.rbuf) >= MaxFrame {
		t.Fatalf("allocated %d bytes for a frame that never came", cap(r.rbuf))
	}
}
