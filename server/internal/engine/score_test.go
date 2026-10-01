package engine

import (
	"math/rand/v2"
	"sort"
	"testing"
)

func hand4(s string) [4]Card {
	cs := MustParseCards(s)
	return [4]Card{cs[0], cs[1], cs[2], cs[3]}
}

func card(s string) Card { return MustParseCards(s)[0] }

func TestScoreHandKnown(t *testing.T) {
	tests := []struct {
		name    string
		hand    string
		starter string
		crib    bool
		want    Breakdown
	}{
		{"29 hand", "5H 5C 5S JD", "5D", false, Breakdown{Fifteens: 16, Pairs: 12, Nobs: 1}},
		{"28 hand", "5H 5C 5S 5D", "JH", false, Breakdown{Fifteens: 16, Pairs: 12}},
		{"24 double-double run", "4C 5D 5H 6S", "6C", false, Breakdown{Fifteens: 8, Pairs: 4, Runs: 12}},
		{"double run of four", "3C 4D 5H 5S", "6C", false, Breakdown{Fifteens: 4, Pairs: 2, Runs: 8}},
		{"run of five", "AC 2D 3H 4S", "5C", false, Breakdown{Fifteens: 2, Runs: 5}},
		{"triple run", "7C 7D 7H 8S", "9C", false, Breakdown{Fifteens: 6, Pairs: 6, Runs: 9}},
		{"hand flush 4", "2H 4H 6H 8H", "KS", false, Breakdown{Flush: 4}},
		{"crib flush 4 does not count", "2H 4H 6H 8H", "KS", true, Breakdown{}},
		{"hand flush 5", "2H 4H 6H 8H", "KH", false, Breakdown{Flush: 5}},
		{"crib flush 5", "2H 4H 6H 8H", "KH", true, Breakdown{Flush: 5}},
		{"zero hand", "2C 4D 6S 8H", "KC", false, Breakdown{}},
		{"nobs", "JH 2C 4D 8S", "KH", false, Breakdown{Nobs: 1}},
		{"jack starter is not nobs", "2H 4C 6D 8S", "JH", false, Breakdown{}},
		{"fifteens with faces", "TC JD QH KS", "5C", false, Breakdown{Fifteens: 8, Runs: 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScoreHand(hand4(tt.hand), card(tt.starter), tt.crib)
			if got != tt.want {
				t.Errorf("got %+v (total %d), want %+v (total %d)", got, got.Total(), tt.want, tt.want.Total())
			}
		})
	}
}

// refScore is a slow, literal implementation of the scoring rules used to
// cross-check ScoreHand.
func refScore(hand [4]Card, starter Card, crib bool) Breakdown {
	var b Breakdown
	all := []Card{hand[0], hand[1], hand[2], hand[3], starter}
	for mask := 1; mask < 32; mask++ {
		sum := 0
		for i := 0; i < 5; i++ {
			if mask&(1<<i) != 0 {
				sum += all[i].Value()
			}
		}
		if sum == 15 {
			b.Fifteens += 2
		}
	}
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			if all[i].Rank() == all[j].Rank() {
				b.Pairs += 2
			}
		}
	}
	for size := 5; size >= 3 && b.Runs == 0; size-- {
		for mask := 0; mask < 32; mask++ {
			var ranks []int
			for i := 0; i < 5; i++ {
				if mask&(1<<i) != 0 {
					ranks = append(ranks, all[i].Rank())
				}
			}
			if len(ranks) != size {
				continue
			}
			sort.Ints(ranks)
			ok := true
			for i := 1; i < len(ranks); i++ {
				if ranks[i] != ranks[i-1]+1 {
					ok = false
				}
			}
			if ok {
				b.Runs += size
			}
		}
	}
	if hand[0].Suit() == hand[1].Suit() && hand[1].Suit() == hand[2].Suit() && hand[2].Suit() == hand[3].Suit() {
		if starter.Suit() == hand[0].Suit() {
			b.Flush = 5
		} else if !crib {
			b.Flush = 4
		}
	}
	for _, c := range hand {
		if c.Rank() == 11 && c.Suit() == starter.Suit() {
			b.Nobs = 1
		}
	}
	return b
}

func TestScoreHandMatchesReference(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	n := 200000
	if testing.Short() {
		n = 10000
	}
	for i := 0; i < n; i++ {
		perm := r.Perm(52)
		var h [4]Card
		for j := range h {
			h[j] = Card(perm[j])
		}
		st := Card(perm[4])
		crib := i%2 == 0
		if got, want := ScoreHand(h, st, crib), refScore(h, st, crib); got != want {
			t.Fatalf("%v + %v crib=%v: got %+v, want %+v", h, st, crib, got, want)
		}
	}
}

func TestImpossibleScores(t *testing.T) {
	// No hand is worth 19, 25, 26 or 27, and none exceeds 29. Checking all
	// ~12.9M hand/starter pairs is slow, so stride through a large subset.
	var seen [30]bool
	for a := 0; a < 52; a++ {
		for b := a + 1; b < 52; b++ {
			for c := b + 1; c < 52; c++ {
				for d := c + 1; d < 52; d += 3 {
					for s := 0; s < 52; s += 7 {
						if s == a || s == b || s == c || s == d {
							continue
						}
						tot := ScoreHand([4]Card{Card(a), Card(b), Card(c), Card(d)}, Card(s), false).Total()
						if tot >= len(seen) {
							t.Fatalf("score %d out of range", tot)
						}
						seen[tot] = true
					}
				}
			}
		}
	}
	for _, imp := range []int{19, 25, 26, 27} {
		if seen[imp] {
			t.Errorf("found impossible score %d", imp)
		}
	}
}

func TestScorePeg(t *testing.T) {
	tests := []struct {
		seq  string
		want PegScore
	}{
		{"5H", PegScore{}},
		{"TH 5C", PegScore{Fifteen: 2}},
		{"7H 7C", PegScore{Pairs: 2}},
		{"7H 7C 7D", PegScore{Pairs: 6}},
		{"5H 5C 5D 5S", PegScore{Pairs: 12}},
		{"7H 8C 6D", PegScore{Run: 3}},
		{"7H 8C 7D", PegScore{}},
		{"3C 4D 2H 5S", PegScore{Run: 4}},
		{"4C 6D 5H", PegScore{Fifteen: 2, Run: 3}},
		{"KC QD JH AS", PegScore{ThirtyOne: 2}},
		{"KC QD JH", PegScore{Run: 3}},
		{"2C 3D 2H 4S", PegScore{Run: 3}},       // run is 3-2-4 (the trailing three)
		{"AC 2D 3H 3S", PegScore{Pairs: 2}},     // pair breaks the run
		{"AC 2D 3H 4S 5C 6D", PegScore{Run: 6}}, // count 21
		{"9C 6D 7H 8S", PegScore{Run: 4}},       // count 30
		{"TC TD 5H 6S", PegScore{ThirtyOne: 2}}, // count 31
	}
	for _, tt := range tests {
		got := ScorePeg(MustParseCards(tt.seq))
		if got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.seq, got, tt.want)
		}
	}
}
