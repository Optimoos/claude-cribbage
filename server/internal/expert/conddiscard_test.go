package expert

import (
	"math"
	"math/rand/v2"
	"testing"

	"cribbage/internal/engine"
)

// The exact posterior pass agrees with sampling: deal the opponent six
// cards and the starter at random from those I cannot see, weight each of
// their keeps by the keep model, and average their show (and, as pone, my
// view of their show plus crib).
func TestCondPosteriorMatchesSampling(t *testing.T) {
	b, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		t.Skip(err)
	}
	keep := b.keepModel()
	rng := rand.New(rand.NewPCG(9, 10))
	cr := cribRank()
	for trial := 0; trial < 6; trial++ {
		deck := rng.Perm(52)
		var six [6]engine.Card
		for i := range six {
			six[i] = engine.Card(deck[i])
		}
		iDeal := trial%2 == 0
		oside := 0
		if iDeal {
			oside = 1
		}
		d := &discarder{keep: keep, post: new(condPost), iDeal: iDeal}
		d.post.posterior(keep, oside, six)
		var held [52]bool
		for _, c := range six {
			held[c] = true
		}
		kept := [4]engine.Card{six[0], six[1], six[2], six[3]}
		disc := [2]engine.Card{six[4], six[5]}
		d.condOption(kept, disc, &held, func(engine.Card) bool { return true }, true)
		var qMean, tMean, mass float64
		for q, p := range d.qmix {
			qMean += float64(q) * p
			mass += p
		}
		if math.Abs(mass-1) > 1e-9 {
			t.Fatalf("their show distribution sums to %v", mass)
		}
		if !iDeal {
			mass = 0
			for h := range d.theirsByH {
				for x, p := range d.theirsByH[h] {
					tMean += float64(x) * p
					mass += p
				}
			}
			if math.Abs(mass-1) > 1e-9 {
				t.Fatalf("pone joint sums to %v", mass)
			}
		}

		// Sampling.
		unseen := deck[6:]
		var sw, sq, st float64
		myPair := pairOf(disc[0], disc[1])
		for n := 0; n < 200000; n++ {
			for i := 0; i < 7; i++ {
				j := i + rng.IntN(len(unseen)-i)
				unseen[i], unseen[j] = unseen[j], unseen[i]
			}
			var r6 [6]int8
			for i := range r6 {
				r6[i] = int8(engine.Card(unseen[i]).Rank())
			}
			sorted := r6
			for i := 1; i < 6; i++ {
				for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
					sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
				}
			}
			s := engine.Card(unseen[6])
			sr := s.Rank()
			k := rank6(sorted)
			lo, hi := keep.start[k], keep.start[k+1]
			for oi := lo; oi < hi; oi++ {
				p := float64(keep.probs[oside][oi])
				shape := keep.shapes[oside][oi]
				kr := shapeRanks[shape]
				// The thrown pair: six less the kept ranks.
				var cnt [14]int
				for _, r := range sorted {
					cnt[r]++
				}
				jk := 0
				for _, r := range kr {
					cnt[r]--
					if r == 11 {
						jk++
					}
				}
				var pr []int
				for r := 1; r <= 13; r++ {
					for ; cnt[r] > 0; cnt[r]-- {
						pr = append(pr, r)
					}
				}
				pi := int(pairIdx[pr[0]][pr[1]])
				q := float64(handEVByShape[shape][sr-1])
				if sr != 11 {
					q += float64(jk) / 4
				}
				sw += p
				sq += p * q
				if !iDeal {
					c := float64(cr[myPair][pi][sr-1])
					if sr != 11 {
						c += float64(pairJacks[pi]) / 4
						if (disc[0].Rank() == 11 && disc[0].Suit() == s.Suit()) || (disc[1].Rank() == 11 && disc[1].Suit() == s.Suit()) {
							c++
						}
					} else {
						c += 2
					}
					st += p * (q + c)
				}
			}
		}
		sq /= sw
		st /= sw
		t.Logf("trial %d (deal %v): their show %.3f exact, %.3f sampled; pone total %.3f exact, %.3f sampled", trial, iDeal, qMean, sq, tMean, st)
		if math.Abs(qMean-sq) > 0.03 {
			t.Errorf("trial %d: their show mean %.4f, sampled %.4f", trial, qMean, sq)
		}
		if !iDeal && math.Abs(tMean-st) > 0.05 {
			t.Errorf("trial %d: pone total mean %.4f, sampled %.4f", trial, tMean, st)
		}
	}
}

// A shape-pair table that does not depend on the opponent's shape shifts
// nothing: every option values exactly as without it.
func TestCondPegConstantTableIsIdentity(t *testing.T) {
	b, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		t.Skip(err)
	}
	pp := &PegPairTable{Dealer: make([]float32, numShapes*numShapes), Pone: make([]float32, numShapes*numShapes)}
	for i := range pp.Dealer {
		pp.Dealer[i], pp.Pone[i] = 3.5, 2.5
	}
	b.PegPair = pp
	plain := NewBotWith(b, Options{Effort: 25, CondDiscard: true})
	paired := NewBotWith(b, Options{Effort: 25, CondDiscard: true, CondPeg: true})
	if paired.discard.pair == nil {
		t.Fatal("condpeg not set up")
	}
	rng := rand.New(rand.NewPCG(11, 12))
	for trial := 0; trial < 40; trial++ {
		deck := rng.Perm(52)
		var hands [2][6]engine.Card
		for i := 0; i < 12; i++ {
			hands[i/6][i%6] = engine.Card(deck[i])
		}
		g := engine.NewHand(1, trial%2, [2]int{rng.IntN(121), rng.IntN(121)}, hands, engine.Card(deck[12]))
		var o engine.Observation
		g.ObserveInto(0, &o)
		legal := g.Legal(0)
		plain.Choose(&o, legal)
		paired.Choose(&o, legal)
		// Choose returns its posterior to the pool; value needs one.
		for _, d := range []*discarder{plain.discard, paired.discard} {
			oside := 0
			if d.iDeal {
				oside = 1
			}
			d.post = new(condPost)
			d.post.posterior(d.keep, oside, hands[0])
		}
		var held [52]bool
		for _, c := range hands[0] {
			held[c] = true
		}
		for _, a := range legal {
			var kept [4]engine.Card
			n := 0
			for _, c := range hands[0] {
				if c != a.Cards[0] && c != a.Cards[1] {
					kept[n] = c
					n++
				}
			}
			x, y := plain.discard.value(kept, a.Cards, &held), paired.discard.value(kept, a.Cards, &held)
			if math.Abs(x-y) > 1e-12 {
				t.Fatalf("trial %d %v: cond %v, condpeg %v", trial, a, x, y)
			}
		}
	}
}
