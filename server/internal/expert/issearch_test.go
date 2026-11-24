package expert

import (
	"math/rand/v2"
	"testing"
)

// With a single world the information-set search is minimax: every root
// play gets exactly the value the minimax search gives it.
func TestISSearchOneWorldIsMinimax(t *testing.T) {
	b, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		t.Skip(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	s := newSearcher(b.Win, 0)
	s.ends = endCaches.Get().(*endCache)
	defer endCaches.Put(s.ends)
	checked := 0
	for trial := 0; trial < 3000; trial++ {
		// Two random four-card rank hands, scores and a few plays in.
		var deck []int8
		for r := int8(1); r <= 13; r++ {
			for range 4 {
				deck = append(deck, r)
			}
		}
		rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
		var n pegNode
		copy(n.hand[0][:], deck[0:4])
		copy(n.hand[1][:], deck[4:8])
		n.n = [2]int8{4, 4}
		n.last = -1
		s.base = [2]int{rng.IntN(121), rng.IntN(121)}
		s.iDeal = rng.IntN(2) == 0
		s.myShow = rng.IntN(20)
		for c := range s.crib {
			s.crib[c] = 0
		}
		s.crib[rng.IntN(12)] = 1
		w := world{ranks: n.hand[1], n: 4, weight: 1, show: rng.IntN(20)}
		s.world = &w
		// Advance a few random plays, keeping it my turn.
		code := pegContinue
		for step := rng.IntN(4); step > 0 && code == pegContinue; step-- {
			var legal []int
			for i := 0; i < int(n.n[n.turn]); i++ {
				if n.count+rankValue(n.hand[n.turn][i]) <= 31 {
					legal = append(legal, i)
				}
			}
			code = n.apply(legal[rng.IntN(len(legal))], s.base)
		}
		if code != pegContinue || n.turn != 0 {
			continue
		}
		w.ranks, w.n = n.hand[1], n.n[1]
		worlds := []world{w}
		for i := 0; i < int(n.n[0]); i++ {
			if n.count+rankValue(n.hand[0][i]) > 31 {
				continue
			}
			s.newEnds()
			want := s.after(n, i, -1, 2)
			vals := make([]float64, 1)
			s.newEnds()
			s.isRoot(n, i, worlds, false, vals)
			if vals[0] != want {
				t.Fatalf("trial %d play %d: information-set %v, minimax %v (node %+v)", trial, i, vals[0], want, n)
			}
			checked++
		}
	}
	if checked < 2000 {
		t.Errorf("only %d positions checked", checked)
	}
}
