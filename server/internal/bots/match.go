package bots

import (
	"fmt"
	"io"

	"cribbage/internal/engine"
)

// Watcher is a bot that sees the whole game, hidden cards too. Only
// test oracles, which bound what perfect information is worth, do this;
// PlayGame shows a Watcher each game before it starts.
type Watcher interface {
	Watch(g *engine.Game, seat int)
}

// watcher finds a Watcher in b or the bots it wraps (those with an Unwrap
// method), or returns nil.
func watcher(b Bot) Watcher {
	for {
		if w, ok := b.(Watcher); ok {
			return w
		}
		u, ok := b.(interface{ Unwrap() Bot })
		if !ok {
			return nil
		}
		b = u.Unwrap()
	}
}

// Result summarizes a finished game.
type Result struct {
	Winner int
	Scores [2]int
	Hands  int
	Skunk  int    // 0 none, 1 skunk (loser < 91), 2 double skunk (loser < 61)
	Pegged [2]int // pegging points
}

// PlayGame plays one game between players[0] and players[1] using the rules
// engine directly. If trace is non-nil, a human-readable log including both
// hands is written to it.
func PlayGame(seed uint64, players [2]Bot, trace io.Writer) (Result, error) {
	return PlayFrom(engine.NewGame(seed), seed, players, trace)
}

// PlayFrom plays game g, which may start part way (see engine.NewHand),
// to the end; seed only labels errors.
func PlayFrom(g *engine.Game, seed uint64, players [2]Bot, trace io.Writer) (Result, error) {
	for p, b := range players {
		if w := watcher(b); w != nil {
			w.Watch(g, p)
		}
	}
	if trace == nil {
		g.SetQuiet(true)
	} else {
		writeEvents(trace, g, g.Events())
	}
	buf := make([]engine.Action, 0, 16)
	o := new(engine.Observation) // reused: passing it through the Bot interface would otherwise allocate per decision
	for !g.Over() {
		for p := 0; p < 2 && !g.Over(); p++ {
			if !g.ToAct(p) {
				continue
			}
			g.ObserveInto(p, o)
			buf = g.AppendLegal(p, buf[:0])
			a := players[p].Choose(o, buf)
			if err := g.Apply(p, a); err != nil {
				return Result{}, fmt.Errorf("seed %d: %s (P%d) chose %v: %w", seed, players[p].Name(), p, a, err)
			}
			if trace != nil {
				if a.Kind == engine.ActDiscard {
					fmt.Fprintf(trace, "  P%d %s\n", p, a)
				}
				writeEvents(trace, g, g.Events())
			}
		}
	}
	r := Result{Winner: g.Winner(), Scores: g.Scores(), Hands: g.HandNumber(), Pegged: g.Pegged()}
	switch loser := r.Scores[1-r.Winner]; {
	case loser < 61:
		r.Skunk = 2
	case loser < 91:
		r.Skunk = 1
	}
	return r, nil
}

func writeEvents(w io.Writer, g *engine.Game, evs []engine.Event) {
	for _, e := range evs {
		switch e.Kind {
		case engine.EvDeal:
			fmt.Fprintf(w, "\n== hand %d, scores %v, P%d deals\n", g.HandNumber(), e.Scores, e.Player)
			fmt.Fprintf(w, "  P0 dealt %v\n  P1 dealt %v\n", g.Hand(0), g.Hand(1))
		case engine.EvDiscard:
			// The discarded cards are printed by the caller.
		default:
			fmt.Fprintf(w, "  %v  %v\n", e, e.Scores)
		}
	}
}
