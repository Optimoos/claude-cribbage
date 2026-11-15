package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
	"cribbage/internal/mail"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
	"cribbage/internal/transport/tcp"
)

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
		Auth:       &auth.Service{Store: store.NewMemory(), BotKey: testBotKey, Mailer: discardMail{}},
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

type discardMail struct{}

func (discardMail) Send(context.Context, mail.Message) error { return nil }

func botLogin(user string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_BotLogin{BotLogin: &pb.BotLogin{Username: user, Key: testBotKey}}}
}

func TestAuthRateLimits(t *testing.T) {
	addr := startRemote(t, "203.0.113.5", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 2, RegisterEvery: time.Hour,
		CodesPerIP: 2, CodeEvery: time.Hour,
	})
	c := anon(t, addr)
	// Registrations: two, then refused.
	for _, u := range []string{"reg1", "reg2"} {
		if m := c.call(t, msg(&pb.Register{Username: u, Email: u + "@example.com"})); m.GetSignInStarted() == nil {
			t.Fatalf("register %s: %v", u, m)
		}
	}
	wantError(t, c.call(t, msg(&pb.Register{Username: "reg3", Email: "reg3@example.com"})), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	// Sign-in codes: two, then refused.
	for i := 0; i < 2; i++ {
		if m := c.call(t, msg(&pb.RequestSignIn{Name: "someone"})); m.GetSignInStarted() == nil {
			t.Fatalf("sign in %d: %v", i, m)
		}
	}
	wantError(t, c.call(t, msg(&pb.RequestSignIn{Name: "someone"})), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)

	// Attempts per address.
	addr = startRemote(t, "203.0.113.6", AuthLimits{
		AttemptsPerIP: 2, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		CodesPerIP: 10, CodeEvery: time.Hour,
	})
	c = anon(t, addr)
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: "x", Code: "1"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, c.call(t, msg(&pb.UseSignInLink{Token: "x"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: "x", Code: "1"})), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
}

// Loopback clients (local bots, tests) are not limited.
func TestAuthLimitsSkipLoopback(t *testing.T) {
	addr := startRemote(t, "127.0.0.1", AuthLimits{
		AttemptsPerIP: 1, AttemptEvery: time.Hour,
		RegistersPerIP: 1, RegisterEvery: time.Hour,
		CodesPerIP: 1, CodeEvery: time.Hour,
	})
	c := anon(t, addr)
	for i := 0; i < 5; i++ {
		wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: "x", Code: "1"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
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

// A connection that has started signing in (it is waiting for an emailed
// code, say) gets longer than one that has done nothing.
func TestSignInWaitsLonger(t *testing.T) {
	addr := startRemote(t, "203.0.113.8", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		CodesPerIP: 10, CodeEvery: time.Hour,
		ConnsPerAddr: 10, Conns: 100, LoginWithin: 200 * time.Millisecond, SignInWithin: time.Second,
	})
	idle, waiting := anon(t, addr), anon(t, addr)
	waiting.call(t, msg(&pb.RequestSignIn{Name: "alice"}))
	start := time.Now()
	closed(t, idle)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("idle connection lasted %v", d)
	}
	time.Sleep(300 * time.Millisecond) // well past LoginWithin
	if m := waiting.call(t, &pb.ClientMessage{Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{}}}); m.GetPong() == nil {
		t.Fatalf("waiting connection: %v", m)
	}
	closed(t, waiting) // at SignInWithin
}

func TestConnectionLimits(t *testing.T) {
	addr := startRemote(t, "203.0.113.9", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
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
	if a.next(t, replyTo(a.send(t, botLogin("prompt")))).GetAuthOk() == nil {
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

// Requests after login are limited per player, across connections.
func TestActionRateLimit(t *testing.T) {
	addr := startRemote(t, "203.0.113.20", AuthLimits{
		AttemptsPerIP: 100, AttemptEvery: time.Hour,
		RegistersPerIP: 10, RegisterEvery: time.Hour,
		ActionsPerUser: 3, ActionEvery: time.Hour,
	})
	list := &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}}
	a := login(t, addr, "alice")
	for i := 0; i < 2; i++ {
		if a.next(t, replyTo(a.send(t, list))).GetGameList() == nil {
			t.Fatalf("request %d refused", i)
		}
	}
	b := login(t, addr, "alice") // a second connection shares the budget
	if b.next(t, replyTo(b.send(t, list))).GetGameList() == nil {
		t.Fatal("third request refused")
	}
	wantError(t, a.next(t, replyTo(a.send(t, list))), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	// Pings are not counted, and other players have their own budget.
	if a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{}}}))).GetPong() == nil {
		t.Fatal("ping refused")
	}
	c := login(t, addr, "carol")
	if c.next(t, replyTo(c.send(t, list))).GetGameList() == nil {
		t.Fatal("another player refused")
	}
}

// countingBot records how many bots are choosing at once.
type countingBot struct{ now, peak *atomic.Int32 }

func (countingBot) Name() string { return "counting" }
func (b countingBot) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	n := b.now.Add(1)
	for p := b.peak.Load(); n > p && !b.peak.CompareAndSwap(p, n); p = b.peak.Load() {
	}
	time.Sleep(time.Millisecond)
	b.now.Add(-1)
	return legal[0]
}

// Bot decisions across all games are capped at BotMoves at a time.
func TestBotMovesCapped(t *testing.T) {
	var now, peak atomic.Int32
	bots.Register("counting", func(uint64) bots.Bot { return countingBot{&now, &peak} })
	l := *loadLimits
	l.BotMoves = 2
	_, addr := startServerWith(t, &l)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := login(t, addr, fmt.Sprint("player", i))
			u := c.next(t, replyTo(c.send(t, createGame("counting")))).GetGameUpdate()
			for j := 0; j < 5 && u != nil && len(u.Legal) > 0; j++ {
				u = c.next(t, replyTo(c.send(t, act(u.Game.GameId, u.Legal[0])))).GetGameUpdate()
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 2 || p < 2 {
		t.Fatalf("peak concurrent bot moves %d, want 2", p)
	}
}
