package bots

import (
	"math/rand/v2"
	"testing"

	"cribbage/internal/engine"
)

func TestHeuristic2MatchesHeuristicBeforeEndgame(t *testing.T) {
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
				if max(o.Scores[0], o.Scores[1]) < h.Endgame {
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
	if checked < 10000 {
		t.Fatalf("only %d decisions checked", checked)
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
