package expert

import (
	"slices"
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// oracleCheck wraps an oracle expert and, after each pegging decision,
// checks the worlds it searched against the opponent's real cards.
type oracleCheck struct {
	*Bot
	t      *testing.T
	g      *engine.Game
	seat   int
	checks int
}

func (c *oracleCheck) Watch(g *engine.Game, seat int) {
	c.g, c.seat = g, seat
	c.Bot.Watch(g, seat)
}

func (c *oracleCheck) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if o.Phase != engine.PhasePegging || len(legal) < 2 || o.OppHandN == 0 {
		return c.Bot.Choose(o, legal)
	}
	var opp engine.Observation
	c.g.ObserveInto(1-c.seat, &opp)
	a := c.Bot.Choose(o, legal)
	real := ranksOf(opp.HandCards())
	worlds := c.search.worlds
	switch c.search.oracle {
	case OracleHand:
		if len(worlds) != 1 || !slices.Equal(ranksOf8(worlds[0].ranks[:worlds[0].n]), real) {
			c.t.Fatalf("hand oracle searched %d worlds (first %v), real hand %v", len(worlds), worlds[0].ranks, real)
		}
	case OracleSix:
		found := false
		for _, w := range worlds {
			if slices.Equal(ranksOf8(w.ranks[:w.n]), real) {
				found = true
			}
			if len(worlds) > 15 {
				c.t.Fatalf("six oracle kept %d worlds", len(worlds))
			}
		}
		if !found {
			c.t.Fatalf("six oracle lost the real hand %v", real)
		}
	}
	c.checks++
	return a
}

func ranksOf(cs []engine.Card) []int {
	var r []int
	for _, c := range cs {
		r = append(r, c.Rank())
	}
	slices.Sort(r)
	return r
}

func ranksOf8(rs []int8) []int {
	var r []int
	for _, x := range rs {
		r = append(r, int(x))
	}
	slices.Sort(r)
	return r
}

func TestOracles(t *testing.T) {
	_, d := heuristicTables(t, 300)
	b := &Bundle{Win: &WinTable{}, Discard: d, KeepTemp: [2]float64{0.5, 0.5}}
	for _, o := range []Oracle{OracleHand, OracleSix} {
		c := &oracleCheck{Bot: NewBotWith(b, Options{Effort: 50, Oracle: o}), t: t}
		for i := range 20 {
			if _, err := bots.PlayGame(uint64(i), [2]bots.Bot{c, bots.NewHeuristic()}, nil); err != nil {
				t.Fatal(err)
			}
		}
		if c.checks < 100 {
			t.Errorf("oracle %d: only %d decisions checked", o, c.checks)
		}
	}
}
