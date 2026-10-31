package server

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/auth"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
	"cribbage/internal/transport/tcp"
)

func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.take("a", now) {
			t.Fatalf("take %d refused within the burst", i)
		}
	}
	if l.take("a", now) || l.allow("a", now) {
		t.Fatal("allowed past the burst")
	}
	if !l.take("b", now) {
		t.Fatal("keys share a bucket")
	}
	if l.allow("a", now.Add(59*time.Second)) {
		t.Fatal("refilled early")
	}
	if !l.take("a", now.Add(time.Minute)) || l.take("a", now.Add(time.Minute)) {
		t.Fatal("did not refill exactly one token a minute")
	}
	l.sweep(now.Add(time.Hour))
	if len(l.buckets) != 0 {
		t.Fatalf("sweep kept %d refilled buckets", len(l.buckets))
	}
}

// remoteTransport makes a TCP connection look as if it came from ip.
type remoteTransport struct {
	*tcp.Conn
	ip string
}

func (r remoteTransport) RemoteIP() string { return r.ip }

// startRemote starts a server whose connections all appear to come from
// the given address.
func startRemote(t *testing.T, ip string, lim AuthLimits) string {
	t.Helper()
	var addr atomic.Value
	addr.Store(ip)
	return startRemoteFrom(t, &addr, lim)
}

// startRemoteFrom is startRemote with an address that may change between
// connections.
func startRemoteFrom(t *testing.T, ip *atomic.Value, lim AuthLimits) string {
	t.Helper()
	if lim.ConnsPerAddr == 0 {
		lim.ConnsPerAddr, lim.Conns, lim.LoginWithin = 100, 100, time.Minute
	}
	srv := New(Config{
		Auth:       &auth.Service{Store: store.NewMemory(), BcryptCost: bcrypt.MinCost},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AuthLimits: &lim,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.ServeConn(remoteTransport{tcp.NewConn(nc), ip.Load().(string)})
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func authMsg(register bool, user, pass string) *pb.ClientMessage {
	if register {
		return &pb.ClientMessage{Body: &pb.ClientMessage_Register{Register: &pb.Register{Username: user, Password: pass}}}
	}
	return &pb.ClientMessage{Body: &pb.ClientMessage_Login{Login: &pb.Login{Username: user, Password: pass}}}
}

func TestAuthRateLimits(t *testing.T) {
	lim := AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 2, RegisterEvery: time.Hour,
		FailuresPerUser: 3, FailureEvery: time.Hour,
	}
	addr := startRemote(t, "203.0.113.5", lim)
	c := connect(t, addr)
	c.startReading()
	try := func(register bool, user, pass string) *pb.ServerMessage {
		t.Helper()
		return c.next(t, replyTo(c.send(t, authMsg(register, user, pass))))
	}

	// Registrations: two, then refused.
	for _, u := range []string{"reg1", "reg2"} {
		if m := try(true, u, "password123"); m.GetAuthOk() == nil {
			t.Fatalf("register %s: %v", u, m)
		}
		c = connect(t, addr) // a fresh, unauthenticated connection
		c.startReading()
	}
	wantError(t, try(true, "reg3", "password123"), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)

	// Failed logins: three wrong passwords lock the username, even with
	// the right password, but not other usernames.
	for i := 0; i < 3; i++ {
		wantError(t, try(false, "REG1", "wrongpassword"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	}
	wantError(t, try(false, "reg1", "password123"), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	if m := try(false, "reg2", "password123"); m.GetAuthOk() == nil {
		t.Fatalf("other user locked out: %v", m)
	}

	// Attempts per address.
	addr = startRemote(t, "203.0.113.6", AuthLimits{
		AttemptsPerIP: 2, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 10, FailureEvery: time.Hour,
	})
	c = connect(t, addr)
	c.startReading()
	wantError(t, try(false, "nobody", "password123"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, try(false, "nobody2", "password123"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, try(false, "nobody3", "password123"), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
}

// Loopback clients (local bots, tests) are not limited.
func TestAuthLimitsSkipLoopback(t *testing.T) {
	addr := startRemote(t, "127.0.0.1", AuthLimits{
		AttemptsPerIP: 1, AttemptEvery: time.Hour,
		RegistersPerIP: 1, RegisterEvery: time.Hour,
		FailuresPerUser: 1, FailureEvery: time.Hour,
	})
	c := connect(t, addr)
	c.startReading()
	for i := 0; i < 5; i++ {
		wantError(t, c.next(t, replyTo(c.send(t, authMsg(false, "nobody", "password123")))),
			pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	}
}

func TestAddrKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":           "203.0.113.5",
		"::ffff:203.0.113.5":    "203.0.113.5",
		"2001:db8:1:2:3:4:5:6":  "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":  "2001:db8:1:2::/64",
		"2001:db8:1:3::1":       "2001:db8:1:3::/64",
		"":                      "unknown",
		"not-an-ip.example.com": "unknown",
	} {
		if got := addrKey(in); got != want {
			t.Errorf("addrKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Failed logins count per username and address: a wrong guess from one
// place does not lock the player out elsewhere. IPv6 counts by /64.
func TestFailuresPerAddress(t *testing.T) {
	var ip atomic.Value
	ip.Store("2001:db8::1")
	addr := startRemoteFrom(t, &ip, AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 2, FailureEvery: time.Hour,
	})
	try := func(user, pass string) *pb.ServerMessage {
		c := connect(t, addr)
		c.startReading()
		return c.next(t, replyTo(c.send(t, authMsg(false, user, pass))))
	}
	reg := connect(t, addr)
	reg.startReading()
	reg.next(t, replyTo(reg.send(t, authMsg(true, "victim", "password123"))))

	wantError(t, try("victim", "wrongpassword"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	ip.Store("2001:db8::2") // same /64
	wantError(t, try("victim", "wrongpassword"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, try("victim", "password123"), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	ip.Store("198.51.100.7") // the real player, elsewhere
	if m := try("victim", "password123"); m.GetAuthOk() == nil {
		t.Fatalf("player locked out by someone else's guesses: %v", m)
	}
	// Names that cannot exist are refused at once.
	wantError(t, try(strings.Repeat("x", 5000), "password123"), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
}

func TestConnectionLimits(t *testing.T) {
	addr := startRemote(t, "203.0.113.9", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		FailuresPerUser: 10, FailureEvery: time.Hour,
		ConnsPerAddr: 2, Conns: 100, LoginWithin: 200 * time.Millisecond,
	})
	closed := func(c *tclient) bool {
		select {
		case _, ok := <-c.msgs:
			return !ok
		case <-time.After(time.Second):
			return false
		}
	}
	a, b := connect(t, addr), connect(t, addr)
	a.startReading()
	b.startReading()
	time.Sleep(50 * time.Millisecond) // let the server count them
	c := connect(t, addr)
	c.startReading()
	if !closed(c) {
		t.Fatal("third connection from one address was accepted")
	}
	// a logs in; b never does and is cut off.
	if a.next(t, replyTo(a.send(t, authMsg(true, "prompt", "password123")))).GetAuthOk() == nil {
		t.Fatal("login failed")
	}
	if !closed(b) {
		t.Fatal("connection that never logged in was kept")
	}
	select {
	case _, ok := <-a.msgs:
		if !ok {
			t.Fatal("logged-in connection was closed")
		}
	case <-time.After(400 * time.Millisecond):
	}
}
