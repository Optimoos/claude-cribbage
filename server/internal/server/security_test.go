package server

import (
	"testing"
	"time"

	"cribbage/internal/protocol/pb"
)

// A connection that never logs in may send only so many messages.
func TestPreLoginMessagesLimited(t *testing.T) {
	addr := startRemote(t, "203.0.113.32", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
	})
	c := anon(t, addr)
	ping := &pb.ClientMessage{Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{}}}
	for i := 0; i < maxPreLogin; i++ {
		if c.next(t, replyTo(c.send(t, ping))).GetPong() == nil {
			t.Fatalf("ping %d refused", i)
		}
	}
	c.Send(ping)
	closed(t, c)
	// Logging in lifts the limit.
	a := login(t, addr, "alice")
	for i := 0; i < maxPreLogin+5; i++ {
		if a.next(t, replyTo(a.send(t, ping))).GetPong() == nil {
			t.Fatalf("ping %d after login refused", i)
		}
	}
}
