package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"time"

	"cribbage/internal/expert"
	"cribbage/internal/players"
)

// corpus plays games and saves every hand in full (E4), for building
// tables and fitting models later without replaying games.
func corpus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("corpus", flag.ExitOnError)
	a := fs.String("a", "models-expert/expert.bin@400", "seat 0 player")
	b := fs.String("b", "", "seat 1 player (default: the same as -a)")
	games := fs.Int("games", 100000, "games")
	seed := fs.Uint64("seed", 100001, "base seed (below 900000: evaluation uses 900001 and above)")
	explore := fs.Float64("explore", 0, "chance per hand and seat of a random discard, so rare table cells get data (flagged in the corpus)")
	start := fs.String("start", "", "LO,HI: start every game at random scores in [LO, HI] to cover the endgame")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel games")
	out := fs.String("out", "", "corpus file to write (required)")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("corpus: -out is required")
	}
	if *b == "" {
		*b = *a
	}
	meta := expert.CorpusMeta{Bots: [2]string{*a, *b}, Seed: *seed, Games: *games, Explore: *explore}
	if *start != "" {
		if _, err := fmt.Sscanf(*start, "%d,%d", &meta.StartMin, &meta.StartMax); err != nil ||
			meta.StartMin < 0 || meta.StartMax >= expert.Scores || meta.StartMin > meta.StartMax {
			return fmt.Errorf("corpus: -start wants LO,HI within 0..120")
		}
	}
	newA, err := players.Resolve(*a)
	if err != nil {
		return err
	}
	newB, err := players.Resolve(*b)
	if err != nil {
		return err
	}
	t0 := time.Now()
	fmt.Printf("corpus: %s v %s, %d games, seed %d, explore %g, start %q, %d workers\n", *a, *b, *games, *seed, *explore, *start, *workers)
	recs, err := expert.CollectCorpus(ctx, newA, newB, meta, *workers)
	if err != nil {
		return err
	}
	meta.Saved = time.Now().UTC().Format(time.RFC3339)
	if err := expert.SaveCorpus(*out, meta, recs); err != nil {
		return err
	}
	fmt.Printf("saved %d hands of %d games to %s in %v\n", len(recs), *games, *out, time.Since(t0).Round(time.Second))
	return nil
}
