package encode

import (
	"math/rand/v2"
	"testing"

	"cribbage/internal/ai/nn"
	"cribbage/internal/engine"
)

// TestAfterstateMatchesEngine checks, over many random games, that the
// afterstate computed from an observation agrees with the real engine on
// everything the observer can know.
func TestAfterstateMatchesEngine(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var x nn.Sparse
	for seed := uint64(0); seed < 3000; seed++ {
		g := engine.NewGame(seed)
		g.SetQuiet(true)
		for !g.Over() {
			p := 0
			if !g.ToAct(0) {
				p = 1
			}
			o := g.Observation(p)
			legal := g.Legal(p)
			a := legal[r.IntN(len(legal))]
			after, won := Afterstate(&o, a)
			before := g.Scores()
			if err := g.Apply(p, a); err != nil {
				t.Fatal(err)
			}
			real := g.Observation(p)

			// A win by pegging is certain the moment the card is played.
			if won && (!g.Over() || g.Winner() != p) {
				t.Fatalf("seed %d: predicted a win that did not happen", seed)
			}
			if a.Kind == engine.ActPlay && g.Over() && g.Winner() == p && !won {
				if pegOnly := after.Scores[p]; pegOnly >= engine.WinScore {
					t.Fatalf("seed %d: missed a pegging win", seed)
				}
			}
			if g.Over() || real.HandNum != o.HandNum {
				continue // the hand ended; later state depends on hidden cards
			}
			if after.HandN != real.HandN || after.Hand != real.Hand {
				t.Fatalf("seed %d: hand %v, engine %v", seed, after.HandCards(), real.HandCards())
			}
			if after.Discarded != real.Discarded || after.Kept != real.Kept || after.Discards != real.Discards {
				t.Fatalf("seed %d: discard state differs", seed)
			}
			if a.Kind == engine.ActPlay {
				// Our play and its points are certain; the opponent may
				// have replied (or a go may have been scored) since.
				if after.Played[after.PlayedN-1] != real.Played[o.PlayedN] {
					t.Fatalf("seed %d: played card differs", seed)
				}
				pegged := g.Scores()[p] - before[p]
				if gain := after.Scores[p] - before[p]; gain != pegged && gain != pegged-1 {
					t.Fatalf("seed %d: afterstate gained %d, engine %d", seed, gain, pegged)
				}
			}
			Encode(&after, &x)
			for k, i := range x.Idx {
				if int(i) < 0 || int(i) >= Size || x.Val[k] == 0 {
					t.Fatalf("bad feature %d=%v", i, x.Val[k])
				}
			}
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	g := engine.NewGame(1)
	o := g.Observation(0)
	var x nn.Sparse
	for i := 0; i < b.N; i++ {
		Encode(&o, &x)
	}
}
