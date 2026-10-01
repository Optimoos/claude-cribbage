package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
	"cribbage/internal/protocol"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

type seat struct {
	userID   int64 // 0 for bots and empty seats
	nickname string
	botName  string
	bot      bots.Bot
}

// game is an actor. Fields below "owned by the actor" may only be touched
// by commands running on its goroutine (see submit).
//
// Every command that changes the game commits the change to the GameStore
// before publishing it (write-ahead), so a client never sees a state that a
// restart would lose. If the commit fails the change is rolled back.
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
	created  time.Time
	unsaved  []store.Action // applied but not yet committed
	dirty    bool           // header changed since the last commit
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
	defer g.srv.actors.Done()
	defer close(g.done)
	for {
		select {
		case f := <-g.inbox:
			f()
			if g.status == pb.GameStatus_GAME_STATUS_FINISHED {
				g.srv.removeGame(g)
				g.srv.stats.gamesFinished.Add(1)
				return
			}
		case <-g.srv.quit:
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

func (g *game) record() store.Game {
	r := store.Game{
		ID: g.id, Seed: g.seed, Status: int(g.status), Winner: g.winner, Resigned: g.resigned,
		Created: g.created, Updated: time.Now(),
	}
	for i, s := range g.seats {
		r.Seats[i] = store.Seat{UserID: s.userID, Nickname: s.nickname, Bot: s.botName}
	}
	if g.eng != nil {
		r.Scores = g.eng.Scores()
	}
	return r
}

// snapshot captures the mutable state so a failed commit can be undone.
type snapshot struct {
	eng      *engine.Game
	seats    [2]seat
	seq      uint32
	status   pb.GameStatus
	winner   int
	resigned bool
	dirty    bool
}

func (g *game) snapshot() snapshot {
	s := snapshot{seats: g.seats, seq: g.seq, status: g.status, winner: g.winner, resigned: g.resigned, dirty: g.dirty}
	if g.eng != nil {
		s.eng = g.eng.Clone()
	}
	return s
}

func (g *game) rollback(s snapshot) {
	g.eng, g.seats, g.seq, g.status, g.winner, g.resigned, g.dirty = s.eng, s.seats, s.seq, s.status, s.winner, s.resigned, s.dirty
	g.unsaved = g.unsaved[:0]
}

// save commits unsaved actions and header changes.
func (g *game) save() error {
	var hdr *store.Game
	if g.dirty {
		r := g.record()
		hdr = &r
	}
	if hdr == nil && len(g.unsaved) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := g.srv.gameStore.CommitGame(ctx, g.id, hdr, g.unsaved); err != nil {
		return err
	}
	g.unsaved = g.unsaved[:0]
	g.dirty = false
	return nil
}

// commit saves and publishes, or rolls back to snap and reports an error to
// the requesters. It reports whether the change was kept.
func (g *game) commit(snap snapshot, events []*pb.Event, replies ...reply) bool {
	if err := g.save(); err != nil {
		g.srv.log.Error("commit failed", "game", g.id, "err", err)
		g.rollback(snap)
		for _, r := range replies {
			r.conn.sendError(r.id, pb.ErrorCode_ERROR_CODE_INTERNAL, g.id, "could not save game")
		}
		return false
	}
	g.publish(events, replies...)
	return true
}

// apply performs an action and records it for the next commit.
func (g *game) apply(p int, a engine.Action) error {
	if err := g.eng.Apply(p, a); err != nil {
		return err
	}
	g.seq++
	g.unsaved = append(g.unsaved, store.Action{Seq: g.seq, Seat: p, Action: a})
	g.srv.stats.actions.Add(1)
	return nil
}

// start deals the first hand and lets bots act.
func (g *game) start(replies ...reply) {
	snap := g.snapshot()
	g.eng = engine.NewGame(g.seed)
	if g.status != pb.GameStatus_GAME_STATUS_ACTIVE {
		g.status = pb.GameStatus_GAME_STATUS_ACTIVE
		g.dirty = true
	}
	events := protocol.AppendEvents(nil, g.eng.Events())
	events = g.driveBots(events)
	if !g.commit(snap, events, replies...) {
		// A game that cannot be saved cannot be played; drop it.
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		return
	}
	g.srv.stats.gamesStarted.Add(1)
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
	snap := g.snapshot()
	g.seats[1] = seat{userID: c.user.ID, nickname: c.user.Nickname}
	g.dirty = true
	g.eng = engine.NewGame(g.seed)
	g.status = pb.GameStatus_GAME_STATUS_ACTIVE
	if err := g.save(); err != nil {
		g.srv.log.Error("commit failed", "game", g.id, "err", err)
		g.rollback(snap)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, g.id, "could not save game")
		return
	}
	g.srv.gameJoined(g, c.user.ID)
	g.srv.stats.gamesStarted.Add(1)
	g.publish(protocol.AppendEvents(nil, g.eng.Events()), reply{c, id})
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
	snap := g.snapshot()
	if err := g.apply(p, action); err != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ILLEGAL_ACTION, g.id, err.Error())
		return
	}
	events := protocol.AppendEvents(nil, g.eng.Events())
	events = g.driveBots(events)
	g.commit(snap, events, reply{c, id})
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
			if err := g.apply(p, a); err != nil {
				// A bot bug must not wedge the game: resign on its behalf.
				g.srv.log.Error("bot chose illegal action", "game", g.id, "bot", g.seats[p].botName, "action", a, "err", err)
				return append(events, g.resignSeat(p))
			}
			events = protocol.AppendEvents(events, g.eng.Events())
			acted = true
		}
		if !acted {
			break
		}
	}
	if g.eng.Over() && g.status != pb.GameStatus_GAME_STATUS_FINISHED {
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.winner = g.eng.Winner()
		g.dirty = true
	}
	return events
}

// resume runs after a restart: bots may owe a move that was never made.
func (g *game) resume() {
	if g.status != pb.GameStatus_GAME_STATUS_ACTIVE {
		return
	}
	snap := g.snapshot()
	events := g.driveBots(nil)
	if len(g.unsaved) > 0 || g.dirty {
		g.commit(snap, events)
	}
}

func (g *game) resign(c *Conn, id uint32) {
	p := g.seatOf(c.user.ID)
	if p < 0 {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_IN_GAME, g.id, "not a player in this game")
		return
	}
	snap := g.snapshot()
	switch g.status {
	case pb.GameStatus_GAME_STATUS_WAITING: // cancel an open game
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.dirty = true
		g.commit(snap, nil, reply{c, id})
	case pb.GameStatus_GAME_STATUS_ACTIVE:
		g.commit(snap, []*pb.Event{g.resignSeat(p)}, reply{c, id})
	default:
		c.sendError(id, pb.ErrorCode_ERROR_CODE_GAME_UNAVAILABLE, g.id, "game is over")
	}
}

func (g *game) resignSeat(p int) *pb.Event {
	g.status = pb.GameStatus_GAME_STATUS_FINISHED
	g.resigned = true
	g.winner = 1 - p
	g.dirty = true
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

// presenceChanged tells the other players that userID connected or left.
func (g *game) presenceChanged(userID int64) {
	g.updateSummary()
	for p, s := range g.seats {
		if s.userID == 0 || s.userID == userID {
			continue
		}
		m := &pb.ServerMessage{Body: &pb.ServerMessage_GameUpdate{GameUpdate: g.updateFor(p, nil, false)}}
		g.srv.pushUser(s.userID, m, nil)
	}
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

// restoreGame rebuilds a game from its stored record by replaying actions.
func (s *Server) restoreGame(rec store.GameWithActions) (*game, error) {
	r := rec.Game
	g := &game{
		id:       r.ID,
		srv:      s,
		inbox:    make(chan func(), 64),
		done:     make(chan struct{}),
		seed:     r.Seed,
		status:   pb.GameStatus(r.Status),
		winner:   r.Winner,
		resigned: r.Resigned,
		created:  r.Created,
	}
	for i, st := range r.Seats {
		g.seats[i] = seat{userID: st.UserID, nickname: st.Nickname, botName: st.Bot}
		if st.Bot != "" {
			b, err := bots.New(st.Bot, newSeed())
			if err != nil {
				return nil, err
			}
			g.seats[i].bot = b
		}
	}
	if g.status == pb.GameStatus_GAME_STATUS_ACTIVE {
		g.eng = engine.NewGame(r.Seed)
		g.eng.SetQuiet(true)
		for _, a := range rec.Actions {
			if err := g.eng.Apply(a.Seat, a.Action); err != nil {
				return nil, fmt.Errorf("replaying action %d (%v by seat %d): %w", a.Seq, a.Action, a.Seat, err)
			}
		}
		g.eng.SetQuiet(false)
		g.seq = uint32(len(rec.Actions))
	} else if len(rec.Actions) > 0 {
		return nil, fmt.Errorf("waiting game has %d actions", len(rec.Actions))
	}
	g.updateSummary()
	return g, nil
}
