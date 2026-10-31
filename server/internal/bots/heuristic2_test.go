package bots

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"cribbage/internal/engine"
)

// The pone throws poor cards to the dealer's crib and the dealer good ones
// to their own, so my discards are worth less than in a random crib when I
// deal and more when I am the pone.
func TestH2CribBySide(t *testing.T) {
	h2Once.Do(buildH2)
	var dealer, pone, random float64
	for r1 := 1; r1 <= 13; r1++ {
		for r2 := r1; r2 <= 13; r2++ {
			for sr := 0; sr < 13; sr++ {
				dealer += h2CribMean[0][r1][r2][sr] / 13
				pone += h2CribMean[1][r1][r2][sr] / 13
			}
			random += CribEV(engine.NewCard(r1, engine.Clubs), engine.NewCard(r2, engine.Diamonds))
		}
	}
	if !(dealer < random && random < pone) {
		t.Errorf("summed crib means: dealer's %.1f, random %.1f, pone's %.1f", dealer, random, pone)
	}
}

func TestH2WinTable(t *testing.T) {
	h2Once.Do(buildH2)
	if v := h2V[0][0]; v < 0.54 || v > 0.58 {
		t.Errorf("dealer wins %.4f from 0-0, want about 0.56", v)
	}
	const eps = 1e-9
	for x := 0; x < engine.WinScore; x++ {
		for y := 0; y < engine.WinScore; y++ {
			v := h2V[x][y]
			if v < 0 || v > 1 {
				t.Fatalf("V[%d][%d] = %v", x, y, v)
			}
			if x+1 < engine.WinScore && h2V[x+1][y] < v-eps {
				t.Errorf("V[%d][%d] = %.6f < V[%d][%d] = %.6f", x+1, y, h2V[x+1][y], x, y, v)
			}
			if y+1 < engine.WinScore && h2V[x][y+1] > v+eps {
				t.Errorf("V[%d][%d] = %.6f > V[%d][%d] = %.6f", x, y+1, h2V[x][y+1], x, y, v)
			}
		}
	}
}

// guaranteed reports whether kept shows at least pts with every starter not
// in held.
func guaranteed(kept [4]engine.Card, held *[52]bool, pts int) bool {
	for s := engine.Card(0); s < 52; s++ {
		if !held[s] && engine.ScoreHand(kept, s, false).Total() < pts {
			return false
		}
	}
	return true
}

// The pone on 115 counts first, so a keep that always shows 6 wins unless
// the dealer pegs 21: it beats any keep with more points on average.
func TestHeuristic2KeepsASureSix(t *testing.T) {
	h := NewHeuristic2()
	rng := rand.New(rand.NewPCG(5, 6))
	deck := make([]engine.Card, 52)
	for i := range deck {
		deck[i] = engine.Card(i)
	}
	tested, differ := 0, 0
	for tested < 200 {
		rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
		o := discardObs("", false)
		copy(o.Hand[:], deck[:6])
		o.HandN = 6
		o.Scores = [2]int{115, 100}
		var held [52]bool
		for _, c := range deck[:6] {
			held[c] = true
		}
		legal := legalDiscards(&o)
		hand := o.HandCards()
		anySure := false
		for _, a := range legal {
			anySure = anySure || guaranteed(keptAfter(hand, a), &held, 6)
		}
		if !anySure {
			continue
		}
		tested++
		a := h.Choose(&o, legal)
		if !guaranteed(keptAfter(hand, a), &held, 6) {
			t.Errorf("hand %v: kept %v, which can show under 6", hand, keptAfter(hand, a))
		}
		if b := NewHeuristic().Choose(&o, legal); !guaranteed(keptAfter(hand, b), &held, 6) {
			differ++
		}
	}
	if differ == 0 {
		t.Error("heuristic always kept a sure 6 as well; the test shows nothing")
	}
	t.Logf("%d hands, heuristic risked the 6 in %d", tested, differ)
}

func TestHeuristic2Lookahead(t *testing.T) {
	h := NewHeuristic2()
	for _, tc := range []struct {
		name              string
		seq, rest, unseen string
		k                 int
		want              float64
	}{
		// 10 on the table; they hold one of 5C (fifteen, 2) and KC (20, 0).
		{"their reply", "TH", "", "5C KC", 1, 0.5 * -2},
		// My 5D answers 5C with a pair (2) and KC with nothing.
		{"my answer", "TH", "5D", "5C KC", 1, 0.5*(2-2) + 0.5*0},
		// They play the 5 whenever they hold it: P = 1 - (2/3)(1/2).
		{"their best card", "TH", "", "5C KC QC", 2, (1 - 2.0/3*1.0/2) * -2},
		// At 25 no ten-card fits, so they say go and I get the point.
		{"go", "TH KD 5S", "", "KC QC JC TC", 2, 1},
		// They have no cards left.
		{"no cards", "TH", "", "5C KC", 0, 1},
	} {
		seq := cards(tc.seq)
		count := 0
		for _, c := range seq {
			count += c.Value()
		}
		h.opp = holdings{k: tc.k}
		for _, u := range cards(tc.unseen) {
			h.opp.cards[h.opp.n], h.opp.w[h.opp.n] = u, 1
			h.opp.total++
			h.opp.n++
		}
		if got := h.lookahead(seq, count, cards(tc.rest)); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: lookahead = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHoldingsNoneOf(t *testing.T) {
	// Equal weights: the hypergeometric probability.
	m := holdings{n: 10, total: 10, k: 3}
	if got, want := m.noneOf(4, 4), 6.0/10*5/9*4/8; math.Abs(got-want) > 1e-12 {
		t.Errorf("equal weights: %v, want %v", got, want)
	}
	// Heavier cards are likelier to be held.
	m = holdings{n: 10, total: 14, k: 3}
	if light, heavy := m.noneOf(2, 1), m.noneOf(2, 4); heavy >= light {
		t.Errorf("none of two heavy cards %v, of two light ones %v", heavy, light)
	}
	if got := m.noneOf(8, 13); got != 0 {
		t.Errorf("only two cards left for three: %v", got)
	}
}

// The opponent's real hand is likelier under the rank prior and the go
// floor than under card counting alone.
func TestHoldingsLikelihood(t *testing.T) {
	var base Heuristic
	var ll [2]float64
	for seed := uint64(1); seed <= 500; seed++ {
		g := engine.NewGame(seed)
		g.SetQuiet(true)
		for !g.Over() {
			for p := 0; p < 2 && !g.Over(); p++ {
				if !g.ToAct(p) {
					continue
				}
				o, legal := g.Observation(p), g.Legal(p)
				if o.Phase == engine.PhasePegging && o.OppHandN > 0 {
					for v, model := range []bool{false, true} {
						var m holdings
						m.observe(&o, model, model)
						for _, c := range g.Hand(1 - p) {
							i := slices.Index(m.cards[:m.n], c)
							if i < 0 {
								t.Fatalf("model %v rules out the opponent's %v", model, c)
							}
							ll[v] += math.Log(1 - m.noneOf(1, m.w[i]))
						}
					}
				}
				if err := g.Apply(p, base.Choose(&o, legal)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if ll[1] <= ll[0] {
		t.Errorf("log-likelihood %.1f with the model, %.1f by card counting", ll[1], ll[0])
	}
}

func TestShapeIndex(t *testing.T) {
	seen := make([]bool, numShapes)
	for a := 1; a <= 13; a++ {
		for b := a; b <= 13; b++ {
			for c := b; c <= 13; c++ {
				for d := c; d <= 13; d++ {
					// Suits and order do not matter.
					sh := shapeIndex([4]engine.Card{engine.NewCard(d, engine.Hearts), engine.NewCard(b, engine.Clubs),
						engine.NewCard(a, engine.Spades), engine.NewCard(c, engine.Diamonds)})
					if seen[sh] {
						t.Fatalf("%d %d %d %d: shape %d seen twice", a, b, c, d, sh)
					}
					seen[sh] = true
				}
			}
		}
	}
	if sh := shapeIndex([4]engine.Card(cards("AC AD AH AS"))); sh != 0 {
		t.Errorf("AAAA is shape %d", sh)
	}
	if sh := shapeIndex([4]engine.Card(cards("KC KD KH KS"))); sh != numShapes-1 {
		t.Errorf("KKKK is shape %d", sh)
	}
}

// Low cards and pairs peg better than ten-cards, on either side.
func TestH2PegShape(t *testing.T) {
	low := shapeIndex([4]engine.Card(cards("2C 2D 3H 3S")))
	high := shapeIndex([4]engine.Card(cards("TC JD QH KS")))
	for side := range h2PegShape {
		if l, h := h2PegShape[side][low], h2PegShape[side][high]; l <= h+1 {
			t.Errorf("side %d: 2233 pegs %.2f, TJQK %.2f", side, l, h)
		}
	}
}
