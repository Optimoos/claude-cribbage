package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"

	"cribbage/internal/auth"
	"cribbage/internal/botclient"
	"cribbage/internal/client"
	"cribbage/internal/expert"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// instance is one server process: a database, a server and a listener.
type instance struct {
	db  *store.SQLite
	srv *Server
	ln  net.Listener
}

func startPersistent(t *testing.T, path, addr string) *instance {
	t.Helper()
	db, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Auth:       &auth.Service{Store: db, BcryptCost: bcrypt.MinCost},
		Games:      db,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		GameLimits: loadLimits,
	})
	if _, err := srv.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The previous instance's port may take a moment to free up.
	var ln net.Listener
	for i := 0; ; i++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	go srv.ServeTCP(ln)
	return &instance{db: db, srv: srv, ln: ln}
}

// stop simulates the process exiting: no more connections, all clients
// dropped, actors stopped, database closed.
func (in *instance) stop(t *testing.T) {
	in.ln.Close()
	in.srv.Shutdown()
	if err := in.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReproducesState(t *testing.T) { testRestore(t, "heuristic") }

func TestRestoreHeuristic2Game(t *testing.T) { testRestore(t, "heuristic2") }

// A game against the expert bot resumes after a restart: the bot is
// recreated by name, from the same bundle.
func TestRestoreExpertGame(t *testing.T) {
	registerTestExpert(t)
	testRestore(t, expert.BotName)
}

func testRestore(t *testing.T, bot string) {
	path := filepath.Join(t.TempDir(), "s.db")
	in := startPersistent(t, path, "127.0.0.1:0")
	addr := in.ln.Addr().String()

	a := login(t, addr, "alice")
	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: bot}}})
	u := a.next(t, replyTo(id)).GetGameUpdate()
	gameID := u.Game.GameId
	// Play into the pegging phase.
	for i := 0; i < 4 && len(u.Legal) > 0; i++ {
		id = a.send(t, act(gameID, u.Legal[0]))
		u = a.next(t, replyTo(id)).GetGameUpdate()
	}
	if u.Observation.Phase != pb.Phase_PHASE_PEGGING {
		t.Fatalf("expected pegging, got %v", u.Observation.Phase)
	}
	a.Close()
	in.stop(t)

	in = startPersistent(t, path, addr)
	defer in.stop(t)
	c := connect(t, addr)
	if _, err := c.Resume(a.auth.Token); err != nil {
		t.Fatalf("session did not survive restart: %v", err)
	}
	c.startReading()
	s := c.next(t, isUpdate(gameID)).GetGameUpdate()
	if s.Seq != u.Seq || !proto.Equal(s.Observation, u.Observation) || len(s.Legal) != len(u.Legal) {
		t.Fatalf("restored state differs:\n got seq %d %v\nwant seq %d %v", s.Seq, s.Observation, u.Seq, u.Observation)
	}
	for i := range s.Legal {
		if !proto.Equal(s.Legal[i], u.Legal[i]) {
			t.Fatalf("legal actions differ")
		}
	}
	// The game carries on.
	id = c.send(t, act(gameID, s.Legal[0]))
	if n := c.next(t, replyTo(id)).GetGameUpdate(); n == nil || n.Seq <= s.Seq {
		t.Fatal("could not act after restore")
	}
}

func TestOpenGameSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.db")
	in := startPersistent(t, path, "127.0.0.1:0")
	addr := in.ln.Addr().String()
	a := login(t, addr, "alice")
	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{}}})
	gameID := a.next(t, replyTo(id)).GetGameUpdate().Game.GameId
	in.stop(t)

	in = startPersistent(t, path, addr)
	defer in.stop(t)
	b := login(t, addr, "bob")
	id = b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})
	if u := b.next(t, replyTo(id)).GetGameUpdate(); u == nil || u.Game.Status != pb.GameStatus_GAME_STATUS_ACTIVE {
		t.Fatalf("join after restart: %v", u)
	}
}

func TestPresenceNotifications(t *testing.T) {
	_, addr := startServer(t)
	a, b := login(t, addr, "alice"), login(t, addr, "bob")
	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{}}})
	gameID := a.next(t, replyTo(id)).GetGameUpdate().Game.GameId
	id = b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})
	b.next(t, replyTo(id))

	b.Close()
	u := a.next(t, func(m *pb.ServerMessage) bool {
		u := m.GetGameUpdate()
		return u != nil && !u.Game.Players[1].Connected
	}).GetGameUpdate()
	if len(u.Events) != 0 || !u.Game.Players[0].Connected {
		t.Fatalf("presence update: %v", u)
	}

	c := connect(t, addr)
	if _, err := c.Resume(b.auth.Token); err != nil {
		t.Fatal(err)
	}
	a.next(t, func(m *pb.ServerMessage) bool {
		u := m.GetGameUpdate()
		return u != nil && u.Game.Players[1].Connected
	})
}

// TestBotclientsSurviveServerRestart restarts the server twice while three
// bot clients play: two matched against each other and one against server
// bots. Every game must complete with no errors.
func TestBotclientsSurviveServerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	in := startPersistent(t, path, "127.0.0.1:0")
	addr := in.ln.Addr().String()

	const games = 400
	configs := []botclient.Config{
		{Username: "alice", Concurrent: 50},
		{Username: "bob", Concurrent: 50, Seed: 1},
		{Username: "carol", Concurrent: 50, Opponent: "heuristic"},
	}
	var wg sync.WaitGroup
	results := make([]botclient.Stats, len(configs))
	errs := make([]error, len(configs))
	for i, cfg := range configs {
		cfg.Addr, cfg.Password, cfg.Bot, cfg.Games = addr, "password123", "heuristic", games
		cfg.Logger = slog.New(slog.NewTextHandler(testWriter{t}, nil))
		wg.Add(1)
		go func(i int, cfg botclient.Config) {
			defer wg.Done()
			results[i], errs[i] = botclient.Run(context.Background(), cfg)
		}(i, cfg)
	}

	for restart := 1; restart <= 2; restart++ {
		// Each instance's counters start at zero.
		deadline := time.Now().Add(30 * time.Second)
		for in.srv.Stats().GamesFinished < 150 {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: only %d games finished on this instance", restart, in.srv.Stats().GamesFinished)
			}
			time.Sleep(10 * time.Millisecond)
		}
		active := in.srv.Stats().ActiveGames
		in.stop(t)
		time.Sleep(100 * time.Millisecond) // clients notice and start retrying
		in = startPersistent(t, path, addr)
		// A game created during shutdown may be saved but never started, so
		// more games can be restored than were running.
		if got := in.srv.Stats().ActiveGames; got == 0 {
			t.Fatalf("restart %d: restored no games, %d were active", restart, active)
		}
		t.Logf("restart %d: %d games active at shutdown, %d restored", restart, active, in.srv.Stats().ActiveGames)
	}
	wg.Wait()
	defer in.stop(t)

	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("%s: %v", configs[i].Username, errs[i])
		}
		t.Logf("%s: %+v", configs[i].Username, r)
		if r.Finished < games || r.Errors != 0 || r.Lost != 0 || r.Reconnects != 2 {
			t.Errorf("%s: %+v", configs[i].Username, r)
		}
	}
	if results[0].Won+results[1].Won != results[0].Finished {
		t.Errorf("alice and bob wins (%d+%d) don't match games (%d)", results[0].Won, results[1].Won, results[0].Finished)
	}
	left, err := in.db.LoadUnfinished(context.Background())
	if err != nil || len(left) != 0 {
		t.Errorf("%d games left unfinished (err %v)", len(left), err)
	}
}

func TestBadCredentialsDoNotRetry(t *testing.T) {
	_, addr := startServer(t)
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	start := time.Now()
	_, err = botclient.Run(context.Background(), botclient.Config{
		Addr: addr, Username: "x", Password: "password123", Bot: "heuristic", Games: 1,
	})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("invalid username should fail fast, got %v after %v", err, time.Since(start))
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
