package expert

import (
	"math"
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

func TestKeepModel(t *testing.T) {
	_, d := heuristicTables(t, 300)
	m := newKeepModel(d, [2]float64{0.5, 0.5})
	// Probabilities over the options of a hand sum to one.
	six := [6]int8{1, 5, 5, 5, 11, 13}
	row := m.rows[0][key6(six)]
	var sum float64
	for _, p := range row.probs {
		sum += float64(p)
	}
	if math.Abs(sum-1) > 1e-5 || len(row.shapes) < 3 {
		t.Fatalf("row %+v sums to %v", row, sum)
	}
	// Keeping three fives and a king is the clear choice.
	keep := int16(shapeOf([4]engine.Card{engine.NewCard(5, 0), engine.NewCard(5, 1), engine.NewCard(5, 2), engine.NewCard(13, 0)}))
	if p := m.prob(0, [6]int8{1, 5, 5, 5, 9, 13}, keep); p < 0.5 {
		t.Errorf("P(keep 5-5-5-K) = %.3f", p)
	}
}

// The opponent model makes the opponent's real hand more likely than
// counting cards alone does.
func TestOpponentModelFindsTheRealHand(t *testing.T) {
	sp, d := heuristicTables(t, 1000)
	temps, _ := FitKeepTemps(d, sp.Details)
	plain := newSearcher(&WinTable{}, 0)
	model := newSearcher(&WinTable{}, 0)
	model.keep = newKeepModel(d, temps)
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
