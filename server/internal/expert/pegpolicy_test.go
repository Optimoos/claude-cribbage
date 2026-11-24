package expert

import (
	"context"
	"math"
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// With weight only on immediate points, the policy is the old softmax on
// points, so the two likelihoods agree on every world.
func TestPegPolicyMatchesPoints(t *testing.T) {
	old := newSearcher(&WinTable{}, 0)
	old.pegTemp = 0.7
	pol := newSearcher(&WinTable{}, 0)
	p := pointsPolicy(0.7)
	pol.pegPol = &p
	checked := 0
	botGames(t, bots.NewHeuristic(), 60, func(g *engine.Game, o *engine.Observation, _ engine.Action) {
		if o.Phase != engine.PhasePegging || o.OppHandN < 1 {
			return
		}
		var seen [52]bool
		for _, c := range o.Kept {
			seen[c] = true
		}
		for _, c := range o.Discards {
			seen[c] = true
		}
		seen[o.Starter] = true
		var oppPlayed []engine.Card
		for _, pc := range o.Played[:o.PlayedN] {
			seen[pc.Card] = true
			if pc.Player != o.Player {
				oppPlayed = append(oppPlayed, pc.Card)
			}
		}
		if len(oppPlayed) == 0 {
			return
		}
		old.enumerate(o, &seen, oppPlayed)
		pol.prepPegPolicy(o)
		for _, w := range old.worlds {
			a, b := old.pegLikelihood(o, w), pol.pegPolicyLik(w)
			if math.Abs(a-b) > 1e-12*max(a, 1e-300) {
				t.Fatalf("world %v: softmax %g, policy %g", w.ranks[:w.n], a, b)
			}
			checked++
		}
	})
	if checked < 10000 {
		t.Fatalf("only %d worlds checked", checked)
	}
}

// Fitting to the heuristic's plays beats the softmax on points, on hands
// held out of the fit, and the bundle keeps the fitted policy.
func TestFitPegPolicy(t *testing.T) {
	h := func(int) bots.Bot { return bots.NewHeuristic() }
	recs, err := CollectCorpus(context.Background(), h, h, CorpusMeta{Seed: 7, Games: 600}, 4)
	if err != nil {
		t.Fatal(err)
	}
	half := len(recs) / 2
	train, test := PegDecisions(recs[:half], 3), PegDecisions(recs[half:], 3)
	if len(train) < 3000 {
		t.Fatalf("only %d decisions", len(train))
	}
	for _, d := range train {
		if d.Chosen < 0 || d.Chosen >= len(d.F) || len(d.F) > 4 {
			t.Fatalf("bad decision %+v", d)
		}
	}
	base := pointsPolicy(DefaultPegTemp)
	fit := FitPegPolicy(train, base, 1e-4, 30)
	lb, lf := base.LogLik(test), fit.LogLik(test)
	t.Logf("held-out log-likelihood per decision: points %.4f, fitted %.4f\n%v", lb, lf, fit)
	if lf <= lb+0.05 {
		t.Errorf("fit %.4f does not beat points %.4f", lf, lb)
	}
}
