// Package server hosts cribbage games for connected clients. Each game is an
// actor: a goroutine that owns the game state and processes commands from
// its inbox, so games run in parallel without a global lock.
package server

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
	"cribbage/internal/transport/tcp"
)

// Transport carries protobuf messages for one client connection. Read is
// called from one goroutine and Write/Flush from another.
type Transport interface {
	Read(proto.Message) error
	Write(proto.Message) error // may buffer until Flush
	Flush() error
	Close() error
	String() string
}

// Config configures a Server.
type Config struct {
	Auth       *auth.Service
	Logger     *slog.Logger
	OutboxSize int // per-connection outbound queue; default 4096
}

// Server routes client requests to game actors.
type Server struct {
	auth   *auth.Service
	log    *slog.Logger
	outbox int

	// Lock order: gmu, then game.smu, then cmu. Never hold any of these
	// while submitting to a game inbox.
	gmu       sync.RWMutex
	games     map[string]*game
	userGames map[int64]map[string]*game
	open      map[string]*game // waiting for a second player

	cmu   sync.RWMutex
	conns map[int64]map[*Conn]struct{}

	qmu   sync.Mutex
	queue []matchRequest

	stats struct {
		gamesStarted, gamesFinished, actions, conns atomic.Int64
	}
}

type matchRequest struct {
	conn  *Conn
	reqID uint32
}

// New creates a server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.OutboxSize == 0 {
		cfg.OutboxSize = 4096
	}
	return &Server{
		auth:      cfg.Auth,
		log:       cfg.Logger,
		outbox:    cfg.OutboxSize,
		games:     map[string]*game{},
		userGames: map[int64]map[string]*game{},
		open:      map[string]*game{},
		conns:     map[int64]map[*Conn]struct{}{},
	}
}

// Stats is a point-in-time snapshot of server activity.
type Stats struct {
	ActiveGames   int
	Connections   int64
	GamesStarted  int64
	GamesFinished int64
	Actions       int64
}

func (s *Server) Stats() Stats {
	s.gmu.RLock()
	n := len(s.games)
	s.gmu.RUnlock()
	return Stats{
		ActiveGames:   n,
		Connections:   s.stats.conns.Load(),
		GamesStarted:  s.stats.gamesStarted.Load(),
		GamesFinished: s.stats.gamesFinished.Load(),
		Actions:       s.stats.actions.Load(),
	}
}

// ServeTCP accepts connections until ln is closed.
func (s *Server) ServeTCP(ln net.Listener) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.ServeConn(tcp.NewConn(nc))
	}
}

// ServeConn handles one client until its transport fails or is closed.
func (s *Server) ServeConn(t Transport) {
	c := &Conn{srv: s, t: t, out: make(chan *pb.ServerMessage, s.outbox), done: make(chan struct{})}
	s.stats.conns.Add(1)
	go c.writeLoop()
	defer func() {
		c.close()
		s.unregisterConn(c)
		s.stats.conns.Add(-1)
	}()
	for {
		m := new(pb.ClientMessage)
		if err := t.Read(m); err != nil {
			return
		}
		s.handle(c, m)
	}
}

func (s *Server) handle(c *Conn, m *pb.ClientMessage) {
	id := m.RequestId
	switch b := m.Body.(type) {
	case *pb.ClientMessage_Register:
		s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
			return s.auth.Register(ctx, b.Register.Username, b.Register.Password, b.Register.Nickname)
		})
		return
	case *pb.ClientMessage_Login:
		s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
			return s.auth.Login(ctx, b.Login.Username, b.Login.Password)
		})
		return
	case *pb.ClientMessage_Resume:
		s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
			return s.auth.Resume(ctx, b.Resume.Token)
		})
		return
	case *pb.ClientMessage_Ping:
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Pong{Pong: &pb.Pong{
			ClientTime: b.Ping.ClientTime, ServerTime: uint64(time.Now().UnixMilli()),
		}}})
		return
	case nil:
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "empty message")
		return
	}

	if c.user == nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_UNAUTHENTICATED, "", "log in first")
		return
	}
	switch b := m.Body.(type) {
	case *pb.ClientMessage_CreateGame:
		s.createGame(c, id, b.CreateGame.Bot)
	case *pb.ClientMessage_JoinGame:
		s.withGame(c, id, b.JoinGame.GameId, func(g *game) { g.join(c, id) })
	case *pb.ClientMessage_ListGames:
		s.listGames(c, id)
	case *pb.ClientMessage_FindMatch:
		s.findMatch(c, id)
	case *pb.ClientMessage_CancelMatch:
		s.cancelMatch(c.user.ID)
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
	case *pb.ClientMessage_SyncGame:
		s.withGame(c, id, b.SyncGame.GameId, func(g *game) { g.sync(c, id) })
	case *pb.ClientMessage_Act:
		s.withGame(c, id, b.Act.GameId, func(g *game) { g.act(c, id, b.Act.Action) })
	case *pb.ClientMessage_Resign:
		s.withGame(c, id, b.Resign.GameId, func(g *game) { g.resign(c, id) })
	default:
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "unsupported message")
	}
}

func (s *Server) authenticate(c *Conn, id uint32, fn func(context.Context) (*auth.Result, error)) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already logged in")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := fn(ctx)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrInvalidInput):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", err.Error())
		return
	case errors.Is(err, store.ErrUsernameTaken):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_USERNAME_TAKEN, "", "username taken")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS, "", err.Error())
		return
	default:
		s.log.Error("authentication failed", "err", err)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "internal error")
		return
	}
	c.user = res.User
	s.registerConn(c)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_AuthOk{AuthOk: &pb.AuthOk{
		UserId: res.User.ID, Username: res.User.Username, Nickname: res.User.Nickname, Token: res.Token,
	}}})
	// Bring the connection up to date on every game the user is in, so a
	// reconnecting client can resume immediately.
	for _, g := range s.gamesOf(res.User.ID) {
		g.submit(func() { g.sync(c, 0) })
	}
}

func (s *Server) withGame(c *Conn, id uint32, gameID string, fn func(*game)) {
	s.gmu.RLock()
	g := s.games[gameID]
	s.gmu.RUnlock()
	if g == nil || !g.submit(func() { fn(g) }) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, gameID, "no such active game")
	}
}

func (s *Server) createGame(c *Conn, id uint32, botName string) {
	creator := seat{userID: c.user.ID, nickname: c.user.Nickname}
	if botName == "" {
		g := s.newGame([2]seat{creator, {}}, false)
		g.submit(func() { g.publish(nil, reply{c, id}) })
		return
	}
	b, err := bots.New(botName, rand.Uint64())
	if err != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", err.Error())
		return
	}
	g := s.newGame([2]seat{creator, {nickname: botName + " bot", botName: botName, bot: b}}, true)
	g.submit(func() { g.start(reply{c, id}) })
}

func (s *Server) findMatch(c *Conn, id uint32) {
	s.qmu.Lock()
	for i, q := range s.queue {
		if q.conn.user.ID != c.user.ID && !q.conn.closed() {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.qmu.Unlock()
			a := seat{userID: q.conn.user.ID, nickname: q.conn.user.Nickname}
			b := seat{userID: c.user.ID, nickname: c.user.Nickname}
			g := s.newGame([2]seat{a, b}, true)
			g.submit(func() { g.start(reply{q.conn, q.reqID}, reply{c, id}) })
			return
		}
	}
	s.queue = append(s.queue, matchRequest{conn: c, reqID: id})
	s.qmu.Unlock()
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_MatchQueued{MatchQueued: &pb.MatchQueued{}}})
}

func (s *Server) cancelMatch(userID int64) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	q := s.queue[:0]
	for _, r := range s.queue {
		if r.conn.user.ID != userID {
			q = append(q, r)
		}
	}
	s.queue = q
}

func (s *Server) listGames(c *Conn, id uint32) {
	const maxOpen = 100
	list := &pb.GameList{}
	s.gmu.RLock()
	for _, g := range s.userGames[c.user.ID] {
		list.Mine = append(list.Mine, g.summaryFor(c.user.ID))
	}
	for _, g := range s.open {
		if len(list.Open) >= maxOpen {
			break
		}
		if g.creator() != c.user.ID {
			list.Open = append(list.Open, g.summaryFor(c.user.ID))
		}
	}
	s.gmu.RUnlock()
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_GameList{GameList: list}})
}

// newGame registers a game and starts its actor. A game that is not ready
// waits for a second player to join.
func (s *Server) newGame(seats [2]seat, ready bool) *game {
	g := &game{
		id:     auth.RandomID(8),
		srv:    s,
		seats:  seats,
		inbox:  make(chan func(), 64),
		done:   make(chan struct{}),
		status: pb.GameStatus_GAME_STATUS_WAITING,
		winner: -1,
	}
	g.updateSummary()
	s.gmu.Lock()
	s.games[g.id] = g
	for _, st := range seats {
		if st.userID != 0 {
			s.addUserGame(st.userID, g)
		}
	}
	if !ready {
		s.open[g.id] = g
	}
	s.gmu.Unlock()
	go g.run()
	return g
}

// addUserGame must be called with gmu held.
func (s *Server) addUserGame(userID int64, g *game) {
	m := s.userGames[userID]
	if m == nil {
		m = map[string]*game{}
		s.userGames[userID] = m
	}
	m[g.id] = g
}

func (s *Server) gameJoined(g *game, userID int64) {
	s.gmu.Lock()
	delete(s.open, g.id)
	s.addUserGame(userID, g)
	s.gmu.Unlock()
}

func (s *Server) removeGame(g *game) {
	s.gmu.Lock()
	delete(s.games, g.id)
	delete(s.open, g.id)
	for _, st := range g.seats {
		if m := s.userGames[st.userID]; m != nil {
			delete(m, g.id)
			if len(m) == 0 {
				delete(s.userGames, st.userID)
			}
		}
	}
	s.gmu.Unlock()
}

func (s *Server) gamesOf(userID int64) []*game {
	s.gmu.RLock()
	defer s.gmu.RUnlock()
	out := make([]*game, 0, len(s.userGames[userID]))
	for _, g := range s.userGames[userID] {
		out = append(out, g)
	}
	return out
}

func (s *Server) registerConn(c *Conn) {
	s.cmu.Lock()
	m := s.conns[c.user.ID]
	if m == nil {
		m = map[*Conn]struct{}{}
		s.conns[c.user.ID] = m
	}
	m[c] = struct{}{}
	s.cmu.Unlock()
}

func (s *Server) unregisterConn(c *Conn) {
	if c.user == nil {
		return
	}
	s.cmu.Lock()
	if m := s.conns[c.user.ID]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(s.conns, c.user.ID)
		}
	}
	s.cmu.Unlock()

	s.qmu.Lock()
	q := s.queue[:0]
	for _, r := range s.queue {
		if r.conn != c {
			q = append(q, r)
		}
	}
	s.queue = q
	s.qmu.Unlock()
}

func (s *Server) connected(userID int64) bool {
	s.cmu.RLock()
	defer s.cmu.RUnlock()
	return len(s.conns[userID]) > 0
}

type reply struct {
	conn *Conn
	id   uint32
}

// pushUser sends m to every connection of a user. A connection listed in
// replies receives a copy carrying its request id.
func (s *Server) pushUser(userID int64, m *pb.ServerMessage, replies []reply) {
	s.cmu.RLock()
	defer s.cmu.RUnlock()
	for c := range s.conns[userID] {
		out := m
		for _, r := range replies {
			if r.conn == c && r.id != 0 {
				out = &pb.ServerMessage{RequestId: r.id, Body: m.Body}
			}
		}
		c.send(out)
	}
}
