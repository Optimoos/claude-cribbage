package server

import (
	"context"
	"fmt"
	"time"

	"cribbage/internal/protocol/pb"
)

// reservation counts games being created for a user that are not yet
// registered.
type reservation struct{ games, bots, open int }

// gameKind is what a new game counts against.
type gameKind struct{ bot, open bool }

// limitError says which player is at which limit.
type limitError struct{ msg string }

func (e *limitError) Error() string { return e.msg }

// reserve checks that each of users (by user ID, with names for messages)
// may take part in another game of kind, and holds a place for it. Call
// release once the game is registered or creation has failed.
func (s *Server) reserve(kind gameKind, users ...reserveFor) (release func(), err error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	for _, u := range users {
		games, bots, open := s.countGames(u.id)
		games += u.extra
		who := "You have"
		if !u.self {
			who = u.name + " has"
		}
		switch {
		case games >= s.limits.Games:
			return nil, &limitError{fmt.Sprintf("%s %d games in progress, the most allowed; finish or resign one first", who, games)}
		case kind.bot && bots >= s.limits.BotGames:
			return nil, &limitError{fmt.Sprintf("%s %d games against the computer, the most allowed; finish or resign one first", who, bots)}
		case kind.open && open >= s.limits.Open:
			return nil, &limitError{fmt.Sprintf("%s an open game already; cancel it or wait for someone to join", who)}
		}
	}
	for _, u := range users {
		r := s.reserved[u.id]
		r.games++
		if kind.bot {
			r.bots++
		}
		if kind.open {
			r.open++
		}
		s.reserved[u.id] = r
	}
	return func() {
		s.gmu.Lock()
		defer s.gmu.Unlock()
		for _, u := range users {
			r := s.reserved[u.id]
			r.games--
			if kind.bot {
				r.bots--
			}
			if kind.open {
				r.open--
			}
			if r == (reservation{}) {
				delete(s.reserved, u.id)
			} else {
				s.reserved[u.id] = r
			}
		}
	}, nil
}

type reserveFor struct {
	id   int64
	name string
	self bool // the requester: "You have…" rather than "Name has…"
	// extra counts toward the game limit only: games the user is waiting
	// for, such as matchmaking requests.
	extra int
}

// countGames counts a user's unfinished games, those against bots, and
// their open games, including reserved places. Call with gmu held.
func (s *Server) countGames(userID int64) (games, bots, open int) {
	for _, g := range s.userGames[userID] {
		g.smu.Lock()
		sum := g.sum
		g.smu.Unlock()
		if sum.status == pb.GameStatus_GAME_STATUS_FINISHED {
			continue
		}
		games++
		if sum.seats[0].botName != "" || sum.seats[1].botName != "" {
			bots++
		}
		if sum.status == pb.GameStatus_GAME_STATUS_WAITING {
			open++
		}
	}
	r := s.reserved[userID]
	return games + r.games, bots + r.bots, open + r.open
}

// sendLimit reports err to c: a limit as TOO_MANY_GAMES, anything else as
// an internal error.
func (s *Server) sendLimit(c *Conn, id uint32, err error) {
	if le, ok := err.(*limitError); ok {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES, "", le.msg)
		return
	}
	c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
}

// sweep periodically ends games that have waited too long.
func (s *Server) sweep() {
	t := time.NewTicker(s.limits.SweepEvery)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			s.expireGames(now)
			s.expireInvites(now)
			s.pruneSessions()
		case <-s.quit:
			return
		}
	}
}

func (s *Server) pruneSessions() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if n, err := s.auth.Prune(ctx); err != nil {
		s.log.Error("delete expired sessions", "err", err)
	} else if n > 0 {
		s.log.Info("deleted expired sessions", "count", n)
	}
}

// expireGames ends open games nobody joined and games without a move for
// too long.
func (s *Server) expireGames(now time.Time) {
	var due []*game
	s.gmu.RLock()
	for _, g := range s.games {
		g.smu.Lock()
		sum := g.sum
		g.smu.Unlock()
		if now.After(g.deadline(sum)) {
			due = append(due, g)
		}
	}
	s.gmu.RUnlock()
	for _, g := range due {
		g.submit(func() { g.expire(now) })
	}
}

// deadline is when g expires if nothing happens before then.
func (g *game) deadline(sum summary) time.Time {
	last := time.UnixMilli(g.lastMove.Load())
	l := g.srv.limits
	switch {
	case sum.status == pb.GameStatus_GAME_STATUS_WAITING:
		return last.Add(l.OpenFor)
	case sum.seats[0].botName != "" || sum.seats[1].botName != "":
		return last.Add(l.BotIdle)
	default:
		return last.Add(l.HumanIdle)
	}
}

// expire ends the game unfinished if it is still past its deadline at now.
func (g *game) expire(now time.Time) {
	g.updateSummary()
	g.smu.Lock()
	sum := g.sum
	g.smu.Unlock()
	if !now.After(g.deadline(sum)) {
		return // a move came in meanwhile
	}
	snap := g.snapshot()
	var events []*pb.Event
	switch g.status {
	case pb.GameStatus_GAME_STATUS_WAITING:
		g.srv.log.Info("open game expired", "game", g.id)
	case pb.GameStatus_GAME_STATUS_ACTIVE:
		g.srv.log.Info("game expired", "game", g.id)
		s := g.eng.Scores()
		events = []*pb.Event{{Kind: pb.EventKind_EVENT_KIND_EXPIRED, Seat: -1, Scores: []int32{int32(s[0]), int32(s[1])}}}
	default:
		return
	}
	g.status = pb.GameStatus_GAME_STATUS_FINISHED
	g.winner = -1
	g.dirty = true
	g.commit(snap, events)
}
