package ai

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"cribbage/internal/bots"
)

func collect(t *testing.T, newA, newB func(int) bots.Bot, games int, seed uint64) []Pair {
	t.Helper()
	var mu sync.Mutex
	var ps []Pair
	_, err := ArenaPairs(context.Background(), newA, newB, games, 4, seed, func(p Pair) {
		mu.Lock()
		ps = append(ps, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestPairFileRoundTrip(t *testing.T) {
	random := func(w int) bots.Bot { return bots.NewRandom(uint64(w)) }
	f := &PairFile{Bot: "heuristic", Opponent: "random", Seed: 9, Pairs: collect(t, heuristic, random, 100, 9)}
	path := filepath.Join(t.TempDir(), "p.csv")
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	g, err := LoadPairFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f, g) {
		t.Errorf("read back %+v\nwant %+v", g, f)
	}
}

// The same deterministic match twice: every deal plays the same way, so
// the coupled difference is exactly zero with a zero interval.
func TestCoupledSameBotIsZero(t *testing.T) {
	h2 := func(int) bots.Bot { return bots.NewHeuristic2() }
	a := collect(t, h2, heuristic, 200, 3)
	b := collect(t, h2, heuristic, 200, 3)
	byDeal := map[int64]Pair{}
	for _, p := range b {
		byDeal[p.Deal] = p
	}
	var c Coupled
	for _, p := range a {
		c.Add(p, byDeal[p.Deal])
	}
	if c.Pairs != 100 || c.Deviated != 0 {
		t.Fatalf("pairs %d deviated %d, want 100 0", c.Pairs, c.Deviated)
	}
	for name, f := range map[string]func() (float64, float64){"win": c.WinRate, "margin": c.Margin, "peg": c.PegMargin} {
		if m, ci := f(); m != 0 || ci != 0 {
			t.Errorf("%s: %v ± %v, want 0 ± 0", name, m, ci)
		}
	}
	// So does a randomized opponent, which is reseeded per deal.
	random := func(w int) bots.Bot { return bots.NewRandom(uint64(w)) }
	r1 := collect(t, h2, random, 200, 4)
	r2 := collect(t, h2, random, 200, 4)
	traces := map[int64]uint64{}
	for _, p := range r1 {
		traces[p.Deal] = p.Trace
	}
	for _, p := range r2 {
		if traces[p.Deal] != p.Trace {
			t.Fatalf("deal %d against random played differently twice", p.Deal)
		}
	}
	// A different bot on the same deals deviates on most of them.
	d := collect(t, heuristic, heuristic, 200, 3)
	var c2 Coupled
	for _, p := range d {
		c2.Add(p, byDeal[p.Deal])
	}
	if c2.Deviated < 50 {
		t.Errorf("heuristic v heuristic2 deviated on %d of 100 deals", c2.Deviated)
	}
}

// Simulated win fractions per pair: the test accepts H1 when the true mean
// is Mu1 and H0 when it is Mu0, most of the time.
func TestSPRTDecides(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pair := func(p float64) float64 { // wins of 2 games at rate p, as a fraction
		w := 0
		for range 2 {
			if rng.Float64() < p {
				w++
			}
		}
		return float64(w) / 2
	}
	for _, c := range []struct {
		p    float64
		want int
	}{{0.5, -1}, {0.52, 1}} {
		got := map[int]int{}
		for range 40 {
			s := SPRT{Mu0: 0.5, Mu1: 0.52, Alpha: 0.05, Beta: 0.05, MinPairs: 100}
			for s.Decision() == 0 {
				s.Add(pair(c.p))
			}
			got[s.Decision()]++
		}
		if got[c.want] < 34 {
			t.Errorf("true rate %v: decisions %v, want mostly %d", c.p, got, c.want)
		}
	}
}
