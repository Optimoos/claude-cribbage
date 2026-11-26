package expert

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"cribbage/internal/engine"
)

// PegPairTable is the mean pegging of each pair of kept rank shapes
// (proposal 4, second part): for the dealer keeping shape d and the pone
// keeping shape p, Dealer[d*numShapes+p] and Pone[d*numShapes+p] are the
// points each pegs on average, played from 0-0 with random suits,
// discards and starter. Impossible pairs (a fifth card of a rank) are 0.
type PegPairTable struct {
	Dealer, Pone []float32
}

var pegPairShape = []int{2, numShapes, numShapes}

// BuildPegPair plays samples hands of every shape pair with two players
// made by newBot (each worker makes its own pair), and returns the means.
// progress, if not nil, is called with the dealer shapes done so far.
func BuildPegPair(ctx context.Context, newBot func() [2]Choosers, samples, workers int, seed uint64, progress func(done int)) (*PegPairTable, error) {
	handEVTable()
	t := &PegPairTable{Dealer: make([]float32, numShapes*numShapes), Pone: make([]float32, numShapes*numShapes)}
	var next, done atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			players := newBot()
			var o engine.Observation
			buf := make([]engine.Action, 0, 16)
			for ctx.Err() == nil {
				d := int(next.Add(1) - 1)
				if d >= numShapes {
					return
				}
				for p := 0; p < numShapes; p++ {
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
						continue
					}
					var sd, sp float64
					for s := 0; s < samples; s++ {
						rng := rand.New(rand.NewPCG(seed^uint64(d*numShapes+p), uint64(s)))
						g := pairHand(rng, d, p)
						for !g.Over() && g.HandNumber() == 1 {
							for seat := 0; seat < 2; seat++ {
								if g.Over() || g.HandNumber() != 1 || !g.ToAct(seat) {
									continue
								}
								g.ObserveInto(seat, &o)
								buf = g.AppendLegal(seat, buf[:0])
								a := buf[0]
								if len(buf) > 1 {
									a = players[seat].Choose(&o, buf)
								}
								if err := g.Apply(seat, a); err != nil {
									panic(err)
								}
							}
						}
						pg := g.Pegged()
						sd += float64(pg[0])
						sp += float64(pg[1])
					}
					t.Dealer[d*numShapes+p] = float32(sd / float64(samples))
					t.Pone[d*numShapes+p] = float32(sp / float64(samples))
				}
				if n := done.Add(1); progress != nil {
					progress(int(n))
				}
			}
		}()
	}
	wg.Wait()
	return t, ctx.Err()
}

// Choosers is anything that picks an action: a bot.
type Choosers interface {
	Choose(o *engine.Observation, legal []engine.Action) engine.Action
}

// pairHand deals a hand at 0-0 with seat 0 dealing, the dealer keeping
// shape d and the pone shape p in random suits, each throwing two random
// other cards, and a random starter; the discards are made, so the hand
// is ready to peg (the pone leads).
func pairHand(rng *rand.Rand, d, p int) *engine.Game {
	var used [52]bool
	var kept [2][4]engine.Card
	var suits [14][4]int
	var taken [14]int
	for r := 1; r <= 13; r++ {
		suits[r] = [4]int{0, 1, 2, 3}
		rng.Shuffle(4, func(i, j int) { suits[r][i], suits[r][j] = suits[r][j], suits[r][i] })
	}
	for seat, sh := range [2]int{d, p} {
		for i, r := range shapeRanks[sh] {
			c := engine.NewCard(int(r), engine.Suit(suits[r][taken[r]]))
			taken[r]++
			kept[seat][i] = c
			used[c] = true
		}
	}
	var rest []engine.Card
	for c := engine.Card(0); c < 52; c++ {
		if !used[c] {
			rest = append(rest, c)
		}
	}
	for i := 0; i < 5; i++ {
		j := i + rng.IntN(len(rest)-i)
		rest[i], rest[j] = rest[j], rest[i]
	}
	var hands [2][6]engine.Card
	for seat := 0; seat < 2; seat++ {
		copy(hands[seat][:4], kept[seat][:])
		hands[seat][4], hands[seat][5] = rest[2*seat], rest[2*seat+1]
	}
	g := engine.NewHand(rng.Uint64(), 0, [2]int{}, hands, rest[4])
	g.SetQuiet(true)
	for seat := 0; seat < 2; seat++ {
		if err := g.Apply(seat, engine.Discard(hands[seat][4], hands[seat][5])); err != nil {
			panic(err)
		}
	}
	return g
}
