package bots

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"cribbage/internal/engine"
)

func cards(s string) []engine.Card { return engine.MustParseCards(s) }

func discardObs(hand string, dealer bool) engine.Observation {
	o := engine.Observation{Player: 0, Phase: engine.PhaseDiscard, Dealer: 1, Starter: engine.NoCard, MustAct: true}
	if dealer {
		o.Dealer = 0
	}
	h := cards(hand)
	copy(o.Hand[:], h)
	o.HandN = len(h)
	return o
}

func legalDiscards(o *engine.Observation) []engine.Action {
	var out []engine.Action
	h := o.HandCards()
	for i := range h {
		for j := i + 1; j < len(h); j++ {
			out = append(out, engine.Discard(h[i], h[j]))
		}
	}
	return out
}

func TestHeuristicKeepsFives(t *testing.T) {
	// The dealer may keep the K instead of the J (the jack's nobs chance moves
	// to its own crib, a near tie), but the three fives always stay and the
	// 2 always goes.
	for _, dealer := range []bool{true, false} {
		o := discardObs("5H 5C 5S JD KC 2D", dealer)
		a := NewHeuristic().Choose(&o, legalDiscards(&o))
		if a.Cards[0].Rank() == 5 || a.Cards[1].Rank() == 5 || !slices.Contains(a.Cards[:], cards("2D")[0]) {
			t.Errorf("dealer=%v: discarded %v", dealer, a)
		}
	}
	o := discardObs("5H 5C 5S JD KC 2D", false)
	a := NewHeuristic().Choose(&o, legalDiscards(&o))
	if slices.Contains(a.Cards[:], cards("JD")[0]) {
		t.Errorf("pone should keep the jack for nobs, discarded %v", a)
	}
}

func TestHeuristicDoesNotGiveFivesToOpponent(t *testing.T) {
	// As pone, never throw 5-5 into the dealer's crib.
	o := discardObs("5H 5C 9S KD 2C 7H", false)
	a := NewHeuristic().Choose(&o, legalDiscards(&o))
	if a.Cards[0].Rank() == 5 && a.Cards[1].Rank() == 5 {
		t.Errorf("pone discarded %v", a)
	}
}

func TestCribEV(t *testing.T) {
	fives := CribEV(engine.NewCard(5, engine.Clubs), engine.NewCard(5, engine.Hearts))
	kingTen := CribEV(engine.NewCard(13, engine.Clubs), engine.NewCard(10, engine.Hearts))
	if fives < 8 || fives > 10 || kingTen > 4.5 || kingTen < 3 {
		t.Errorf("crib EV 5-5 = %.2f, K-10 = %.2f", fives, kingTen)
	}
	if CribEV(engine.NewCard(5, engine.Clubs), engine.NewCard(6, engine.Clubs)) !=
		CribEV(engine.NewCard(6, engine.Spades), engine.NewCard(5, engine.Hearts)) {
		t.Error("crib EV should be symmetric and suit independent")
	}
}

func pegObs(hand string, seq string, oppHand int) (engine.Observation, []engine.Action) {
	o := engine.Observation{Player: 0, Phase: engine.PhasePegging, Dealer: 1, Turn: 0, MustAct: true, Starter: engine.NewCard(13, engine.Spades), OppHandN: oppHand}
	h := cards(hand)
	copy(o.Hand[:], h)
	o.HandN = len(h)
	copy(o.Kept[:], h)
	o.Discards = [2]engine.Card{engine.NoCard, engine.NoCard}
	for _, c := range cards(seq) {
		o.Played[o.PlayedN] = engine.PlayedCard{Card: c, Player: 1}
		o.Count += c.Value()
		o.PlayedN++
	}
	var legal []engine.Action
	for _, c := range h {
		if o.Count+c.Value() <= 31 {
			legal = append(legal, engine.Play(c))
		}
	}
	return o, legal
}

func TestHeuristicPegging(t *testing.T) {
	tests := []struct {
		name, hand, seq string
		want            string
		avoid           bool
	}{
		{"takes fifteen", "TH 2C 3D 4S", "5S", "TH", false},
		{"takes pair", "8H 2C 3D QS", "8S", "8H", false},
		{"takes 31", "AH 9C 3D", "KS QS JS", "AH", false},
		{"does not lead a five", "5H KC 3D 9S", "", "5H", true},
	}
	for _, tt := range tests {
		o, legal := pegObs(tt.hand, tt.seq, 4)
		a := NewHeuristic().Choose(&o, legal)
		hit := a.Cards[0] == cards(tt.want)[0]
		if hit == tt.avoid {
			t.Errorf("%s: played %v", tt.name, a.Cards[0])
		}
	}
}

func TestPlayGameTrace(t *testing.T) {
	var sb strings.Builder
	r, err := PlayGame(42, [2]Bot{NewHeuristic(), NewRandom(1)}, &sb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "wins") || r.Scores[r.Winner] != engine.WinScore {
		t.Fatalf("unexpected result %+v", r)
	}
}

func TestHeuristicBeatsRandom(t *testing.T) {
	n := 2000
	if testing.Short() {
		n = 200
	}
	h, rnd := NewHeuristic(), NewRandom(7)
	wins := 0
	for i := 0; i < n; i++ {
		seats := [2]Bot{h, rnd}
		hs := 0
		if i%2 == 1 {
			seats, hs = [2]Bot{rnd, h}, 1
		}
		r, err := PlayGame(uint64(i), seats, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Winner == hs {
			wins++
		}
	}
	rate := float64(wins) / float64(n)
	t.Logf("heuristic won %.1f%% of %d games", 100*rate, n)
	if rate < 0.9 {
		t.Errorf("heuristic win rate %.3f < 0.90", rate)
	}
}

func BenchmarkHeuristicDiscard(b *testing.B) {
	o := discardObs("5H 5C 9S KD 2C 7H", false)
	legal := legalDiscards(&o)
	h := NewHeuristic()
	CribEV(0, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Choose(&o, legal)
	}
}

func BenchmarkPlayGameRandom(b *testing.B) {
	p := [2]Bot{NewRandom(1), NewRandom(2)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := PlayGame(uint64(i), p, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHandEVMatchesNaive(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 2000; i++ {
		perm := r.Perm(52)
		var seen [52]bool
		var kept [4]engine.Card
		for j := 0; j < 6; j++ {
			seen[perm[j]] = true
			if j < 4 {
				kept[j] = engine.Card(perm[j])
			}
		}
		if i%10 == 0 { // force some flushes
			for j := range kept {
				kept[j] = engine.NewCard(kept[j].Rank(), engine.Hearts)
			}
			if kept[0].Rank() == kept[1].Rank() || kept[1].Rank() == kept[2].Rank() || kept[0].Rank() == kept[2].Rank() ||
				kept[3].Rank() == kept[0].Rank() || kept[3].Rank() == kept[1].Rank() || kept[3].Rank() == kept[2].Rank() {
				continue
			}
			seen = [52]bool{}
			for _, c := range kept {
				seen[c] = true
			}
		}
		total, n := 0, 0
		for s := engine.Card(0); s < 52; s++ {
			if !seen[s] {
				total += engine.ScoreHand(kept, s, false).Total()
				n++
			}
		}
		want := float64(total) / float64(n)
		if got := HandEV(kept, &seen); math.Abs(got-want) > 1e-9 {
			t.Fatalf("%v: got %v want %v", kept, got, want)
		}
	}
}
