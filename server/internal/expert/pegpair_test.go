package expert

import (
	"math/rand/v2"
	"testing"
	"time"

	"cribbage/internal/engine"
)

// pairHand deals the shapes asked for, ready to peg, and a hand played
// from it pegs plausible totals.
func TestPairHand(t *testing.T) {
	b, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		t.Skip(err)
	}
	handEVTable()
	bots := [2]*Bot{NewBot(b, 25), NewBot(b, 25)}
	rng := rand.New(rand.NewPCG(1, 1))
	var o engine.Observation
	t0 := time.Now()
	var tot [2]float64
	n := 300
	for i := 0; i < n; i++ {
		d, p := rng.IntN(numShapes), rng.IntN(numShapes)
		var cnt [14]int
		ok := true
		for _, r := range shapeRanks[d] {
			cnt[r]++
		}
		for _, r := range shapeRanks[p] {
			if cnt[r]++; cnt[r] > 4 {
				ok = false
			}
		}
		if !ok {
			i--
			continue
		}
		g := pairHand(rng, d, p)
		for seat := 0; seat < 2; seat++ {
			if shapeOf(g.Kept(seat)) != []int{d, p}[seat] {
				t.Fatalf("seat %d kept %v, want shape %v", seat, g.Kept(seat), shapeRanks[[]int{d, p}[seat]])
			}
		}
		for !g.Over() && g.HandNumber() == 1 {
			for seat := 0; seat < 2; seat++ {
				if g.Over() || g.HandNumber() != 1 || !g.ToAct(seat) {
					continue
				}
				g.ObserveInto(seat, &o)
				legal := g.Legal(seat)
				a := legal[0]
				if len(legal) > 1 {
					a = bots[seat].Choose(&o, legal)
				}
				if err := g.Apply(seat, a); err != nil {
					t.Fatal(err)
				}
			}
		}
		pg := g.Pegged()
		tot[0] += float64(pg[0])
		tot[1] += float64(pg[1])
	}
	el := time.Since(t0)
	t.Logf("%d hands in %v (%v each); mean pegging dealer %.2f, pone %.2f", n, el, el/time.Duration(n), tot[0]/float64(n), tot[1]/float64(n))
	if m := tot[0] / float64(n); m < 2.5 || m > 6 {
		t.Errorf("dealer pegs %.2f on average", m)
	}
}
