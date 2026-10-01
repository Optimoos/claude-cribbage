// Package botclient runs a headless bot that plays many concurrent games
// on a server over the TCP transport.
package botclient

import (
	"context"
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
	Addr     string
	Username string
	Password string
	Nickname string
	Bot      string // local bot that makes decisions
	// Opponent is a server-side bot to play against; empty uses matchmaking
	// against other connected clients.
	Opponent   string
	Concurrent int // games in flight at once
	Games      int // stop after this many finished games; 0 runs until ctx ends
	Seed       uint64
	Workers    int // decision goroutines; default NumCPU
	Logger     *slog.Logger
}

// Stats summarizes a run.
type Stats struct {
	Finished int
	Won      int
	Resigned int
	Errors   int
	Elapsed  time.Duration
}

type work struct {
	gameID string
	reqID  uint32         // request id the message answers, if any
	upd    *pb.GameUpdate // nil with failed=false means the game ended
	failed bool           // the request reqID was rejected
}

// Run plays games until cfg.Games have finished or ctx is cancelled.
func Run(ctx context.Context, cfg Config) (Stats, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.Concurrent <= 0 {
		cfg.Concurrent = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	start := time.Now()
	var st Stats

	c, err := client.Dial(cfg.Addr)
	if err != nil {
		return st, err
	}
	defer c.Close()
	if _, err := c.Login(cfg.Username, cfg.Password, cfg.Nickname, true); err != nil {
		return st, fmt.Errorf("login %s: %w", cfg.Username, err)
	}
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
	queues := make([]chan work, cfg.Workers)
	var wg sync.WaitGroup
	for i := range queues {
		b, err := bots.New(cfg.Bot, cfg.Seed+uint64(i))
		if err != nil {
			return st, err
		}
		queues[i] = make(chan work, 1024)
		wg.Add(1)
		go func(q chan work, b bots.Bot) {
			defer wg.Done()
			worker(c, q, b, cfg.Logger)
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

	requested := 0
	request := func() error {
		if cfg.Games > 0 && requested >= cfg.Games {
			return nil
		}
		requested++
		m := &pb.ClientMessage{Body: &pb.ClientMessage_FindMatch{FindMatch: &pb.FindMatch{}}}
		if cfg.Opponent != "" {
			m = &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: cfg.Opponent}}}
		}
		_, err := c.Send(m)
		return err
	}
	for i := 0; i < cfg.Concurrent; i++ {
		if err := request(); err != nil {
			return st, err
		}
	}

	for cfg.Games == 0 || st.Finished < cfg.Games {
		m, err := c.Recv()
		if err != nil {
			st.Elapsed = time.Since(start)
			if ctx.Err() != nil {
				return st, nil
			}
			return st, err
		}
		switch b := m.Body.(type) {
		case *pb.ServerMessage_Error:
			st.Errors++
			if st.Errors <= 10 {
				cfg.Logger.Warn("server error", "code", b.Error.Code, "msg", b.Error.Message, "game", b.Error.GameId)
			}
			if b.Error.GameId != "" {
				queueFor(b.Error.GameId) <- work{gameID: b.Error.GameId, reqID: m.RequestId, failed: true}
			}
		case *pb.ServerMessage_GameUpdate:
			u := b.GameUpdate
			id := u.Game.GameId
			switch u.Game.Status {
			case pb.GameStatus_GAME_STATUS_FINISHED:
				st.Finished++
				if u.Game.Winner == u.Game.Seat {
					st.Won++
				}
				if u.Game.Resigned {
					st.Resigned++
				}
				queueFor(id) <- work{gameID: id}
				if err := request(); err != nil {
					return st, err
				}
			case pb.GameStatus_GAME_STATUS_ACTIVE:
				queueFor(id) <- work{gameID: id, reqID: m.RequestId, upd: u}
			}
		}
	}
	st.Elapsed = time.Since(start)
	return st, nil
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
			if _, err := c.Send(&pb.ClientMessage{Body: &pb.ClientMessage_SyncGame{SyncGame: &pb.SyncGame{GameId: w.gameID}}}); err != nil {
				return
			}
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
		if err != nil {
			return
		}
		pending[w.gameID] = id
	}
}
