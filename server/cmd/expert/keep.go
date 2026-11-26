package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	"cribbage/internal/bots"
	"cribbage/internal/expert"
	"cribbage/internal/players"
)

// keepx builds a bundle's exact keep table (proposal 3): its own
// discarder's choice on every six-card hand, by side.
func keepx(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keepx", flag.ExitOnError)
	in := fs.String("bundle", "models-expert/expert.bin", "bundle whose discarder decides, and which gets the table")
	opts := fs.String("opts", "", "player options for the discarder, as after @ in a player spec (e.g. cond); effort does not matter")
	out := fs.String("out", "", "bundle to write (required)")
	scores := fs.String("scores", "30,30", "MINE,THEIRS: the scores the discards are decided at")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel workers")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("keepx: -out is required")
	}
	var sc [2]int
	if _, err := fmt.Sscanf(*scores, "%d,%d", &sc[0], &sc[1]); err != nil {
		return fmt.Errorf("keepx: -scores wants MINE,THEIRS")
	}
	b, err := expert.LoadBundle(*in)
	if err != nil {
		return err
	}
	t0 := time.Now()
	spec := *in + "@1"
	if *opts != "" {
		spec += "," + *opts
	}
	newBot, err := players.Resolve(spec)
	if err != nil {
		return err
	}
	ex, err := expert.BuildExactKeep(ctx, func() bots.Bot { return newBot(0) }, sc, *workers, func(n int) {
		if n%2000 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/18395 rank shapes, %v\n", n, time.Since(t0).Round(time.Second))
		}
	})
	if err != nil {
		return err
	}
	b.KeepExact = ex
	if b.Meta.Metrics == nil {
		b.Meta.Metrics = map[string]float64{}
	}
	b.Meta.Notes = strings.TrimSpace(b.Meta.Notes + fmt.Sprintf(" keepx: discarder %s at %d-%d.", spec, sc[0], sc[1]))
	b.Meta.Saved = time.Now().UTC().Format(time.RFC3339)
	if err := b.Save(*out); err != nil {
		return err
	}
	fmt.Printf("exact keep table from %s at %d-%d in %v, saved %s\n", *in, sc[0], sc[1], time.Since(t0).Round(time.Second), *out)
	return nil
}

// keepcheck reports how much probability the opponent model gives each
// opponent's real keep, in held-out games against the bundle's bot.
func keepcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keepcheck", flag.ExitOnError)
	in := fs.String("bundle", "models-expert/expert.bin", "bundle whose opponent model is checked (and which plays seat 0)")
	vs := fs.String("vs", "self,heuristic,heuristic2,models-big/best.bin", "opponents whose keeps are scored; self is the bundle's bot")
	games := fs.Int("games", 2000, "games per opponent")
	effort := fs.Int("effort", 50, "search effort of the bundle's bot (pegging does not affect discards)")
	epsList := fs.String("eps", "0,0.05,0.1,0.2", "softmax weights against the exact table to report")
	seed := fs.Uint64("seed", 900001, "base seed (held out from building)")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel games")
	fs.Parse(args)
	b, err := expert.LoadBundle(*in)
	if err != nil {
		return err
	}
	var eps []float64
	for _, e := range strings.Split(*epsList, ",") {
		var x float64
		if _, err := fmt.Sscanf(e, "%g", &x); err != nil {
			return fmt.Errorf("keepcheck: bad -eps %q", e)
		}
		eps = append(eps, x)
	}
	self := fmt.Sprintf("%s@%d", *in, *effort)
	newA, err := players.Resolve(self)
	if err != nil {
		return err
	}
	fmt.Printf("Mean probability of the real keep (and mean log probability), %d games each, %s\n", *games, *in)
	fmt.Printf("%-22s %8s  %-20s", "opponent", "keeps", "softmax")
	if b.KeepExact != nil {
		for _, e := range eps {
			fmt.Printf("  %-20s", fmt.Sprintf("exact, eps %g", e))
		}
	}
	fmt.Println()
	for _, name := range strings.Split(*vs, ",") {
		spec := name
		if name == "self" {
			spec = self
		}
		newB, err := players.Resolve(spec)
		if err != nil {
			return err
		}
		recs, err := expert.CollectCorpus(ctx, newA, newB, expert.CorpusMeta{Seed: *seed, Games: *games}, *workers)
		if err != nil {
			return err
		}
		type acc struct{ p, lp float64 }
		var soft acc
		ex := make([]acc, len(eps))
		n := 0
		for i := range recs {
			r := &recs[i]
			if r.Discards[1][0] == r.Discards[1][1] { // no discard recorded (cannot happen)
				continue
			}
			side := 1
			if r.Dealer == 1 {
				side = 0
			}
			kept := r.Kept(1)
			n++
			p := b.KeepProb(0, true, side, r.Six[1], kept)
			soft.p += p
			soft.lp += math.Log(max(p, 1e-12))
			if b.KeepExact != nil {
				for j, e := range eps {
					q := b.KeepProb(e, false, side, r.Six[1], kept)
					ex[j].p += q
					ex[j].lp += math.Log(max(q, 1e-12))
				}
			}
		}
		f := func(a acc) string { return fmt.Sprintf("%.3f (%.3f)", a.p/float64(n), a.lp/float64(n)) }
		fmt.Printf("%-22s %8d  %-20s", name, n, f(soft))
		for _, a := range ex {
			fmt.Printf("  %-20s", f(a))
		}
		fmt.Println()
	}
	return nil
}
