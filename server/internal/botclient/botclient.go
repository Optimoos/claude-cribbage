// Package botclient runs a headless bot that plays many concurrent games
// on a server over the TCP transport. It reconnects and resumes its games
// if the connection drops or the server restarts.
package botclient

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"cribbage/internal/bots"
	"cribbage/internal/client"
	"cribbage/internal/engine"
	"cribbage/internal/protocol"
	"cribbage/internal/protocol/pb"
)

// Config for Run.
type Config struct {
	Addr     string // TCP host:port, or ws://host:port/rtc for WebRTC
	Username string
	Key      string // the server's bot key
	Nickname string
	Bot      string // local bot that makes decisions
	// Opponent is a server-side bot to play against; empty uses matchmaking
	// against other connected clients.
	Opponent   string
	Concurrent int // games in flight at once
	Games      int // stop after this many finished games; 0 runs until ctx ends
	Seed       uint64
	Workers    int           // decision goroutines; default NumCPU
	Reconnect  time.Duration // keep retrying a lost connection this long; default 30s, <0 disables
	Logger     *slog.Logger
}

// Stats summarizes a run.
type Stats struct {
	Finished   int
	Won        int
	Resigned   int
	Lost       int // games that vanished while disconnected with no record of the result
	Errors     int
	Reconnects int
	Elapsed    time.Duration
}

type work struct {
	gameID string
	reqID  uint32         // request id the message answers, if any
	upd    *pb.GameUpdate // nil with failed=false means the game ended
	failed bool           // the request reqID was rejected
}

type runner struct {
	cfg         Config
	st          Stats
	token       string
	active      map[string]bool // games in progress, kept across reconnects
	outstanding int             // game requests not yet answered by a new game
	ended       recentSet       // games already counted as finished
}

// recentSet remembers the last few thousand finished game IDs. A game list
// is assembled separately from game updates, so it can arrive after a
// game's final update and still show the game as active.
type recentSet struct {
	ids  map[string]bool
	ring []string
	next int
}

func (s *recentSet) add(id string) {
	const size = 4096
	if s.ids == nil {
		s.ids, s.ring = map[string]bool{}, make([]string, size)
	}
	delete(s.ids, s.ring[s.next])
	s.ring[s.next] = id
	s.ids[id] = true
	s.next = (s.next + 1) % size
}

func (s *recentSet) has(id string) bool { return s.ids[id] }

// Run plays games until cfg.Games have finished or ctx is cancelled.
func Run(ctx context.Context, cfg Config) (Stats, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.Concurrent <= 0 {
		cfg.Concurrent = 1
	}
	if cfg.Reconnect == 0 {
		cfg.Reconnect = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if _, err := bots.New(cfg.Bot, 0); err != nil {
		return Stats{}, err
	}
	r := &runner{cfg: cfg, active: map[string]bool{}}
	start := time.Now()
	defer func() { r.st.Elapsed = time.Since(start) }()

	for first := true; ; first = false {
		c, err := r.connect(ctx, first)
		if err != nil {
			r.st.Elapsed = time.Since(start)
			if ctx.Err() != nil {
				return r.st, nil
			}
			return r.st, err
		}
		done, err := r.session(ctx, c)
		c.Close()
		if done || ctx.Err() != nil {
			r.st.Elapsed = time.Since(start)
			return r.st, nil
		}
		if cfg.Reconnect < 0 {
			r.st.Elapsed = time.Since(start)
			return r.st, err
		}
		r.st.Reconnects++
		cfg.Logger.Warn("connection lost; reconnecting", "user", cfg.Username, "err", err)
	}
}

// connect dials and authenticates. Reconnects retry until cfg.Reconnect
// elapses, resuming with the session token where possible.
func (r *runner) connect(ctx context.Context, first bool) (*client.Client, error) {
	deadline := time.Now().Add(r.cfg.Reconnect)
	backoff := 50 * time.Millisecond
	for {
		c, err := r.dialAuth()
		if err == nil || first {
			return c, err
		}
		var se client.ServerError
		if errors.As(err, &se) || time.Now().After(deadline) {
			return nil, err // the server answered but refused us: don't retry
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff = min(2*backoff, time.Second)
	}
}

func (r *runner) dialAuth() (*client.Client, error) {
	c, err := client.Dial(r.cfg.Addr)
	if err != nil {
		return nil, err
	}
	var a *pb.AuthOk
	if r.token != "" {
		a, err = c.Resume(r.token)
		var se client.ServerError
		if errors.As(err, &se) && se.Code == pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS {
			a, err = c.BotLogin(r.cfg.Username, r.cfg.Nickname, r.cfg.Key)
		}
	} else {
		a, err = c.BotLogin(r.cfg.Username, r.cfg.Nickname, r.cfg.Key)
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("authenticate %s: %w", r.cfg.Username, err)
	}
	r.token = a.Token
	return c, nil
}

func (r *runner) finished() bool { return r.cfg.Games > 0 && r.st.Finished >= r.cfg.Games }

// session runs one connection. It reports done=true when the run is
// complete, otherwise the error that ended the connection.
func (r *runner) session(ctx context.Context, c *client.Client) (bool, error) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-stop:
		}
	}()

	// Workers each own a bot; a game always maps to the same worker so its
	// updates are handled in order.
	queues := make([]chan work, r.cfg.Workers)
	var wg sync.WaitGroup
	for i := range queues {
		b, _ := bots.New(r.cfg.Bot, r.cfg.Seed+uint64(i))
		queues[i] = make(chan work, 1024)
		wg.Add(1)
		go func(q chan work, b bots.Bot) {
			defer wg.Done()
			worker(c, q, b, r.cfg.Logger)
		}(queues[i], b)
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		wg.Wait()
	}()
	queueFor := func(id string) chan work {
		h := fnv.New32a()
		h.Write([]byte(id))
		return queues[h.Sum32()%uint32(len(queues))]
	}

	// Requests sent on a previous connection are gone (the server forgets
	// queued matches on restart). Reconcile our game list with the server's
	// before asking for more games.
	r.outstanding = 0
	listID, err := c.Send(&pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}})
	if err != nil {
		return false, err
	}
	listed := false

	for !r.finished() {
		m, err := c.Recv()
		if err != nil {
			return false, err
		}
		switch b := m.Body.(type) {
		case *pb.ServerMessage_GameList:
			if m.RequestId != listID {
				continue
			}
			listed = true
			onServer := map[string]bool{}
			ended := map[string]*pb.GameSummary{}
			for _, g := range b.GameList.Recent {
				ended[g.GameId] = g
			}
			for _, g := range b.GameList.Mine {
				switch g.Status {
				case pb.GameStatus_GAME_STATUS_ACTIVE:
					if !r.ended.has(g.GameId) {
						onServer[g.GameId] = true
						r.active[g.GameId] = true
					}
				case pb.GameStatus_GAME_STATUS_FINISHED:
					ended[g.GameId] = g // finishing right now; its final update may follow
				}
			}
			for id := range r.active {
				if onServer[id] {
					continue
				}
				// The game ended while we were disconnected.
				delete(r.active, id)
				r.ended.add(id)
				r.st.Finished++
				if g := ended[id]; g != nil {
					if g.Winner == g.Seat {
						r.st.Won++
					}
					if g.Resigned {
						r.st.Resigned++
					}
				} else {
					r.st.Lost++
				}
			}
			if err := r.topUp(c); err != nil {
				return false, err
			}
		case *pb.ServerMessage_Error:
			r.st.Errors++
			if r.st.Errors <= 10 {
				r.cfg.Logger.Warn("server error", "code", b.Error.Code, "msg", b.Error.Message, "game", b.Error.GameId)
			}
			if b.Error.GameId != "" {
				queueFor(b.Error.GameId) <- work{gameID: b.Error.GameId, reqID: m.RequestId, failed: true}
			}
		case *pb.ServerMessage_GameUpdate:
			u := b.GameUpdate
			id := u.Game.GameId
			switch u.Game.Status {
			case pb.GameStatus_GAME_STATUS_FINISHED:
				queueFor(id) <- work{gameID: id}
				if !r.active[id] {
					break // already counted when reconciling with the game list
				}
				delete(r.active, id)
				r.ended.add(id)
				r.st.Finished++
				if u.Game.Winner == u.Game.Seat {
					r.st.Won++
				}
				if u.Game.Resigned {
					r.st.Resigned++
				}
				if listed {
					if err := r.topUp(c); err != nil {
						return false, err
					}
				}
			case pb.GameStatus_GAME_STATUS_ACTIVE:
				if !r.active[id] {
					r.active[id] = true
					if r.outstanding > 0 {
						r.outstanding--
					}
				}
				queueFor(id) <- work{gameID: id, reqID: m.RequestId, upd: u}
			}
		}
	}
	return true, nil
}

// topUp requests new games until Concurrent are in flight or enough have
// been started to reach Games.
func (r *runner) topUp(c *client.Client) error {
	for len(r.active)+r.outstanding < r.cfg.Concurrent &&
		(r.cfg.Games == 0 || r.st.Finished+len(r.active)+r.outstanding < r.cfg.Games) {
		m := &pb.ClientMessage{Body: &pb.ClientMessage_FindMatch{FindMatch: &pb.FindMatch{}}}
		if r.cfg.Opponent != "" {
			m = &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: r.cfg.Opponent}}}
		}
		if _, err := c.Send(m); err != nil {
			return err
		}
		r.outstanding++
	}
	return nil
}

// worker makes decisions for the games assigned to it. After acting in a
// game it waits for the server's reply to that action before acting again:
// during the discard phase both players act at once, so an update caused by
// the opponent can still show this player as needing to act.
func worker(c *client.Client, q chan work, b bots.Bot, log *slog.Logger) {
	pending := map[string]uint32{} // game -> request id of our unanswered action
	var legal []engine.Action
	for w := range q {
		if w.upd == nil && !w.failed {
			delete(pending, w.gameID)
			continue
		}
		if p, ok := pending[w.gameID]; ok {
			if w.reqID != p {
				continue // not the answer to our action; state is stale
			}
			delete(pending, w.gameID)
		}
		if w.failed {
			// Our action was rejected; fetch fresh state and try again.
			// A send error means the connection is gone; keep draining the
			// queue so the receive loop never blocks on it.
			_, _ = c.Send(&pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: w.gameID}}})
			continue
		}
		if !w.upd.Observation.GetMustAct() || len(w.upd.Legal) == 0 {
			continue
		}
		o, err := protocol.ObservationFromPB(w.upd.Observation)
		if err != nil {
			log.Error("bad observation", "game", w.gameID, "err", err)
			continue
		}
		legal = legal[:0]
		for _, a := range w.upd.Legal {
			ea, err := protocol.ActionFromPB(a)
			if err != nil {
				log.Error("bad legal action", "game", w.gameID, "err", err)
				continue
			}
			legal = append(legal, ea)
		}
		if len(legal) == 0 {
			continue
		}
		a := b.Choose(&o, legal)
		id, err := c.Send(&pb.ClientMessage{Body: &pb.ClientMessage_Act{Act: &pb.Act{
			GameId: w.gameID, Action: protocol.ActionToPB(a),
		}}})
		if err == nil {
			pending[w.gameID] = id
		}
	}
}
