package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"cribbage/internal/expert"
)

// pegpair builds the shape-pair pegging table (proposal 4) by playing
// every pair of kept shapes with the bundle's own pegger.
func pegpair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pegpair", flag.ExitOnError)
	in := fs.String("bundle", "models-expert/expert.bin", "bundle whose pegger plays, and which gets the table")
	out := fs.String("out", "", "bundle to write (required)")
	effort := fs.Int("effort", 25, "pegging search effort")
	samples := fs.Int("samples", 4, "hands per shape pair")
	seed := fs.Uint64("seed", 300001, "base seed")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel workers")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("pegpair: -out is required")
	}
	b, err := expert.LoadBundle(*in)
	if err != nil {
		return err
	}
	t0 := time.Now()
	t, err := expert.BuildPegPair(ctx, func() [2]expert.Choosers {
		return [2]expert.Choosers{expert.NewBot(b, *effort), expert.NewBot(b, *effort)}
	}, *samples, *workers, *seed, func(n int) {
		if n%91 == 0 {
			el := time.Since(t0)
			fmt.Fprintf(os.Stderr, "  %d/1820 dealer shapes, %v, about %v left\n", n, el.Round(time.Second),
				(time.Duration(float64(el) / float64(n) * float64(1820-n))).Round(time.Minute))
		}
	})
	if err != nil {
		return err
	}
	b.PegPair = t
	b.Meta.Notes = strings.TrimSpace(b.Meta.Notes + fmt.Sprintf(" pegpair: %d hands per shape pair at effort %d (%s).", *samples, *effort, *in))
	b.Meta.Saved = time.Now().UTC().Format(time.RFC3339)
	if err := b.Save(*out); err != nil {
		return err
	}
	fmt.Printf("shape-pair pegging table, %d samples, in %v, saved %s\n", *samples, time.Since(t0).Round(time.Second), *out)
	return nil
}
