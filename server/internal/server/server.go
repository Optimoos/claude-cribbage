// Package server hosts cribbage games for connected clients. Each game is an
// actor: a goroutine that owns the game state and processes commands from
// its inbox, so games run in parallel without a global lock.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/netkey"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/ratelimit"
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
	Games      store.GameStore // defaults to an in-memory store
	Logger     *slog.Logger
	OutboxSize int // per-connection outbound queue; default 4096
	// AuthLimits bounds sign-in attempts; nil uses DefaultAuthLimits.
	AuthLimits *AuthLimits
	// GameLimits bounds games and invites per player and sets how long
	// they last; nil uses DefaultGameLimits.
	GameLimits *GameLimits
}

// GameLimits bounds what one player can hold open, and when idle games
// sleep and expire.
type GameLimits struct {
	Games    int // unfinished games (waiting or active) per player
	BotGames int // of which against server bots
	Open     int // open games waiting for anyone to join

	InvitesOut int // invites a player has sent awaiting an answer
	InvitesIn  int // invites waiting for one player's answer

	InviteFor  time.Duration // an invite nobody answers is withdrawn
	OpenFor    time.Duration // an open game nobody joins is cancelled
	HumanIdle  time.Duration // a game between people with no move expires
	BotIdle    time.Duration // a game against a bot with no move expires
	SleepAfter time.Duration // a game with no activity leaves memory
	SweepEvery time.Duration // how often to look for expired games and invites

	// BotMoves is how many bot decisions may be computed at once (default
	// GOMAXPROCS-1), so bot games played as fast as a script can send
	// moves cannot take every core.
	BotMoves int
}

// DefaultGameLimits suit slow family games: up to 20 games each, games
// between people last 60 days without a move, and a game sleeps (costing
// only its database rows) after 10 minutes without activity.
var DefaultGameLimits = GameLimits{
	Games: 20, BotGames: 5, Open: 1,
	InvitesOut: 5, InvitesIn: 20,
	InviteFor:  48 * time.Hour,
	OpenFor:    48 * time.Hour,
	HumanIdle:  60 * 24 * time.Hour,
	BotIdle:    30 * 24 * time.Hour,
	SleepAfter: 10 * time.Minute,
	SweepEvery: time.Hour,
}

// AuthLimits bounds sign-in attempts from remote clients, as token buckets
// (a burst, then one more per interval). Connections from loopback or with
// no address (in-process transports) are not limited.
type AuthLimits struct {
	AttemptsPerIP int // sign-in attempts (codes, links, passkeys) from one address
	AttemptEvery  time.Duration
	CodesPerIP    int // sign-in codes asked for from one address
	CodeEvery     time.Duration
	// Connections: at most ConnsPerAddr from one address (as limited
	// above) and Conns in all; one that has not signed in after
	// LoginWithin is closed, or after SignInWithin if it started signing
	// in (asked for a code, say).
	ConnsPerAddr int
	Conns        int
	LoginWithin  time.Duration
	SignInWithin time.Duration
	// Requests after logging in: ActionsPerUser at once, then one more
	// every ActionEvery, across all of a player's connections. Zero uses
	// the default.
	ActionsPerUser int
	ActionEvery    time.Duration
}

// DefaultAuthLimits allows 20 quick attempts per address then 10 a minute,
// and 5 sign-in codes then 1 every 5 minutes, so one address cannot try
// many accounts' addresses.
// (Each account and address also gets few emails, and few wrong codes: see
// the auth package.) IPv6 addresses count by their /64. A player may make
// 60 requests at once, then 10 a second: far more than anyone clicking,
// but it bounds a script.
var DefaultAuthLimits = AuthLimits{
	AttemptsPerIP: 20, AttemptEvery: 6 * time.Second,
	CodesPerIP: 5, CodeEvery: 5 * time.Minute,
	ConnsPerAddr: 20, Conns: 5000, LoginWithin: 30 * time.Second, SignInWithin: 20 * time.Minute,
	ActionsPerUser: 60, ActionEvery: 100 * time.Millisecond,
}

// Server routes client requests to game actors.
type Server struct {
	auth      *auth.Service
	gameStore store.GameStore
	log       *slog.Logger
	outbox    int

	quit     chan struct{} // closed by Shutdown
	quitOnce sync.Once
	actors   sync.WaitGroup

	// Lock order: gmu, then game.smu, then cmu. Never hold any of these
	// while submitting to a game inbox.
	gmu       sync.RWMutex
	games     map[string]*game
	userGames map[int64]map[string]*game
	open      map[string]*game // waiting for a second player

	cmu      sync.RWMutex
	conns    map[int64]map[*Conn]struct{} // authenticated, by user
	allConns map[*Conn]struct{}

	qmu   sync.Mutex
	queue []matchRequest

	attempts, codes *ratelimit.Limiter
	actions         *ratelimit.Limiter // requests per user, once signed in
	botMoves        chan struct{}      // bot decisions in progress

	limits  GameLimits
	authLim AuthLimits // connection limits (cmu guards connsByAddr)

	connsByAddr map[string]int
	// reserved counts games being created for each user, not yet in
	// userGames, so concurrent creations cannot pass the limits. Guarded
	// by gmu.
	reserved map[int64]reservation
	// deleted marks users whose accounts are being or have been deleted,
	// so they cannot start games. Guarded by gmu.
	deleted map[int64]bool

	imu                    sync.Mutex
	inv                    invites
	invitesSent, codeTries *ratelimit.Limiter

	devices devices // sign-ins waiting for approval from another device

	bg sync.WaitGroup // background requests, such as sending email
	// readers: connections' read loops, which handle requests (and may
	// write to the store) themselves.
	readers sync.WaitGroup

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
	if cfg.Games == nil {
		cfg.Games = store.NewMemory()
	}
	gl := DefaultGameLimits
	if cfg.GameLimits != nil {
		gl = *cfg.GameLimits
	}
	lim := DefaultAuthLimits
	if cfg.AuthLimits != nil {
		lim = *cfg.AuthLimits
	}
	if lim.SignInWithin < lim.LoginWithin {
		lim.SignInWithin = max(lim.LoginWithin, DefaultAuthLimits.SignInWithin)
	}
	if lim.ActionsPerUser <= 0 {
		lim.ActionsPerUser, lim.ActionEvery = DefaultAuthLimits.ActionsPerUser, DefaultAuthLimits.ActionEvery
	}
	if gl.BotMoves <= 0 {
		gl.BotMoves = max(1, runtime.GOMAXPROCS(0)-1) // leave a core for everything else
	}
	s := &Server{
		attempts:    ratelimit.New(lim.AttemptsPerIP, lim.AttemptEvery),
		codes:       ratelimit.New(lim.CodesPerIP, lim.CodeEvery),
		actions:     ratelimit.New(lim.ActionsPerUser, lim.ActionEvery),
		botMoves:    make(chan struct{}, gl.BotMoves),
		limits:      gl,
		authLim:     lim,
		connsByAddr: map[string]int{},
		reserved:    map[int64]reservation{},
		deleted:     map[int64]bool{},
		inv:         newInvites(),
		// Ten invites, then one every six minutes; ten code guesses, then
		// one a minute.
		invitesSent: ratelimit.New(10, 6*time.Minute),
		codeTries:   ratelimit.New(10, time.Minute),
		devices:     newDevices(),
		auth:        cfg.Auth,
		gameStore:   cfg.Games,
		log:         cfg.Logger,
		outbox:      cfg.OutboxSize,
		quit:        make(chan struct{}),
		allConns:    map[*Conn]struct{}{},
		games:       map[string]*game{},
		userGames:   map[int64]map[string]*game{},
		open:        map[string]*game{},
		conns:       map[int64]map[*Conn]struct{}{},
	}
	go s.sweep()
	return s
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
	c := &Conn{srv: s, t: t, out: make(chan *pb.ServerMessage, s.outbox), done: make(chan struct{}), drain: make(chan struct{})}
	if limited(c) {
		c.addr = addrKey(remoteIP(t))
	}
	s.cmu.Lock()
	if s.shuttingDown() || len(s.allConns) >= s.authLim.Conns ||
		(c.addr != "" && s.connsByAddr[c.addr] >= s.authLim.ConnsPerAddr) {
		s.cmu.Unlock()
		t.Close()
		return
	}
	s.allConns[c] = struct{}{}
	if c.addr != "" {
		s.connsByAddr[c.addr]++
	}
	s.readers.Add(1)
	s.cmu.Unlock()
	defer s.readers.Done()
	if c.addr != "" {
		// Remote connections must sign in promptly, or soon after they
		// start to (asking for a code, say).
		var t *time.Timer
		t = time.AfterFunc(s.authLim.LoginWithin, func() {
			if c.authed.Load() {
				return
			}
			if c.signingIn.CompareAndSwap(true, false) {
				t.Reset(s.authLim.SignInWithin - s.authLim.LoginWithin)
				return
			}
			c.close()
		})
		defer t.Stop()
	}
	s.stats.conns.Add(1)
	go c.writeLoop()
	defer func() {
		c.close()
		s.unregisterConn(c)
		s.stats.conns.Add(-1)
	}()
	preLogin := 0
	for {
		m := new(pb.ClientMessage)
		if err := t.Read(m); err != nil {
			return
		}
		if m.ProtocolVersion != protocolVersion {
			s.refuseVersion(c, m)
			return
		}
		// A remote client gets a few messages to log in with (each may
		// cost a database lookup), not as many as it can send in the time
		// it has.
		if c.addr != "" && !c.authed.Load() {
			if preLogin++; preLogin > maxPreLogin {
				return
			}
		}
		s.handle(c, m)
	}
}

const protocolVersion = uint32(pb.ProtocolVersion_PROTOCOL_VERSION_CURRENT)

// refuseVersion tells a client built for another protocol version to
// update, and closes the connection once that is sent.
func (s *Server) refuseVersion(c *Conn, m *pb.ClientMessage) {
	msg := "This version of Cribbage is out of date. Please update it to keep playing."
	if m.ProtocolVersion > protocolVersion {
		msg = "This server runs an older version of Cribbage than your app. Try again later."
	}
	c.sendError(m.RequestId, pb.ErrorCode_ERROR_CODE_UPGRADE_REQUIRED, "", msg)
	c.drainAndClose()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// maxPreLogin is how many messages a remote connection may send before it
// signs in: plenty for a client that mistypes a code a few times.
const maxPreLogin = 50

func (s *Server) handle(c *Conn, m *pb.ClientMessage) {
	id := m.RequestId
	switch b := m.Body.(type) {
	case *pb.ClientMessage_RequestSignIn:
		s.requestSignIn(c, id, b.RequestSignIn.Name)
		return
	case *pb.ClientMessage_EnterCode:
		if s.allowSignIn(c, id) {
			s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
				e := b.EnterCode
				return s.auth.EnterCode(ctx, e.Challenge, e.Code, e.Username, e.Nickname)
			})
		}
		return
	case *pb.ClientMessage_UseSignInLink:
		if s.allowSignIn(c, id) {
			s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
				l := b.UseSignInLink
				return s.auth.UseSignInLink(ctx, l.Token, l.Username, l.Nickname)
			})
		}
		return
	case *pb.ClientMessage_PasskeySignInBegin:
		s.passkeySignInBegin(c, id)
		return
	case *pb.ClientMessage_PasskeySignInFinish:
		if s.allowSignIn(c, id) {
			sd := c.passkey
			c.passkey = nil
			s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
				return s.auth.FinishPasskeySignIn(ctx, sd, b.PasskeySignInFinish.CredentialJson)
			})
		}
		return
	case *pb.ClientMessage_StartDeviceSignIn:
		s.startDeviceSignIn(c, id)
		return
	case *pb.ClientMessage_ClaimDeviceSignIn:
		s.claimDeviceSignIn(c, id, b.ClaimDeviceSignIn.Challenge)
		return
	case *pb.ClientMessage_BotLogin:
		if s.allowSignIn(c, id) {
			s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
				return s.auth.BotLogin(ctx, b.BotLogin.Username, b.BotLogin.Nickname, b.BotLogin.Key)
			})
		}
		return
	case *pb.ClientMessage_Resume:
		s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
			return s.auth.Resume(ctx, b.Resume.Token)
		})
		return
	case *pb.ClientMessage_VerifyEmail:
		s.verifyEmail(c, id, b.VerifyEmail.Token)
		return
	case *pb.ClientMessage_ConfirmDeleteAccount:
		s.confirmDeleteAccount(c, id, b.ConfirmDeleteAccount.Token)
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
	if limited(c) && !s.actions.Take(c.userKey, time.Now()) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many requests; slow down")
		return
	}
	switch b := m.Body.(type) {
	case *pb.ClientMessage_GetHistory:
		s.getHistory(c, id, b.GetHistory)
	case *pb.ClientMessage_SetEmail:
		s.setEmail(c, id, b.SetEmail)
	case *pb.ClientMessage_DeleteAccount:
		s.deleteAccount(c, id, b.DeleteAccount)
	case *pb.ClientMessage_SignOutOthers:
		s.signOutOthers(c, id)
	case *pb.ClientMessage_ApproveDevice:
		s.approveDevice(c, id, b.ApproveDevice.Code)
	case *pb.ClientMessage_PasskeyAddBegin:
		s.passkeyAddBegin(c, id)
	case *pb.ClientMessage_PasskeyAddFinish:
		s.passkeyAddFinish(c, id, b.PasskeyAddFinish)
	case *pb.ClientMessage_PasskeyRemove:
		s.passkeyRemove(c, id, b.PasskeyRemove.Id)
	case *pb.ClientMessage_CreateGame:
		s.createGame(c, id, b.CreateGame.Bot)
	case *pb.ClientMessage_JoinGame:
		release, err := s.reserve(gameKind{}, reserveFor{id: c.user.ID, self: true})
		if err != nil {
			s.sendLimit(c, id, err)
			return
		}
		if !s.withGame(c, id, b.JoinGame.GameId, func(g *game) { defer release(); g.join(c, id) }) {
			release()
		}
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
	case *pb.ClientMessage_Logout:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.auth.Logout(ctx, c.token)
		cancel()
		if err != nil {
			s.log.Error("logout", "user", c.user.ID, "err", err)
			c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "internal error")
			return
		}
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
		c.drainAndClose()
	case *pb.ClientMessage_SendInvite:
		s.sendInvite(c, id, b.SendInvite.Username)
	case *pb.ClientMessage_RespondInvite:
		s.respondInvite(c, id, b.RespondInvite.InviteId, b.RespondInvite.Accept)
	case *pb.ClientMessage_RedeemCode:
		s.redeemCode(c, id, b.RedeemCode.Code)
	case *pb.ClientMessage_Resign:
		s.withGame(c, id, b.Resign.GameId, func(g *game) { g.resign(c, id) })
	default:
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "unsupported message")
	}
}

// remoteIP is the address of the client at the other end of a transport,
// if the transport knows it.
func remoteIP(t Transport) string {
	if r, ok := t.(interface{ RemoteIP() string }); ok {
		return r.RemoteIP()
	}
	return ""
}

// limited reports whether c is subject to AuthLimits: every connection
// over a transport that reports addresses (TCP, WebRTC), except from
// loopback. One whose address is not known yet is limited, as "unknown".
func limited(c *Conn) bool {
	if _, ok := c.t.(interface{ RemoteIP() string }); !ok {
		return false // in-process
	}
	ip := net.ParseIP(remoteIP(c.t))
	return ip == nil || !ip.IsLoopback()
}

// addrKey is the limiter key for an address (IPv6 by its /64).
func addrKey(addr string) string { return netkey.Of(addr) }

// allowSignIn spends one of c's sign-in attempts, sending an error if
// there are none left.
func (s *Server) allowSignIn(c *Conn, id uint32) bool {
	if c.user != nil {
		return true // authenticate reports "already signed in"
	}
	return s.allowFrom(c, id, s.attempts)
}

func (s *Server) authenticate(c *Conn, id uint32, fn func(context.Context) (*auth.Result, error)) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already signed in")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := fn(ctx)
	if err != nil {
		s.sendAccountError(c, id, "sign in", err)
		return
	}
	acct, err := s.accountMessage(ctx, res.User)
	if err != nil {
		s.log.Error("account", "user", res.User.ID, "err", err)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "internal error")
		return
	}
	c.user, c.token, c.userKey = res.User, res.Token, strconv.FormatInt(res.User.ID, 10)
	c.passkey = nil
	c.authed.Store(true)
	s.registerConn(c)
	// Signing out other devices (or the admin) ends sessions and then
	// closes the connections it can see. One that ended this session before
	// c was registered missed c, so make sure the session is still there.
	if uid, _, err := s.auth.Store.SessionUser(ctx, res.Token); err != nil || uid != res.User.ID {
		if !errors.Is(err, store.ErrNotFound) && err != nil {
			s.log.Error("session check", "user", res.User.ID, "err", err)
		}
		c.close()
		return
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_AuthOk{AuthOk: &pb.AuthOk{
		UserId: res.User.ID, Username: res.User.Username, Nickname: res.User.Nickname, Token: res.Token,
		Account: acct,
	}}})
	// Bring the connection up to date on every game the user is in, so a
	// reconnecting client can resume immediately.
	for _, g := range s.gamesOf(res.User.ID) {
		g.submit(func() { g.sync(c, 0) })
	}
}

// withGame runs fn on the game's actor and reports whether it was queued.
func (s *Server) withGame(c *Conn, id uint32, gameID string, fn func(*game)) bool {
	s.gmu.RLock()
	g := s.games[gameID]
	s.gmu.RUnlock()
	if g == nil || !g.submit(func() { fn(g) }) {
		if !s.shuttingDown() { // else the connection is about to close and the client will resync
			c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, gameID, "no such active game")
		}
		return false
	}
	return true
}

func (s *Server) createGame(c *Conn, id uint32, botName string) {
	creator := seat{userID: c.user.ID, nickname: c.user.Nickname}
	release, err := s.reserve(gameKind{bot: botName != "", open: botName == ""}, reserveFor{id: c.user.ID, self: true})
	if err != nil {
		s.sendLimit(c, id, err)
		return
	}
	defer release()
	if botName == "" {
		g, err := s.newGame([2]seat{creator, {}}, false)
		if err != nil {
			if s.shuttingDown() {
				return
			}
			c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
			return
		}
		g.submit(func() { g.publish(nil, reply{c, id}) })
		return
	}
	b, err := bots.New(botName, newSeed())
	if err != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", err.Error())
		return
	}
	g, err := s.newGame([2]seat{creator, {nickname: botName + " bot", botName: botName, bot: b}}, true)
	if err != nil {
		if s.shuttingDown() {
			return
		}
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
		return
	}
	g.submit(func() { g.start(reply{c, id}) })
}

func (s *Server) findMatch(c *Conn, id uint32) {
	// Requests already queued count as games, so a player's places in the
	// queue are bounded by the game limit; check before queueing so a
	// player at it is told now rather than when someone else turns up.
	s.qmu.Lock()
	queued := 0
	for _, q := range s.queue {
		if q.conn.user.ID == c.user.ID {
			queued++
		}
	}
	s.qmu.Unlock()
	release, err := s.reserve(gameKind{}, reserveFor{id: c.user.ID, self: true, extra: queued})
	if err != nil {
		s.sendLimit(c, id, err)
		return
	}
	release()
	s.qmu.Lock()
	for i, q := range s.queue {
		if q.conn.user.ID != c.user.ID && !q.conn.closed() {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.qmu.Unlock()
			release, err := s.reserve(gameKind{},
				reserveFor{id: q.conn.user.ID, name: q.conn.user.Nickname},
				reserveFor{id: c.user.ID, self: true})
			if err != nil {
				s.sendLimit(c, id, err)
				s.sendLimit(q.conn, q.reqID, err)
				return
			}
			defer release()
			a := seat{userID: q.conn.user.ID, nickname: q.conn.user.Nickname}
			b := seat{userID: c.user.ID, nickname: c.user.Nickname}
			g, err := s.newGame([2]seat{a, b}, true)
			if err != nil {
				if s.shuttingDown() {
					return
				}
				q.conn.sendError(q.reqID, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
				c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
				return
			}
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
	// Query the database first so the in-memory snapshot below is as fresh
	// as possible when sent. Clients must still tolerate a list that lags
	// behind game updates: they are produced on different goroutines.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recent, err := s.gameStore.RecentGames(ctx, c.user.ID, 20)
	if err != nil {
		s.log.Error("recent games", "user", c.user.ID, "err", err)
	}
	for _, g := range recent {
		list.Recent = append(list.Recent, finishedSummary(g, c.user.ID))
	}

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
	list.Invites = s.invitesOf(c.user.ID)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_GameList{GameList: list}})
}

// finishedSummary describes a finished game from its stored record.
func finishedSummary(g store.Game, userID int64) *pb.GameSummary {
	out := &pb.GameSummary{
		GameId:   g.ID,
		Status:   pb.GameStatus_GAME_STATUS_FINISHED,
		Seat:     -1,
		Scores:   []int32{int32(g.Scores[0]), int32(g.Scores[1])},
		Winner:   int32(g.Winner),
		Resigned: g.Resigned,
		Updated:  g.Updated.UnixMilli(),
	}
	for i, st := range g.Seats {
		out.Players = append(out.Players, &pb.PlayerInfo{UserId: st.UserID, Nickname: st.Nickname, Bot: st.Bot})
		if st.UserID == userID && userID != 0 {
			out.Seat = int32(i)
		}
	}
	return out
}

// newGame persists and registers a game and starts its actor. A game that
// is not ready waits for a second player to join.
func (s *Server) newGame(seats [2]seat, ready bool) (*game, error) {
	if s.shuttingDown() {
		return nil, errShuttingDown
	}
	g := &game{
		id:      newGameID(),
		srv:     s,
		seats:   seats,
		inbox:   make(chan func(), 64),
		done:    make(chan struct{}),
		seed:    newSeed(),
		status:  pb.GameStatus_GAME_STATUS_WAITING,
		winner:  -1,
		created: time.Now(),
		loaded:  true,
	}
	if ready {
		g.status = pb.GameStatus_GAME_STATUS_ACTIVE
	}
	g.lastMove.Store(g.created.UnixMilli())
	rec := g.record()
	g.savedAt = rec.Updated
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.gameStore.CreateGame(ctx, &rec); err != nil {
		s.log.Error("create game", "err", err)
		return nil, err
	}
	g.updateSummary()
	if err := s.register(g); err != nil {
		return nil, err
	}
	return g, nil
}

var errShuttingDown = errors.New("server is shutting down")

func (s *Server) register(g *game) error {
	s.gmu.Lock()
	if s.shuttingDown() {
		s.gmu.Unlock()
		return errShuttingDown
	}
	s.games[g.id] = g
	for _, st := range g.seats {
		if st.userID != 0 {
			s.addUserGame(st.userID, g)
		}
	}
	if g.status == pb.GameStatus_GAME_STATUS_WAITING {
		s.open[g.id] = g
	}
	s.gmu.Unlock()
	return nil
}

// startActor starts g's goroutine unless the server is stopping. The
// caller holds g.amu.
func (s *Server) startActor(g *game) bool {
	s.gmu.RLock() // so Add is never concurrent with Shutdown's Wait
	defer s.gmu.RUnlock()
	if s.shuttingDown() {
		return false
	}
	s.actors.Add(1)
	go g.run()
	return true
}

// Restore loads unfinished games from the store and resumes them. Call it
// once, before serving connections.
func (s *Server) Restore(ctx context.Context) (int, error) {
	if err := s.loadInvites(ctx); err != nil {
		return 0, err
	}
	recs, err := s.gameStore.LoadUnfinished(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range recs {
		g, err := s.restoreGame(rec)
		if err != nil {
			s.log.Error("cannot restore game; leaving it untouched", "game", rec.Game.ID, "err", err)
			continue
		}
		if err := s.register(g); err != nil {
			return n, err
		}
		g.submit(g.resume)
		n++
	}
	return n, nil
}

// Shutdown stops the server in order: refuse new work, let every game
// finish the command it is running (its changes are already persisted),
// then deliver queued messages and disconnect clients. Games resume when a
// new server calls Restore. Close listeners first.
func (s *Server) Shutdown() {
	s.quitOnce.Do(func() {
		s.gmu.Lock()
		s.cmu.Lock()
		close(s.quit)
		s.cmu.Unlock()
		s.gmu.Unlock()

		s.actors.Wait()
		s.bg.Wait()

		s.cmu.RLock()
		conns := make([]*Conn, 0, len(s.allConns))
		for c := range s.allConns {
			conns = append(conns, c)
		}
		s.cmu.RUnlock()
		timeout := time.After(5 * time.Second)
		for _, c := range conns {
			c.drainAndClose()
		}
		for _, c := range conns {
			select {
			case <-c.done:
			case <-timeout:
				c.close()
			}
		}
		// A request being handled finishes before the store can close.
		s.readers.Wait()
	})
}

func (s *Server) shuttingDown() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func newSeed() uint64 { return rand.Uint64() }

// newGameID returns a unique ID that sorts by creation time, so new games
// are appended to the end of the database's games table instead of
// scattered across it.
func newGameID() string {
	return fmt.Sprintf("%012x%08x", time.Now().UnixMilli(), rand.Uint32())
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
	first := len(m) == 1
	s.cmu.Unlock()
	if first {
		s.notifyPresence(c.user.ID)
	}
}

// notifyPresence tells a user's opponents that they connected or left.
func (s *Server) notifyPresence(userID int64) {
	if s.shuttingDown() {
		return
	}
	for _, g := range s.gamesOf(userID) {
		g.submit(func() { g.presenceChanged(userID) })
	}
}

func (s *Server) unregisterConn(c *Conn) {
	s.cmu.Lock()
	delete(s.allConns, c)
	if c.addr != "" {
		if s.connsByAddr[c.addr]--; s.connsByAddr[c.addr] <= 0 {
			delete(s.connsByAddr, c.addr)
		}
	}
	last := false
	if c.user != nil {
		if m := s.conns[c.user.ID]; m != nil {
			delete(m, c)
			if len(m) == 0 {
				delete(s.conns, c.user.ID)
				last = true
			}
		}
	}
	s.cmu.Unlock()
	if c.user == nil {
		return
	}
	if last {
		s.notifyPresence(c.user.ID)
	}

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
