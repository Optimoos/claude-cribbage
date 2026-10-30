package bots

import (
	"math"
	"math/rand/v2"
	"testing"

	"cribbage/internal/engine"
)

func TestHeuristic2DiscardsAsHeuristicBeforeEndgame(t *testing.T) {
	h, base := NewHeuristic2(), NewHeuristic()
	checked := 0
	for seed := uint64(1); seed <= 200; seed++ {
		g := engine.NewGame(seed)
		g.SetQuiet(true)
		for !g.Over() {
			for p := 0; p < 2 && !g.Over(); p++ {
				if !g.ToAct(p) {
					continue
				}
				o, legal := g.Observation(p), g.Legal(p)
				a := base.Choose(&o, legal)
				if o.Phase == engine.PhaseDiscard && max(o.Scores[0], o.Scores[1]) < h.Endgame {
					if b := h.Choose(&o, legal); b != a {
						t.Fatalf("seed %d, scores %v: heuristic2 chose %v, heuristic %v", seed, o.Scores, b, a)
					}
					checked++
				}
				if err := g.Apply(p, a); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d discards checked", checked)
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
		if got := h.lookahead(seq, count, cards(tc.rest), cards(tc.unseen), tc.k); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: lookahead = %v, want %v", tc.name, got, tc.want)
		}
	}
}
