package expert

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"cribbage/internal/bots"
)

// A corpus of heuristic games gives exactly what Collect records from the
// same deals, so tables can be built from either.
func TestCorpusMatchesCollect(t *testing.T) {
	h := func(int) bots.Bot { return bots.NewHeuristic() }
	meta := CorpusMeta{Bots: [2]string{"heuristic", "heuristic"}, Seed: 3, Games: 60}
	recs, err := CollectCorpus(context.Background(), h, h, meta, 4)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := Collect(context.Background(), h, 60, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := CorpusSelfPlay(recs, CorpusFilter{})
	if got.Games != sp.Games || !reflect.DeepEqual(got.Hands, sp.Hands) || got.Outcomes != sp.Outcomes {
		t.Fatalf("hands or outcomes differ: %d games, %d hands; want %d, %d", got.Games, got.Hands.Len(), sp.Games, sp.Hands.Len())
	}
	if !reflect.DeepEqual(got.Details, sp.Details) {
		t.Fatalf("details differ: %d v %d", len(got.Details), len(sp.Details))
	}
	for _, r := range recs {
		if r.Complete() {
			var pts [2]int
			for _, s := range r.Steps[:r.StepN] {
				if s > 0 {
					pts[r.Dealer] += int(s)
				} else {
					pts[1-r.Dealer] -= int(s)
				}
			}
			for p := 0; p < 2; p++ {
				if int(r.Start[p])+pts[p] != int(r.End[p]) {
					t.Fatalf("game %d hand %d: steps %v do not take %v to %v", r.Game, r.Hand, r.Steps[:r.StepN], r.Start, r.End)
				}
			}
		}
	}

	path := filepath.Join(t.TempDir(), "c.bin")
	if err := SaveCorpus(path, meta, recs); err != nil {
		t.Fatal(err)
	}
	m2, r2, err := LoadCorpus(path)
	if err != nil || m2 != meta || !reflect.DeepEqual(r2, recs) {
		t.Fatalf("round trip: %v, meta %+v", err, m2)
	}
}

// Exploring seats discard at random on about the asked share of hands,
// and synthetic games start where asked.
func TestCorpusExploreAndSynthetic(t *testing.T) {
	h := func(int) bots.Bot { return bots.NewHeuristic() }
	meta := CorpusMeta{Seed: 4, Games: 200, Explore: 0.2, StartMin: 100, StartMax: 115}
	recs, err := CollectCorpus(context.Background(), h, h, meta, 4)
	if err != nil {
		t.Fatal(err)
	}
	var explore, seats int
	for _, r := range recs {
		if r.Flags&FlagSynthetic == 0 {
			t.Fatal("hand not flagged synthetic")
		}
		if r.Hand == 1 && (r.Start[0] < 100 || r.Start[0] > 115 || r.Start[1] < 100 || r.Start[1] > 115) {
			t.Fatalf("game %d starts at %v", r.Game, r.Start)
		}
		for p := 0; p < 2; p++ {
			seats++
			if r.Flags&(FlagExplore0<<p) != 0 {
				explore++
			}
		}
	}
	if f := float64(explore) / float64(seats); f < 0.15 || f > 0.25 {
		t.Errorf("explored %.3f of seat-hands, want about 0.2", f)
	}
}
