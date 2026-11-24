package ai

import (
	"context"
	"testing"
	"time"

	"cribbage/internal/bots"
)

func heuristic(int) bots.Bot { return bots.NewHeuristic() }

// A deterministic bot against itself replays each deal as a mirror image
// with the seats swapped, so it wins exactly one game of every pair.
func TestArenaPairsMirror(t *testing.T) {
	r, err := Arena(context.Background(), heuristic, heuristic, 400, 4, 7)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pairs != 200 || r.Games != 400 || r.WinsA != 200 {
		t.Fatalf("pairs %d games %d wins %d, want 200 400 200", r.Pairs, r.Games, r.WinsA)
	}
	if rate, ci := r.WinRate(); rate != 0.5 || ci != 0 {
		t.Errorf("win rate %v ± %v, want 0.5 ± 0", rate, ci)
	}
	if m, ci := r.Margin(); m != 0 || ci != 0 {
		t.Errorf("margin %v ± %v, want 0 ± 0", m, ci)
	}
	if p, ci := r.PegMargin(); p != 0 || ci != 0 || r.PegA == 0 {
		t.Errorf("pegging margin %v ± %v (%d points), want 0 ± 0", p, ci, r.PegA)
	}
	if r.UnpairedCI() == 0 || r.UnpairedMarginCI() == 0 {
		t.Error("unpaired intervals should be positive")
	}
}

func TestArenaPairedIntervalIsNarrower(t *testing.T) {
	random := func(w int) bots.Bot { return bots.NewRandom(uint64(w)) }
	r, err := Arena(context.Background(), heuristic, random, 2000, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ci := r.Margin(); ci >= r.UnpairedMarginCI() {
		t.Errorf("paired margin interval %.3f not narrower than unpaired %.3f", ci, r.UnpairedMarginCI())
	}
}

func TestDealSeedsDiffer(t *testing.T) {
	seen := map[uint64]bool{}
	for seed := uint64(0); seed < 4; seed++ {
		for i := int64(0); i < 1000; i++ {
			s := DealSeed(seed, i)
			if seen[s] {
				t.Fatalf("seed %d deal %d repeats an earlier deal seed", seed, i)
			}
			seen[s] = true
		}
	}
}

func TestLatencyQuantiles(t *testing.T) {
	var l Latency
	for i := 1; i <= 100; i++ {
		l.Record(time.Duration(i) * time.Microsecond)
	}
	if l.Mean() != 50500*time.Nanosecond || l.Max != 100*time.Microsecond {
		t.Errorf("mean %v max %v", l.Mean(), l.Max)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 50 * time.Microsecond}, {0.99, 99 * time.Microsecond}} {
		got := l.Quantile(c.q)
		if got < c.want || float64(got) > 1.2*float64(c.want) {
			t.Errorf("quantile %v = %v, want within 20%% above %v", c.q, got, c.want)
		}
	}
}

// Both games of a pair start from the same hand with the seats swapped, so a deterministic
// bot against itself still wins exactly one game of each.
func TestArenaFromMirror(t *testing.T) {
	r, err := ArenaFrom(context.Background(), heuristic, heuristic, 400, 4, 7, 100, 118, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.WinsA != 200 || r.Hands > 400*4 {
		t.Errorf("wins %d of 400, %d hands", r.WinsA, r.Hands)
	}
}
