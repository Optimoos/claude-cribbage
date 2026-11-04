package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

func createGame(bot string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: bot}}}
}

func TestGameLimits(t *testing.T) {
	l := DefaultGameLimits
	l.Games, l.BotGames, l.Open = 3, 2, 1
	_, addr := startServerWith(t, &l)
	a := login(t, addr, "alice")
	b := login(t, addr, "bob")

	for i := 0; i < 2; i++ {
		if a.next(t, replyTo(a.send(t, createGame("random")))).GetGameUpdate() == nil {
			t.Fatalf("bot game %d refused", i)
		}
	}
	wantError(t, a.next(t, replyTo(a.send(t, createGame("random")))), pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)

	open := a.next(t, replyTo(a.send(t, createGame("")))).GetGameUpdate()
	if open == nil {
		t.Fatal("open game refused")
	}
	// Three games: at the limit for anything, including joining.
	wantError(t, a.next(t, replyTo(a.send(t, createGame("")))), pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)
	bOpen := b.next(t, replyTo(b.send(t, createGame("")))).GetGameUpdate()
	m := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: bOpen.Game.GameId}}})))
	wantError(t, m, pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)
	wantError(t, a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_FindMatch{FindMatch: &pb.FindMatch{}}}))),
		pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)

	// Cancelling the open game frees a place.
	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resign{Resign: &pb.Resign{GameId: open.Game.GameId}}})
	a.next(t, replyTo(id))
	if a.next(t, replyTo(a.send(t, createGame("")))).GetGameUpdate() == nil {
		t.Fatal("open game refused after cancelling one")
	}
}

// waitAsleep waits for g's actor to go to sleep.
func waitAsleep(t *testing.T, g *game) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.amu.Lock()
		awake := g.awake
		g.amu.Unlock()
		if !awake {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("game did not go to sleep")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gameByID(s *Server, id string) *game {
	s.gmu.RLock()
	defer s.gmu.RUnlock()
	return s.games[id]
}

// A game that sleeps between moves wakes to exactly the state it left.
func TestGameSleepsAndWakes(t *testing.T) {
	l := *loadLimits
	l.SleepAfter = 20 * time.Millisecond
	srv, addr := startServerWith(t, &l)
	testSleepsAndWakes(t, srv, addr)
}

// The same with the SQLite store, which wakes games from its own copy of
// their actions.
func TestGameSleepsAndWakesSQLite(t *testing.T) {
	in := startPersistent(t, filepath.Join(t.TempDir(), "s.db"), "127.0.0.1:0")
	in.srv.limits.SleepAfter = 20 * time.Millisecond
	defer func() { in.stop(t) }()
	testSleepsAndWakes(t, in.srv, in.ln.Addr().String())
}

func testSleepsAndWakes(t *testing.T, srv *Server, addr string) {
	a := login(t, addr, "alice")
	u := a.next(t, replyTo(a.send(t, createGame("heuristic2")))).GetGameUpdate()
	gameID := u.Game.GameId
	g := gameByID(srv, gameID)

	for moves := 0; u.Game.Status == pb.GameStatus_GAME_STATUS_ACTIVE; moves++ {
		waitAsleep(t, g)
		if g.eng != nil || g.seats[1].bot != nil {
			t.Fatal("sleeping game kept its engine or bot")
		}
		// Waking for a sync gives the same view as before sleeping.
		s := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: gameID}}}))).GetGameUpdate()
		if !proto.Equal(s.Observation, u.Observation) || !proto.Equal(&pb.GameUpdate{Legal: s.Legal}, &pb.GameUpdate{Legal: u.Legal}) {
			t.Fatalf("move %d: state changed while asleep:\nbefore %v\nafter  %v", moves, u.Observation, s.Observation)
		}
		if moves%3 == 0 {
			waitAsleep(t, g) // and sometimes act on a sleeping game
		}
		u = a.next(t, replyTo(a.send(t, act(gameID, s.Legal[0])))).GetGameUpdate()
		if u == nil {
			t.Fatalf("move %d refused", moves)
		}
	}
}

func joinGame(id string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: id}}}
}

// finished waits for an update saying the game is over.
func finished(id string) func(*pb.ServerMessage) bool {
	return func(m *pb.ServerMessage) bool {
		u := m.GetGameUpdate()
		return u != nil && u.Game.GameId == id && u.Game.Status == pb.GameStatus_GAME_STATUS_FINISHED
	}
}

func TestGamesExpire(t *testing.T) {
	srv, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	b := login(t, addr, "bob")
	open := a.next(t, replyTo(a.send(t, createGame("")))).GetGameUpdate().Game.GameId
	human := b.next(t, replyTo(b.send(t, createGame("")))).GetGameUpdate().Game.GameId
	if a.next(t, replyTo(a.send(t, joinGame(human)))).GetGameUpdate() == nil {
		t.Fatal("join failed")
	}
	bot := a.next(t, replyTo(a.send(t, createGame("random")))).GetGameUpdate().Game.GameId
	now := time.Now()
	alive := func(ids ...string) {
		t.Helper()
		time.Sleep(50 * time.Millisecond) // let the actors run
		for _, id := range ids {
			if gameByID(srv, id) == nil {
				t.Fatalf("game %s ended early", id)
			}
		}
	}

	srv.expireGames(now.Add(47 * time.Hour))
	alive(open, human, bot)
	srv.expireGames(now.Add(49 * time.Hour))
	if u := a.next(t, finished(open)).GetGameUpdate(); u.Game.Winner != -1 {
		t.Fatalf("expired open game: %v", u.Game)
	}
	alive(human, bot)
	srv.expireGames(now.Add(31 * 24 * time.Hour))
	a.next(t, finished(bot))
	alive(human)
	srv.expireGames(now.Add(61 * 24 * time.Hour))
	for _, c := range []*tclient{a, b} {
		u := c.next(t, finished(human)).GetGameUpdate()
		if u.Game.Winner != -1 || len(u.Events) != 1 || u.Events[0].Kind != pb.EventKind_EVENT_KIND_EXPIRED {
			t.Fatalf("expired game update: %v", u)
		}
	}
}

// A move pushes expiry back.
func TestMoveDelaysExpiry(t *testing.T) {
	srv, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	u := a.next(t, replyTo(a.send(t, createGame("random")))).GetGameUpdate()
	g := gameByID(srv, u.Game.GameId)
	g.lastMove.Add(-int64(29 * 24 * time.Hour / time.Millisecond)) // last move 29 days ago
	a.next(t, replyTo(a.send(t, act(u.Game.GameId, u.Legal[0]))))
	srv.expireGames(time.Now().Add(2 * 24 * time.Hour))
	time.Sleep(50 * time.Millisecond)
	if gameByID(srv, u.Game.GameId) == nil {
		t.Fatal("game expired despite a recent move")
	}
}

type panicky struct{}

func (panicky) Name() string { return "panicky" }
func (panicky) Choose(*engine.Observation, []engine.Action) engine.Action {
	panic("bot bug")
}

// A bot that panics resigns instead of taking the server down.
func TestBotPanicResigns(t *testing.T) {
	bots.Register("panicky", func(uint64) bots.Bot { return panicky{} })
	_, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	u := a.next(t, replyTo(a.send(t, createGame("panicky")))).GetGameUpdate()
	g := u.Game
	if g.Status != pb.GameStatus_GAME_STATUS_FINISHED || !g.Resigned || g.Winner != g.Seat {
		t.Fatalf("game after bot panic: %v", g)
	}
}

// A command that panics is undone: the game reloads from the store.
func TestCommandPanicReloads(t *testing.T) {
	srv, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	u := a.next(t, replyTo(a.send(t, createGame("random")))).GetGameUpdate()
	id := u.Game.GameId
	g := gameByID(srv, id)
	g.submit(func() {
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.eng.Apply(1-int(u.Game.Seat), engine.Action{}) // whatever it does
		panic("half-done")
	})
	s := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: id}}}))).GetGameUpdate()
	if s == nil || s.Game.Status != pb.GameStatus_GAME_STATUS_ACTIVE || !proto.Equal(s.Observation, u.Observation) {
		t.Fatalf("after a panicking command: %v", s)
	}
	if a.next(t, replyTo(a.send(t, act(id, s.Legal[0])))).GetGameUpdate() == nil {
		t.Fatal("game not playable after reload")
	}
}

// failingActions is a store whose writes of actions fail while fail is
// set; header-only writes still succeed.
type failingActions struct {
	*store.Memory
	fail atomic.Bool
}

func (f *failingActions) CommitGame(ctx context.Context, id string, g *store.Game, acts []store.Action) error {
	if f.fail.Load() && len(acts) > 0 {
		return errors.New("disk on fire")
	}
	return f.Memory.CommitGame(ctx, id, g, acts)
}

// A game whose first deal cannot be saved is recorded as finished, so it
// does not come back after a restart.
func TestUnsavedStartIsCancelled(t *testing.T) {
	games := &failingActions{Memory: store.NewMemory()}
	srv := New(Config{
		Auth:       &auth.Service{Store: store.NewMemory(), BotKey: testBotKey},
		Games:      games,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		GameLimits: loadLimits,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTCP(ln)
	t.Cleanup(func() { ln.Close() })
	a := login(t, ln.Addr().String(), "alice")

	games.fail.Store(true)
	wantError(t, a.next(t, replyTo(a.send(t, createGame("random")))), pb.ErrorCode_ERROR_CODE_INTERNAL)
	games.fail.Store(false)
	left, err := games.LoadUnfinished(context.Background())
	if err != nil || len(left) != 0 {
		t.Fatalf("unfinished games after a failed start: %v, %v", left, err)
	}
}
