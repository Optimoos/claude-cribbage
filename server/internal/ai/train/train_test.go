package train

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"cribbage/internal/ai"
	"cribbage/internal/ai/nn"
)

func TestLambdaReturns(t *testing.T) {
	steps := []step{{v: 0.2}, {v: 0.4}, {v: 0.9}}
	approx := func(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-6 }

	// lambda = 1 is Monte Carlo: every target is the final result.
	for _, s := range lambdaReturns(steps, 1, 1, nil) {
		if !approx(s.target, 1) {
			t.Fatalf("MC target %v", s.target)
		}
	}
	// lambda = 0 is one-step TD: each target is the next value; the last is
	// the result.
	got := lambdaReturns(steps, 0, 0, nil)
	want := []float32{0.4, 0.9, 0}
	for i := range want {
		if !approx(got[i].target, want[i]) {
			t.Fatalf("TD(0) targets %v, want %v", got, want)
		}
	}
	// lambda = 0.5, result 1: G2 = 1; G1 = .5*1 + .5*1 = 1; G0 = .5*.9 + .5*1 = .95
	got = lambdaReturns([]step{{v: 0.3}, {v: 0.9}, {v: 0.5}}, 1, 0.5, nil)
	want = []float32{0.5*0.9 + 0.5*(0.5*0.5+0.5*1), 0.5*0.5 + 0.5*1, 1}
	for i := range want {
		if !approx(got[i].target, want[i]) {
			t.Fatalf("TD(0.5) target %d = %v, want %v", i, got[i].target, want[i])
		}
	}
}

// TestShortRun trains briefly and checks that checkpoints and the log are
// written and loadable.
func TestShortRun(t *testing.T) {
	if testing.Short() {
		t.Skip("trains for a few seconds")
	}
	dir := t.TempDir()
	err := Run(context.Background(), Config{
		Out: dir, Hidden: []int{16}, Actors: 2, LearnerThreads: 2, Batch: 64,
		Games: 300, EvalEvery: 150, EvalGames: 40,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"latest.bin", "best.bin", "train.csv"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	net, meta, err := nn.Load(filepath.Join(dir, "latest.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ai.CheckNet(net, meta); err != nil {
		t.Fatal(err)
	}
	if meta.Games < 300 || meta.Steps == 0 || meta.Metrics["vs_random"] == 0 {
		t.Fatalf("meta %+v", meta)
	}
	// Continue from the checkpoint.
	err = Run(context.Background(), Config{
		Out: dir, Init: filepath.Join(dir, "latest.bin"), Actors: 2, LearnerThreads: 2, Batch: 64,
		Games: 50, EvalEvery: 1000, EvalGames: 20,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, meta2, err := nn.Load(filepath.Join(dir, "latest.bin"))
	if err != nil || meta2.Games < meta.Games+50 {
		t.Fatalf("resumed run: %+v %v", meta2, err)
	}
}
