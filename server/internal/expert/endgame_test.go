package expert

import (
	"math"
	"testing"

	"cribbage/internal/engine"
)

func TestHiddenSuits(t *testing.T) {
	h := func(r int, s engine.Suit) engine.Card { return engine.NewCard(r, s) }
	var seen [52]bool
	known := []engine.Card{h(2, engine.Hearts), h(3, engine.Hearts), h(4, engine.Hearts)}
	for _, c := range known {
		seen[c] = true
	}
	starter := h(9, engine.Hearts)
	seen[starter] = true
	// One hidden seven: a flush of five if it is the seven of hearts, one
	// of the four unseen sevens.
	n, f4, f5 := hiddenSuits(known, []int8{7}, starter, &seen)
	if n != 0 || f4 != 0 || math.Abs(float64(f5)-0.25) > 1e-6 {
		t.Errorf("seven: nobs %v flush4 %v flush5 %v, want 0 0 0.25", n, f4, f5)
	}
	// Seen seven of hearts: no flush.
	seen[h(7, engine.Hearts)] = true
	if _, f4, f5 := hiddenSuits(known, []int8{7}, starter, &seen); f4 != 0 || f5 != 0 {
		t.Errorf("seen seven of hearts: flush %v %v", f4, f5)
	}
	// A hidden jack is nobs when it is the jack of hearts: one of four.
	if n, _, _ := hiddenSuits(known, []int8{11}, starter, &seen); math.Abs(float64(n)-0.25) > 1e-6 {
		t.Errorf("jack: nobs %v, want 0.25", n)
	}
	// Two hidden cards of one rank never make a flush.
	if _, f4, f5 := hiddenSuits(nil, []int8{5, 5, 6, 8}, starter, &seen); f4 != 0 || f5 != 0 {
		t.Errorf("pair: flush %v %v", f4, f5)
	}
}

func TestPegOutFirst(t *testing.T) {
	// I need 2 of my 8 pegging points, they need 6 of their 8: I get there first.
	if p := pegOutFirst(2, 8, 6, 8); p != 1 {
		t.Errorf("got %v, want 1", p)
	}
	if p := pegOutFirst(6, 8, 2, 8); p != 0 {
		t.Errorf("got %v, want 0", p)
	}
	if p := pegOutFirst(3, 6, 3, 6); p != 0.5 {
		t.Errorf("got %v, want 0.5", p)
	}
}
