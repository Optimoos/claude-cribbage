package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cribbage/internal/expert"
)

// wintable rebuilds a bundle's win table where both scores are high
// (proposal 6): it builds win tables from corpus hands (usually games
// started at chosen scores, E4) at several window sizes, splices each into
// the base bundle's table where both scores are at least -min, and
// reports each table's log loss on held-out hands in that region. The
// best splice is saved.
func wintable(args []string) error {
	fs := flag.NewFlagSet("wintable", flag.ExitOnError)
	build := fs.String("corpus", "", "corpus files to build from, comma-separated (required)")
	test := fs.String("test", "", "held-out corpus files (required)")
	in := fs.String("bundle", "models-expert/expert.bin", "base bundle")
	out := fs.String("out", "", "bundle to write with the best splice (none: report only)")
	windows := fs.String("windows", "5000,2000,1000", "window sizes (hands) to try")
	lo := fs.Int("min", 70, "replace the table where both scores are at least this")
	fs.Parse(args)
	if *build == "" || *test == "" {
		return fmt.Errorf("wintable: -corpus and -test are required")
	}
	b, err := expert.LoadBundle(*in)
	if err != nil {
		return err
	}
	sp, err := corpusSelfPlay(*build)
	if err != nil {
		return err
	}
	tp, err := corpusSelfPlay(*test)
	if err != nil {
		return err
	}
	// Held-out outcomes in the region only.
	var region expert.Outcomes
	for x := *lo; x < expert.Scores; x++ {
		for y := *lo; y < expert.Scores; y++ {
			region.N[x][y], region.DealerWins[x][y] = tp.Outcomes.N[x][y], tp.Outcomes.DealerWins[x][y]
		}
	}
	report := func(name string, w *expert.WinTable) float64 {
		bins, loss, base := w.Calibration(&region, 10, 200)
		var maxZ float64
		for _, bn := range bins {
			maxZ = max(maxZ, bn.Z, -bn.Z)
		}
		fmt.Printf("%-24s held-out log loss %.5f (constant %.5f), worst 10x10 bin |z| %.1f\n", name, loss, base, maxZ)
		return loss
	}
	report("base", b.Win)
	best, bestLoss := (*expert.WinTable)(nil), 0.0
	bestName := ""
	for _, ws := range strings.Split(*windows, ",") {
		n, err := strconv.Atoi(ws)
		if err != nil {
			return err
		}
		t0 := time.Now()
		w, err := expert.BuildWinTable(&sp.Hands, n)
		if err != nil {
			return err
		}
		spliced := *b.Win
		for x := *lo; x < expert.Scores; x++ {
			for y := *lo; y < expert.Scores; y++ {
				spliced.V[x][y] = w.V[x][y]
			}
		}
		name := fmt.Sprintf("window %d (%v)", n, time.Since(t0).Round(time.Second))
		if l := report(name, &spliced); best == nil || l < bestLoss {
			best, bestLoss, bestName = &spliced, l, ws
		}
	}
	if *out == "" {
		return nil
	}
	b.Win = best
	b.Meta.Notes = strings.TrimSpace(b.Meta.Notes + fmt.Sprintf(" win: rebuilt where both scores >= %d from %s, window %s.", *lo, *build, bestName))
	b.Meta.Saved = time.Now().UTC().Format(time.RFC3339)
	if err := b.Save(*out); err != nil {
		return err
	}
	fmt.Printf("saved %s (window %s)\n", *out, bestName)
	return nil
}
