package expert

import (
	"bytes"
	"context"
	"math"
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

func heuristicTables(t *testing.T, games int) (*SelfPlay, *DiscardTables) {
	t.Helper()
	sp, err := Collect(context.Background(), func(int) bots.Bot { return bots.NewHeuristic() }, games, 4, 5)
	if err != nil {
		t.Fatal(err)
	}
	return sp, BuildDiscardTables(sp.Details)
}

func TestDiscardTablesAreDistributions(t *testing.T) {
	sp, d := heuristicTables(t, 200)
	if len(sp.Details) < 600 {
		t.Fatalf("only %d detailed hands", len(sp.Details))
	}
	for name, tab := range map[string]struct {
		vals []float32
		size int
	}{"crib": {d.Crib, maxShow}, "peg": {d.Peg, pegBins}, "show": {d.Show, maxShow}} {
		for i := 0; i < len(tab.vals); i += tab.size {
			var sum float64
			for _, p := range tab.vals[i : i+tab.size] {
				sum += float64(p)
			}
			if math.Abs(sum-1) > 1e-4 {
				t.Fatalf("%s cell %d sums to %v", name, i/tab.size, sum)
			}
		}
	}
	// A pair of fives makes a far better crib than king-queen.
	if d.CribMean(0, 5, 5) < d.CribMean(0, 12, 13)+2 {
		t.Errorf("crib 5-5 %.2f vs Q-K %.2f", d.CribMean(0, 5, 5), d.CribMean(0, 12, 13))
	}

	b := &Bundle{Win: &WinTable{}, Discard: d}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Discard == nil || got.Discard.CribMean(1, 5, 11) != d.CribMean(1, 5, 11) {
		t.Error("discard tables did not survive a round trip")
	}
}

// With tables learned from the heuristic's own play, the discard engine
// mostly agrees with it: both maximize roughly the same points.
func TestDiscardAgreesWithHeuristicMostly(t *testing.T) {
	sp, d := heuristicTables(t, 2000)
	win, err := BuildWinTable(&sp.Hands, 200)
	if err != nil {
		t.Fatal(err)
	}
	disc := newDiscarder(win, d)
	var h bots.Heuristic
	agree, n := 0, 0
	for seed := uint64(0); seed < 300; seed++ {
		g := engine.NewGame(seed)
		for p := 0; p < 2; p++ {
			o := g.Observation(p)
			legal := g.Legal(p)
			if disc.choose(&o, legal) == h.Choose(&o, legal) {
				agree++
			}
			n++
		}
	}
	if agree < n/2 {
		t.Errorf("agreed with the heuristic on %d of %d discards", agree, n)
	}
	t.Logf("agreed on %d of %d discards", agree, n)
}
