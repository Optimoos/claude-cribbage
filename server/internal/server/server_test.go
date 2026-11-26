package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/ai/encode"
	"cribbage/internal/ai/nn"
	"cribbage/internal/auth"
	"cribbage/internal/botclient"
	"cribbage/internal/bots"
	"cribbage/internal/client"
	"cribbage/internal/expert"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// loadLimits lets the load tests play hundreds of games per user.
var loadLimits = func() *GameLimits {
	l := DefaultGameLimits
	l.Games, l.BotGames, l.Open = 1<<20, 1<<20, 1<<20
	return &l
}()

// testBotKey is the bot key of test servers; test players are bots.
const testBotKey = "test-bot-key-0123456789"

func startServer(t *testing.T) (*Server, string) {
	t.Helper()
	return startServerWith(t, loadLimits)
}

func startServerWith(t *testing.T, limits *GameLimits) (*Server, string) {
	t.Helper()
	// WebRTC test connections may come from a LAN address rather than
	// loopback, so they are rate limited: allow load tests' bursts.
	al := DefaultAuthLimits
	al.ActionsPerUser = 1 << 20
	srv := New(Config{
		Auth:       &auth.Service{Store: store.NewMemory(), BotKey: testBotKey},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		GameLimits: limits,
		AuthLimits: &al,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTCP(ln)
	t.Cleanup(func() { ln.Close() })
	return srv, ln.Addr().String()
}

// tclient is a logged-in client whose messages are delivered on a channel.
type tclient struct {
	*client.Client
	auth *pb.AuthOk
	msgs chan *pb.ServerMessage
}

func connect(t *testing.T, addr string) *tclient {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &tclient{Client: c, msgs: make(chan *pb.ServerMessage, 1024)}
}

func (c *tclient) startReading() {
	go func() {
		defer close(c.msgs)
		for {
			m, err := c.Recv()
			if err != nil {
				return
			}
			c.msgs <- m
		}
	}()
}

func login(t *testing.T, addr, user string) *tclient {
	t.Helper()
	c := connect(t, addr)
	a, err := c.BotLogin(user, "Nick "+user, testBotKey)
	if err != nil {
		t.Fatal(err)
	}
	c.auth = a
	c.startReading()
	return c
}

func (c *tclient) send(t *testing.T, m *pb.ClientMessage) uint32 {
	t.Helper()
	id, err := c.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// next returns the next message matching pred, discarding others.
func (c *tclient) next(t *testing.T, pred func(*pb.ServerMessage) bool) *pb.ServerMessage {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				t.Fatal("connection closed")
			}
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatal("timed out waiting for message")
		}
	}
}

func replyTo(id uint32) func(*pb.ServerMessage) bool {
	return func(m *pb.ServerMessage) bool { return m.RequestId == id }
}

func wantError(t *testing.T, m *pb.ServerMessage, code pb.ErrorCode) {
	t.Helper()
	if m.GetError().GetCode() != code {
		t.Fatalf("got %v, want error %v", m, code)
	}
}

func TestBotclientVsServerBots(t *testing.T) {
	_, addr := startServer(t)
	st, err := botclient.Run(context.Background(), botclient.Config{
		Addr: addr, Username: "alice", Key: testBotKey, Bot: "heuristic",
		Opponent: "random", Concurrent: 100, Games: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v (%.0f games/s)", st, float64(st.Finished)/st.Elapsed.Seconds())
	if st.Finished != 1000 || st.Errors != 0 || st.Resigned != 0 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if rate := float64(st.Won) / float64(st.Finished); rate < 0.9 {
		t.Fatalf("heuristic beat random server bot only %.2f of the time", rate)
	}
}

func TestMatchmakingBetweenClients(t *testing.T) {
	srv, addr := startServer(t)
	var wg sync.WaitGroup
	results := make([]botclient.Stats, 2)
	errs := make([]error, 2)
	for i, user := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(i int, user string) {
			defer wg.Done()
			results[i], errs[i] = botclient.Run(context.Background(), botclient.Config{
				Addr: addr, Username: user, Key: testBotKey, Bot: "heuristic",
				Concurrent: 100, Games: 1000, Seed: uint64(i),
			})
		}(i, user)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].Finished != 1000 || results[i].Errors != 0 {
			t.Fatalf("client %d: %+v", i, results[i])
		}
	}
	if results[0].Won+results[1].Won != 1000 {
		t.Fatalf("wins don't add up: %d + %d", results[0].Won, results[1].Won)
	}
	s := srv.Stats()
	t.Logf("alice %+v, bob %+v, server %+v", results[0], results[1], s)
	if s.GamesFinished != 1000 {
		t.Fatalf("server finished %d games", s.GamesFinished)
	}
}

func TestAuthErrors(t *testing.T) {
	_, addr := startServer(t) // no mail, no passkeys
	c := anon(t, addr)
	wantError(t, c.call(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "random"}}}),
		pb.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
	bot := func(user, key string) *pb.ClientMessage {
		return &pb.ClientMessage{Body: &pb.ClientMessage_BotLogin{BotLogin: &pb.BotLogin{Username: user, Key: key}}}
	}
	wantError(t, c.call(t, bot("x", testBotKey)), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, c.call(t, bot("carol", "wrong-key-wrong-key")), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, c.call(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resume{Resume: &pb.Resume{Token: "bogus"}}}),
		pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: "nope", Code: "12345678"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, c.call(t, msg(&pb.UseSignInLink{Token: "nope"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, c.call(t, msg(&pb.PasskeySignInBegin{})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, c.call(t, msg(&pb.RequestSignIn{Name: "carol"})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	if a := c.call(t, bot("Carol", testBotKey)).GetAuthOk(); a == nil || a.Nickname != "Carol" || a.Token == "" {
		t.Fatalf("bot login failed: %v", a)
	}
	wantError(t, c.call(t, bot("carol", testBotKey)), pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED)
}

func isUpdate(gameID string) func(*pb.ServerMessage) bool {
	return func(m *pb.ServerMessage) bool {
		u := m.GetGameUpdate()
		return u != nil && (gameID == "" || u.Game.GameId == gameID)
	}
}

func act(gameID string, a *pb.Action) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_Act{Act: &pb.Act{GameId: gameID, Action: a}}}
}

// visibleCards returns every card in an observation.
func visibleCards(o *pb.Observation) map[uint32]bool {
	out := map[uint32]bool{}
	for _, l := range [][]uint32{o.Hand, o.Kept, o.Discards} {
		for _, c := range l {
			out[c] = true
		}
	}
	if o.Starter != nil {
		out[*o.Starter] = true
	}
	for _, p := range o.Played {
		out[p.Card] = true
	}
	return out
}

func TestHumanGameAndCheating(t *testing.T) {
	_, addr := startServer(t)
	a, b, eve := login(t, addr, "alice"), login(t, addr, "bob"), login(t, addr, "eve")

	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{}}})
	u := a.next(t, replyTo(id)).GetGameUpdate()
	if u == nil || u.Game.Status != pb.GameStatus_GAME_STATUS_WAITING || u.Observation != nil {
		t.Fatalf("expected waiting game, got %v", u)
	}
	gameID := u.Game.GameId

	id = b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}})
	list := b.next(t, replyTo(id)).GetGameList()
	if len(list.Open) != 1 || list.Open[0].GameId != gameID || list.Open[0].Players[0].Nickname != "Nick alice" {
		t.Fatalf("open games: %v", list)
	}

	id = a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})
	wantError(t, a.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	id = b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})
	ub := b.next(t, replyTo(id)).GetGameUpdate()
	ua := a.next(t, isUpdate(gameID)).GetGameUpdate()
	if ub.Game.Status != pb.GameStatus_GAME_STATUS_ACTIVE || ub.Game.Seat != 1 || ua.Game.Seat != 0 {
		t.Fatalf("join: a=%v b=%v", ua.Game, ub.Game)
	}

	id = eve.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})
	wantError(t, eve.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	// To an outsider, someone else's game is not found, as if it did not exist.
	id = eve.send(t, act(gameID, ua.Legal[0]))
	wantError(t, eve.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	id = eve.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: gameID}}})
	wantError(t, eve.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	// Bob tries to discard Alice's cards.
	id = b.send(t, act(gameID, ua.Legal[0]))
	wantError(t, b.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_ILLEGAL_ACTION)
	id = b.send(t, act(gameID, &pb.Action{Kind: pb.ActionKind_ACTION_KIND_PLAY, Cards: []uint32{ub.Observation.Hand[0]}}))
	wantError(t, b.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_ILLEGAL_ACTION)
	id = b.send(t, act(gameID, &pb.Action{Kind: pb.ActionKind_ACTION_KIND_DISCARD, Cards: []uint32{0, 200}}))
	wantError(t, b.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	// Play the game out with random moves, recording each player's view per seq.
	views := [2]map[uint32]*pb.GameUpdate{{}, {}}
	views[0][ua.Seq], views[1][ub.Seq] = ua, ub
	r := rand.New(rand.NewPCG(1, 2))
	cur := [2]*pb.GameUpdate{ua, ub}
	clients := [2]*tclient{a, b}
	for cur[0].Game.Status == pb.GameStatus_GAME_STATUS_ACTIVE {
		// Exactly one update per player per step, so wait for both.
		var p int
		for p = 0; p < 2; p++ {
			if len(cur[p].Legal) > 0 {
				break
			}
		}
		if p == 2 {
			t.Fatalf("active game but nobody can act at seq %d", cur[0].Seq)
		}
		clients[p].send(t, act(gameID, cur[p].Legal[r.IntN(len(cur[p].Legal))]))
		for q := 0; q < 2; q++ {
			cur[q] = clients[q].next(t, isUpdate(gameID)).GetGameUpdate()
			views[q][cur[q].Seq] = cur[q]
		}
		if cur[0].Seq != cur[1].Seq {
			t.Fatalf("players out of step: %d vs %d", cur[0].Seq, cur[1].Seq)
		}
	}
	if cur[0].Game.Status != pb.GameStatus_GAME_STATUS_FINISHED || cur[0].Game.Winner < 0 {
		t.Fatalf("game did not finish: %v", cur[0].Game)
	}

	for seq, va := range views[0] {
		vb := views[1][seq]
		for q, pair := range [2][2]*pb.GameUpdate{{va, vb}, {vb, va}} {
			mine, theirs := pair[0], pair[1]
			seen := visibleCards(mine.Observation)
			for _, c := range theirs.Observation.Hand {
				if seen[c] {
					t.Fatalf("seq %d: player %d can see opponent's held card %d", seq, q, c)
				}
			}
			for _, e := range mine.Events {
				switch e.Kind {
				case pb.EventKind_EVENT_KIND_DEAL, pb.EventKind_EVENT_KIND_DISCARD, pb.EventKind_EVENT_KIND_GO:
					if e.Card != nil || len(e.Cards) > 0 {
						t.Fatalf("seq %d: %v event carries cards", seq, e.Kind)
					}
				}
			}
		}
	}

	// The finished game is gone.
	id = a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: gameID}}})
	wantError(t, a.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
}

func TestResignAndCancel(t *testing.T) {
	_, addr := startServer(t)
	a := login(t, addr, "alice")

	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "heuristic"}}})
	u := a.next(t, replyTo(id)).GetGameUpdate()
	if u.Game.Players[1].Bot != "heuristic" || !u.Observation.MustAct || !u.Observation.OppDiscarded {
		t.Fatalf("bot game: %v", u)
	}
	id = a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resign{Resign: &pb.Resign{GameId: u.Game.GameId}}})
	r := a.next(t, replyTo(id)).GetGameUpdate()
	if r.Game.Status != pb.GameStatus_GAME_STATUS_FINISHED || !r.Game.Resigned || r.Game.Winner != 1 ||
		len(r.Events) != 1 || r.Events[0].Kind != pb.EventKind_EVENT_KIND_RESIGN {
		t.Fatalf("resign: %v", r)
	}
	id = a.send(t, act(u.Game.GameId, u.Legal[0]))
	wantError(t, a.next(t, replyTo(id)), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Cancelling an open game removes it from the list.
	id = a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{}}})
	open := a.next(t, replyTo(id)).GetGameUpdate().Game.GameId
	id = a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resign{Resign: &pb.Resign{GameId: open}}})
	a.next(t, replyTo(id))
	b := login(t, addr, "bob")
	id = b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}})
	if l := b.next(t, replyTo(id)).GetGameList(); len(l.Open) != 0 {
		t.Fatalf("cancelled game still open: %v", l)
	}
}

func TestResumeOnNewConnection(t *testing.T) {
	_, addr := startServer(t)
	a := login(t, addr, "alice")
	id := a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "random"}}})
	u := a.next(t, replyTo(id)).GetGameUpdate()
	a.Close()

	c := connect(t, addr)
	if _, err := c.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	c.startReading()
	s := c.next(t, isUpdate(u.Game.GameId)).GetGameUpdate()
	if !s.Full || s.Seq != u.Seq || len(s.Legal) != len(u.Legal) {
		t.Fatalf("resume sync: %v", s)
	}
	id = c.send(t, act(u.Game.GameId, s.Legal[0]))
	if n := c.next(t, replyTo(id)).GetGameUpdate(); n == nil || n.Seq <= s.Seq {
		t.Fatalf("act after resume failed")
	}

	// Two connections for the same user both receive updates.
	d := connect(t, addr)
	if _, err := d.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	d.startReading()
	ds := d.next(t, isUpdate(u.Game.GameId)).GetGameUpdate()
	if len(ds.Legal) > 0 {
		id = d.send(t, act(u.Game.GameId, ds.Legal[0]))
		d.next(t, replyTo(id))
		c.next(t, func(m *pb.ServerMessage) bool {
			x := m.GetGameUpdate()
			return x != nil && x.Seq > ds.Seq
		})
	}

	var se client.ServerError
	e := connect(t, addr)
	if _, err := e.Resume("nope"); !errors.As(err, &se) {
		t.Fatalf("bad token accepted: %v", err)
	}
}

// Logging out ends the session: the server closes the connection and the
// token no longer resumes.
func TestLogout(t *testing.T) {
	_, addr := startServer(t)
	a := login(t, addr, "alice")
	// A second connection on the same token, which logging out must end.
	twin := connect(t, addr)
	if _, err := twin.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	if a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Logout{Logout: &pb.Logout{}}}))).GetAck() == nil {
		t.Fatal("logout not acknowledged")
	}
	select {
	case _, ok := <-a.msgs:
		for ok {
			_, ok = <-a.msgs
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connection kept open after logout")
	}
	twin.startReading()
	select {
	case _, ok := <-twin.msgs:
		for ok {
			_, ok = <-twin.msgs
		}
	case <-time.After(5 * time.Second):
		t.Fatal("same-token connection kept open after logout")
	}
	var se client.ServerError
	if _, err := connect(t, addr).Resume(a.auth.Token); !errors.As(err, &se) {
		t.Fatalf("token resumed after logout: %v", err)
	}
	// Other sessions of the same user still work.
	b := login(t, addr, "alice")
	if _, err := connect(t, addr).Resume(b.auth.Token); err != nil {
		t.Fatal(err)
	}
}

func TestNNBotOpponent(t *testing.T) {
	net, err := nn.New([]int{encode.Size, 16, 1}, rand.New(rand.NewPCG(1, 2)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "model.bin")
	if err := nn.Save(path, net, nn.Meta{EncodingVersion: encode.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := ai.Register(path); err != nil {
		t.Fatal(err)
	}
	_, addr := startServer(t)
	st, err := botclient.Run(context.Background(), botclient.Config{
		Addr: addr, Username: "nnfan", Key: testBotKey, Bot: "random",
		Opponent: ai.BotName, Concurrent: 20, Games: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Finished != 200 || st.Errors != 0 || st.Resigned != 0 {
		t.Fatalf("unexpected stats %+v", st)
	}
}

var (
	testExpertOnce sync.Once
	testExpertErr  error
)

// registerTestExpert registers the expert levels with a small bundle built
// from heuristic self-play.
func registerTestExpert(t *testing.T) {
	testExpertOnce.Do(func() {
		newHeuristic := func(int) bots.Bot { return bots.NewHeuristic() }
		sp, err := expert.Collect(context.Background(), newHeuristic, 200, 4, 1)
		if err != nil {
			testExpertErr = err
			return
		}
		win, err := expert.BuildWinTable(&sp.Hands, 200)
		if err != nil {
			testExpertErr = err
			return
		}
		expert.Register(&expert.Bundle{Win: win, Discard: expert.BuildDiscardTables(sp.Details), KeepTemp: [2]float64{0.5, 0.5}})
	})
	if testExpertErr != nil {
		t.Fatal(testExpertErr)
	}
}

func TestExpertBotOpponents(t *testing.T) {
	registerTestExpert(t)
	_, addr := startServer(t)
	for _, l := range expert.Levels {
		st, err := botclient.Run(context.Background(), botclient.Config{
			Addr: addr, Username: "expertfan", Key: testBotKey, Bot: "random",
			Opponent: l.Name, Concurrent: 10, Games: 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		if st.Finished != 30 || st.Errors != 0 || st.Resigned != 0 {
			t.Fatalf("%s: unexpected stats %+v", l.Name, st)
		}
	}
}
