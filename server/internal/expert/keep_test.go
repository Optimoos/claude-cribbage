package expert

import (
	"math"
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

func TestKeepModel(t *testing.T) {
	_, d := heuristicTables(t, 300)
	m := newKeepModel(d, [2]float64{0.5, 0.5}, nil, 0)
	// Probabilities over the options of a hand sum to one.
	six := [6]int8{1, 5, 5, 5, 11, 13}
	k := rank6(six)
	probs := m.probs[0][m.start[k]:m.start[k+1]]
	var sum float64
	for _, p := range probs {
		sum += float64(p)
	}
	if math.Abs(sum-1) > 1e-5 || len(probs) < 3 {
		t.Fatalf("options %v sum to %v", probs, sum)
	}
	// Keeping three fives and a king is the clear choice.
	keep := int16(shapeOf([4]engine.Card{engine.NewCard(5, 0), engine.NewCard(5, 1), engine.NewCard(5, 2), engine.NewCard(13, 0)}))
	if p := m.prob(0, [6]int8{1, 5, 5, 5, 9, 13}, keep); p < 0.5 {
		t.Errorf("P(keep 5-5-5-K) = %.3f", p)
	}
}

// rank6 numbers every six-rank shape differently, within its bound.
func TestRank6Dense(t *testing.T) {
	seen := map[int]bool{}
	forEachShape6(func(r [6]int8) {
		k := rank6(r)
		if k < 0 || k >= numShapes6 || seen[k] {
			t.Fatalf("rank6(%v) = %d, out of range or repeated", r, k)
		}
		seen[k] = true
	})
	if len(seen) != 18395 {
		t.Errorf("%d six-rank shapes, want 18395", len(seen))
	}
}

// The opponent model makes the opponent's real hand more likely than
// counting cards alone does.
func TestOpponentModelFindsTheRealHand(t *testing.T) {
	sp, d := heuristicTables(t, 1000)
	temps, _ := FitKeepTemps(d, sp.Details)
	plain := newSearcher(&WinTable{}, 0)
	model := newSearcher(&WinTable{}, 0)
	model.keep = newKeepModel(d, temps, nil, 0)
	model.pegTemp = DefaultPegTemp
	var gain float64
	n := 0
	botGames(t, bots.NewHeuristic(), 40, func(g *engine.Game, o *engine.Observation, _ engine.Action) {
		if o.OppHandN < 2 {
			return
		}
		var real [14]int
		for _, c := range g.Hand(1 - o.Player) {
			real[c.Rank()]++
		}
		share := func(s *searcher) float64 {
			var seen [52]bool
			for _, c := range o.Kept {
				seen[c] = true
			}
			for _, c := range o.Discards {
				seen[c] = true
			}
			seen[o.Starter] = true
			var oppPlayed []engine.Card
			for _, pc := range o.Played[:o.PlayedN] {
				seen[pc.Card] = true
				if pc.Player != o.Player {
					oppPlayed = append(oppPlayed, pc.Card)
				}
			}
			s.enumerate(o, &seen, oppPlayed)
			var total, mine float64
			for _, w := range s.reweight(o, oppPlayed, s.worlds) {
				total += w.weight
				var got [14]int
				for _, r := range w.ranks[:w.n] {
					got[r]++
				}
				if got == real {
					mine += w.weight
				}
			}
			return mine / total
		}
		gain += math.Log(share(model) / share(plain))
		n++
	})
	if gain/float64(n) <= 0.05 {
		t.Errorf("mean log gain in the real hand's probability %.3f over %d decisions", gain/float64(n), n)
	}
	t.Logf("mean log gain %.3f over %d decisions (temperatures %.2f, %.2f)", gain/float64(n), n, temps[0], temps[1])
}

// Suit classes cover every hand of a rank shape exactly once: their sizes
// add up to the product over ranks of (4 choose copies). (Over every shape
// they make 962,988 classes of the 20,358,520 hands; checked on a sample.)
func TestSuitClasses(t *testing.T) {
	i := 0
	forEachShape6(func(r [6]int8) {
		if i++; i%97 != 0 {
			return
		}
		var held [14]int
		for _, x := range r {
			held[x]++
		}
		want := 1
		for _, m := range held {
			want *= choose(4, m)
		}
		got := 0
		for _, c := range suitClasses(r) {
			got += c.n
		}
		if got != want {
			t.Fatalf("%v: classes cover %d hands, want %d", r, got, want)
		}
	})
}
