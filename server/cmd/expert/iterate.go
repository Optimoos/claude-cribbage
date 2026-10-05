package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/expert"
	"cribbage/internal/players"
)

// iterate runs policy iteration: each round rebuilds every table from
// self-play of the current best bundle, then plays the new bundle against
// the best in paired games and keeps it only if it does not lose ground.
func iterate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("iterate", flag.ExitOnError)
	start := fs.String("start", "models-expert/expert.bin", "bundle to start from")
	dir := fs.String("dir", "models-expert/iter", "directory for each round's bundle and the log")
	rounds := fs.Int("rounds", 3, "rounds")
	games := fs.Int("games", 400000, "self-play games per round")
	effort := fs.Int("effort", 25, "search effort in self-play (both seats)")
	evalGames := fs.Int("eval-games", 20000, "paired evaluation games per round")
	evalEffort := fs.Int("eval-effort", 100, "search effort in evaluation (both bots)")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel games")
	window := fs.Int("window", expert.DefaultWindowHands, "hands used to value each score in the win table")
	firstRound := fs.Int("first", 1, "number of the first round (to continue a run)")
	fs.Parse(args)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(*dir, "iterate.csv")
	_, statErr := os.Stat(logPath)
	logf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	if statErr != nil {
		fmt.Fprintln(logf, "round,time,candidate,best,games,win,win_ci,margin,margin_ci,accepted")
	}

	best := *start
	for r := *firstRound; r < *firstRound+*rounds; r++ {
		fmt.Printf("\n== round %d: self-play of %s at effort %d\n", r, best, *effort)
		b, err := makeBundle(ctx, buildSpec{
			bot:     fmt.Sprintf("%s@%d", best, *effort),
			want:    map[string]bool{"win": true, "discard": true, "keep": true},
			games:   *games,
			workers: *workers,
			seed:    uint64(1000 * r),
			window:  *window,
		})
		if err != nil {
			return err
		}
		cand := filepath.Join(*dir, fmt.Sprintf("round-%02d.bin", r))
		if err := b.Save(cand); err != nil {
			return err
		}
		newA, err := players.Resolve(fmt.Sprintf("%s@%d", cand, *evalEffort))
		if err != nil {
			return err
		}
		newB, err := players.Resolve(fmt.Sprintf("%s@%d", best, *evalEffort))
		if err != nil {
			return err
		}
		t0 := time.Now()
		res, err := ai.Arena(ctx, newA, newB, *evalGames, *workers, uint64(1000*r+1))
		if err != nil {
			return err
		}
		rate, ci := res.WinRate()
		m, mci := res.Margin()
		accepted := rate >= 0.5
		fmt.Printf("round %d vs %s: %.2f%% ± %.2f%%, margin %+.2f ± %.2f (%d games in %v); accepted %v\n",
			r, best, 100*rate, 100*ci, m, mci, res.Games, time.Since(t0).Round(time.Second), accepted)
		fmt.Fprintf(logf, "%d,%s,%s,%s,%d,%.4f,%.4f,%.3f,%.3f,%v\n", r, time.Now().UTC().Format(time.RFC3339),
			cand, best, res.Games, rate, ci, m, mci, accepted)
		if accepted {
			best = cand
		}
	}
	fmt.Printf("\nbest bundle: %s\n", best)
	return nil
}
