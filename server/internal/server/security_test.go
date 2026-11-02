package server

import (
	"sync"
	"testing"
	"time"

	"cribbage/internal/protocol/pb"
)

// Checking a logged-in player's password costs a bcrypt hash whether it is
// right or wrong, so checks are limited either way.
func TestPasswordChecksLimited(t *testing.T) {
	addr := startRemote(t, "203.0.113.30", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 10, FailureEvery: time.Hour,
	})
	a := login(t, addr, "alice")
	for i := 0; i < 5; i++ {
		if m := a.next(t, replyTo(a.send(t, setEmail("", "password123")))); m.GetAccount() == nil {
			t.Fatalf("check %d: %v", i, m)
		}
	}
	wantError(t, a.next(t, replyTo(a.send(t, setEmail("", "password123")))), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	wantError(t, a.next(t, replyTo(a.send(t, changePassword("password123", "new-password")))), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	b := login(t, addr, "bob") // another player has their own
	if m := b.next(t, replyTo(b.send(t, setEmail("", "password123")))); m.GetAccount() == nil {
		t.Fatalf("another player: %v", m)
	}
}

// Wrong passwords sent all at once are counted before any of them is
// checked, so a burst gets no more guesses than the limit.
func TestFailureBurst(t *testing.T) {
	addr := startRemote(t, "203.0.113.31", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 3, FailureEvery: time.Hour,
		Hashing: 1,
	})
	reg := anon(t, addr)
	reg.next(t, replyTo(reg.send(t, authMsg(true, "victim", "password123"))))
	const n = 12
	cs := make([]*tclient, n)
	for i := range cs {
		cs[i] = connect(t, addr)
		cs[i].startReading()
	}
	codes := make([]pb.ErrorCode, n)
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.next(t, replyTo(c.send(t, authMsg(false, "victim", "wrongpassword")))).GetError().GetCode()
		}()
	}
	wg.Wait()
	checked := 0
	for _, code := range codes {
		if code == pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS {
			checked++
		} else if code != pb.ErrorCode_ERROR_CODE_RATE_LIMITED {
			t.Fatalf("unexpected %v", code)
		}
	}
	if checked != 3 {
		t.Fatalf("%d of %d guesses checked, want 3", checked, n)
	}
}

// A connection that never logs in may send only so many messages.
func TestPreLoginMessagesLimited(t *testing.T) {
	addr := startRemote(t, "203.0.113.32", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 10, FailureEvery: time.Hour,
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
