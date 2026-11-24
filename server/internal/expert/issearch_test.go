package expert

import (
	"math"
	"math/rand/v2"
	"testing"
)

// refBlend values node n (player to move) as the blended search should with
// one world: my best play; the opponent's best reply at weight lambda and
// the policy's expectation at 1-lambda.
func refBlend(s *searcher, n pegNode, avail [14]int8, lambda float64) float64 {
	play := func(i int) float64 {
		a := avail
		a[n.hand[n.turn][i]]--
		c := n
		switch c.apply(i, s.base) {
		case pegWon:
			if c.turn == 0 {
				return 1
			}
			return 0
		case pegOver:
			return s.end(int(c.pts[0]), int(c.pts[1]))
		}
		return refBlend(s, c, a, lambda)
	}
	var tried uint16
	if n.turn == 0 {
		best := -1.0
		for i := 0; i < int(n.n[0]); i++ {
			r := n.hand[0][i]
			if tried&(1<<r) != 0 || n.count+rankValue(r) > 31 {
				continue
			}
			tried |= 1 << r
			best = max(best, play(i))
		}
		return best
	}
	lo, ev, z := 2.0, 0.0, 0.0
	for i := 0; i < int(n.n[1]); i++ {
		r := n.hand[1][i]
		if tried&(1<<r) != 0 || n.count+rankValue(r) > 31 {
			continue
		}
		tried |= 1 << r
		v := play(i)
		var f [nPegFeat]float64
		pegPublic(n.seq[:n.seqN], n.count, &avail, r, &f)
		var rest []int8
		rest = append(rest, n.hand[1][:i]...)
		rest = append(rest, n.hand[1][i+1:n.n[1]]...)
		pegPrivate(rest, n.count, r, &f)
		e := math.Exp(s.pegPol.dot(&f))
		lo, ev, z = min(lo, v), ev+e*v, z+e
	}
	return lambda*lo + (1-lambda)*ev/z
}

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

// With one world and a pegging policy, the blended search gives the
// recursive reference's value to every root play.
func TestISSearchBlendOneWorld(t *testing.T) {
	b, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		t.Skip(err)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	s := newSearcher(b.Win, 0)
	s.ends = endCaches.Get().(*endCache)
	defer endCaches.Put(s.ends)
	pol := pointsPolicy(1)
	pol[pfRisk], pol[pfPairHeld], pol[pfPlayable], pol[pfLeadFive], pol[pfFifteenHeld] = -0.8, 0.6, 1.1, -0.9, 0.3
	s.pegPol, s.isLambda = &pol, 0.3
	checked := 0
	for trial := 0; trial < 1500; trial++ {
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
		var avail [14]int8
		for r := 1; r <= 13; r++ {
			avail[r] = 4
		}
		avail[deck[8]]-- // the starter
		s.base = [2]int{rng.IntN(121), rng.IntN(121)}
		s.iDeal = rng.IntN(2) == 0
		s.myShow = rng.IntN(20)
		clear(s.crib[:])
		s.crib[rng.IntN(12)] = 1
		w := world{ranks: n.hand[1], n: 4, weight: 1, show: rng.IntN(20)}
		s.world = &w
		code := pegContinue
		for step := rng.IntN(4); step > 0 && code == pegContinue; step-- {
			var legal []int
			for i := 0; i < int(n.n[n.turn]); i++ {
				if n.count+rankValue(n.hand[n.turn][i]) <= 31 {
					legal = append(legal, i)
				}
			}
			i := legal[rng.IntN(len(legal))]
			avail[n.hand[n.turn][i]]--
			code = n.apply(i, s.base)
		}
		if code != pegContinue || n.turn != 0 {
			continue
		}
		w.ranks, w.n = n.hand[1], n.n[1]
		worlds := []world{w}
		s.isAvail = avail
		for i := 0; i < int(n.n[0]); i++ {
			if n.count+rankValue(n.hand[0][i]) > 31 {
				continue
			}
			s.newEnds()
			a := avail
			a[n.hand[0][i]]--
			c := n
			var want float64
			switch c.apply(i, s.base) {
			case pegWon:
				want = 1
			case pegOver:
				want = s.end(int(c.pts[0]), int(c.pts[1]))
			default:
				want = refBlend(s, c, a, 0.3)
			}
			vals := make([]float64, 1)
			s.newEnds()
			s.isRoot(n, i, worlds, false, vals)
			if math.Abs(vals[0]-want) > 1e-12 {
				t.Fatalf("trial %d play %d: search %v, reference %v (node %+v)", trial, i, vals[0], want, n)
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Errorf("only %d positions checked", checked)
	}
}
