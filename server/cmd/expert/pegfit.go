package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"cribbage/internal/expert"
)

// pegfit fits the opponent pegging policy (proposal 2) to corpus plays and
// reports its log-likelihood on held-out corpora against the softmax on
// immediate points.
func pegfit(args []string) error {
	fs := flag.NewFlagSet("pegfit", flag.ExitOnError)
	train := fs.String("train", "", "corpus files to fit to, comma-separated; FILE:0 or FILE:1 takes one seat's plays only")
	test := fs.String("test", "", "held-out corpus files, the same form, each reported on its own")
	in := fs.String("bundle", "models-expert/expert.bin", "bundle to add the policy to")
	out := fs.String("out", "", "bundle to write (none: report only)")
	l2 := fs.Float64("l2", 1e-4, "ridge penalty per decision")
	fs.Parse(args)
	if *train == "" {
		return fmt.Errorf("pegfit: -train is required")
	}
	load := func(spec string) ([]expert.PegDecision, error) {
		var all []expert.PegDecision
		for _, f := range strings.Split(spec, ",") {
			path, seats := f, uint8(3)
			if i := strings.LastIndexByte(f, ':'); i >= 0 {
				path = f[:i]
				switch f[i+1:] {
				case "0":
					seats = 1
				case "1":
					seats = 2
				default:
					return nil, fmt.Errorf("pegfit: bad seat in %q", f)
				}
			}
			_, recs, err := expert.LoadCorpus(path)
			if err != nil {
				return nil, err
			}
			all = append(all, expert.PegDecisions(recs, seats)...)
		}
		return all, nil
	}
	t0 := time.Now()
	ds, err := load(*train)
	if err != nil {
		return err
	}
	fmt.Printf("fitting to %d decisions from %s\n", len(ds), *train)
	base := expert.PegPolicy{}
	base[0] = 1 / expert.DefaultPegTemp
	fit := expert.FitPegPolicy(ds, base, *l2, 30)
	fmt.Printf("fitted in %v:\n%v", time.Since(t0).Round(time.Millisecond), fit)
	temps := []float64{0.5, 0.7, 1, 1.5, 2, 3}
	report := func(name string, ds []expert.PegDecision) {
		fmt.Printf("%-40s %8d  fitted %.4f", name, len(ds), fit.LogLik(ds))
		for _, t := range temps {
			p := expert.PegPolicy{}
			p[0] = 1 / t
			fmt.Printf("  t%g %.4f", t, p.LogLik(ds))
		}
		fmt.Println()
	}
	fmt.Println("mean log-likelihood per decision (fitted policy; softmax on points at temperature t):")
	report("train", ds)
	if *test != "" {
		for _, f := range strings.Split(*test, ",") {
			td, err := load(f)
			if err != nil {
				return err
			}
			report(f, td)
		}
	}
	if *out == "" {
		return nil
	}
	b, err := expert.LoadBundle(*in)
	if err != nil {
		return err
	}
	b.PegPolicy = &fit
	b.Meta.Notes = strings.TrimSpace(b.Meta.Notes + fmt.Sprintf(" pegpol: fitted to %s.", *train))
	b.Meta.Saved = time.Now().UTC().Format(time.RFC3339)
	if err := b.Save(*out); err != nil {
		return err
	}
	fmt.Printf("saved %s\n", *out)
	return nil
}
