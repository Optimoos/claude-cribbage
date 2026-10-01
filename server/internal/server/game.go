package server

import (
	"math/rand/v2"
	"sync"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
	"cribbage/internal/protocol"
	"cribbage/internal/protocol/pb"
)

type seat struct {
	userID   int64 // 0 for bots and empty seats
	nickname string
	botName  string
	bot      bots.Bot
}

// game is an actor. Fields below "owned by the actor" may only be touched
// by commands running on its goroutine (see submit).
type game struct {
	id    string
	srv   *Server
	inbox chan func()
	done  chan struct{}

	// Owned by the actor.
	seats    [2]seat
	eng      *engine.Game
	seed     uint64
	seq      uint32
	status   pb.GameStatus
	winner   int
	resigned bool
	obs      engine.Observation
	legal    []engine.Action

	// Snapshot for other goroutines (ListGames), guarded by smu.
	smu sync.Mutex
	sum summary
}

type summary struct {
	status   pb.GameStatus
	seats    [2]seat
	scores   [2]int
	winner   int
	toAct    [2]bool
	resigned bool
}

func (g *game) run() {
	for f := range g.inbox {
		f()
		if g.status == pb.GameStatus_GAME_STATUS_FINISHED {
			g.srv.removeGame(g)
			g.srv.stats.gamesFinished.Add(1)
			close(g.done)
			return
		}
	}
}

// submit queues f to run on the actor. It reports false if the game has
// already finished.
func (g *game) submit(f func()) bool {
	select {
	case <-g.done:
		return false
	default:
	}
	select {
	case g.inbox <- f:
		return true
	case <-g.done:
		return false
	}
}

func (g *game) creator() int64 { return g.seats[0].userID } // immutable

func (g *game) seatOf(userID int64) int {
	for i, s := range g.seats {
		if s.userID == userID && userID != 0 {
			return i
		}
	}
	return -1
}

// start deals the first hand and lets bots act.
func (g *game) start(replies ...reply) {
	g.seed = rand.Uint64()
	g.eng = engine.NewGame(g.seed)
	g.status = pb.GameStatus_GAME_STATUS_ACTIVE
	g.srv.stats.gamesStarted.Add(1)
	events := protocol.AppendEvents(nil, g.eng.Events())
	events = g.driveBots(events)
	g.publish(events, replies...)
}

func (g *game) join(c *Conn, id uint32) {
	if g.status != pb.GameStatus_GAME_STATUS_WAITING {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_GAME_UNAVAILABLE, g.id, "game is not open")
		return
	}
	if g.seatOf(c.user.ID) >= 0 {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, g.id, "cannot join your own game")
		return
	}
	g.seats[1] = seat{userID: c.user.ID, nickname: c.user.Nickname}
	g.srv.gameJoined(g, c.user.ID)
	g.start(reply{c, id})
}

func (g *game) act(c *Conn, id uint32, a *pb.Action) {
	p := g.seatOf(c.user.ID)
	if p < 0 {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_IN_GAME, g.id, "not a player in this game")
		return
	}
	if g.status != pb.GameStatus_GAME_STATUS_ACTIVE {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_GAME_UNAVAILABLE, g.id, "game is not active")
		return
	}
	action, err := protocol.ActionFromPB(a)
	if err != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, g.id, err.Error())
		return
	}
	if err := g.eng.Apply(p, action); err != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ILLEGAL_ACTION, g.id, err.Error())
		return
	}
	g.seq++
	g.srv.stats.actions.Add(1)
	events := protocol.AppendEvents(nil, g.eng.Events())
	events = g.driveBots(events)
	g.publish(events, reply{c, id})
}

// driveBots lets bot seats act until a human must move or the game ends.
func (g *game) driveBots(events []*pb.Event) []*pb.Event {
	for !g.eng.Over() {
		acted := false
		for p := 0; p < 2; p++ {
			b := g.seats[p].bot
			if b == nil || !g.eng.ToAct(p) {
				continue
			}
			g.eng.ObserveInto(p, &g.obs)
			g.legal = g.eng.AppendLegal(p, g.legal[:0])
			a := b.Choose(&g.obs, g.legal)
			if err := g.eng.Apply(p, a); err != nil {
				// A bot bug must not wedge the game: resign on its behalf.
				g.srv.log.Error("bot chose illegal action", "game", g.id, "bot", g.seats[p].botName, "action", a, "err", err)
				return append(events, g.resignSeat(p))
			}
			g.seq++
			g.srv.stats.actions.Add(1)
			events = protocol.AppendEvents(events, g.eng.Events())
			acted = true
		}
		if !acted {
			break
		}
	}
	if g.eng.Over() {
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.winner = g.eng.Winner()
	}
	return events
}

func (g *game) resign(c *Conn, id uint32) {
	p := g.seatOf(c.user.ID)
	if p < 0 {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_IN_GAME, g.id, "not a player in this game")
		return
	}
	switch g.status {
	case pb.GameStatus_GAME_STATUS_WAITING: // cancel an open game
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.publish(nil, reply{c, id})
	case pb.GameStatus_GAME_STATUS_ACTIVE:
		g.publish([]*pb.Event{g.resignSeat(p)}, reply{c, id})
	default:
		c.sendError(id, pb.ErrorCode_ERROR_CODE_GAME_UNAVAILABLE, g.id, "game is over")
	}
}

func (g *game) resignSeat(p int) *pb.Event {
	g.status = pb.GameStatus_GAME_STATUS_FINISHED
	g.resigned = true
	g.winner = 1 - p
	s := g.eng.Scores()
	return &pb.Event{Kind: pb.EventKind_EVENT_KIND_RESIGN, Seat: int32(p), Scores: []int32{int32(s[0]), int32(s[1])}}
}

func (g *game) sync(c *Conn, id uint32) {
	p := g.seatOf(c.user.ID)
	if p < 0 {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_IN_GAME, g.id, "not a player in this game")
		return
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_GameUpdate{GameUpdate: g.updateFor(p, nil, true)}})
}

// publish sends each human player an update with their own observation.
func (g *game) publish(events []*pb.Event, replies ...reply) {
	g.updateSummary()
	for p, s := range g.seats {
		if s.userID == 0 {
			continue
		}
		m := &pb.ServerMessage{Body: &pb.ServerMessage_GameUpdate{GameUpdate: g.updateFor(p, events, false)}}
		g.srv.pushUser(s.userID, m, replies)
	}
}

func (g *game) updateFor(p int, events []*pb.Event, full bool) *pb.GameUpdate {
	u := &pb.GameUpdate{Game: g.summaryFor(g.seats[p].userID), Seq: g.seq, Events: events, Full: full}
	if g.eng != nil {
		g.eng.ObserveInto(p, &g.obs)
		u.Observation = protocol.ObservationToPB(&g.obs)
		if g.status == pb.GameStatus_GAME_STATUS_ACTIVE {
			g.legal = g.eng.AppendLegal(p, g.legal[:0])
			u.Legal = protocol.ActionsToPB(g.legal)
		}
	}
	return u
}

func (g *game) updateSummary() {
	s := summary{status: g.status, seats: g.seats, winner: g.winner, resigned: g.resigned}
	if g.eng != nil {
		s.scores = g.eng.Scores()
		if g.status == pb.GameStatus_GAME_STATUS_ACTIVE {
			s.toAct = [2]bool{g.eng.ToAct(0), g.eng.ToAct(1)}
		}
	}
	g.smu.Lock()
	g.sum = s
	g.smu.Unlock()
}

// summaryFor describes the game from userID's point of view. Safe to call
// from any goroutine.
func (g *game) summaryFor(userID int64) *pb.GameSummary {
	g.smu.Lock()
	s := g.sum
	g.smu.Unlock()
	out := &pb.GameSummary{
		GameId:   g.id,
		Status:   s.status,
		Seat:     -1,
		Scores:   []int32{int32(s.scores[0]), int32(s.scores[1])},
		Winner:   int32(s.winner),
		Resigned: s.resigned,
	}
	for i, st := range s.seats {
		out.Players = append(out.Players, &pb.PlayerInfo{
			UserId:    st.userID,
			Nickname:  st.nickname,
			Bot:       st.botName,
			Connected: st.bot != nil || (st.userID != 0 && g.srv.connected(st.userID)),
		})
		if st.userID == userID && userID != 0 {
			out.Seat = int32(i)
			out.YourTurn = s.toAct[i]
		}
	}
	return out
}
