// Command expert builds, inspects and evaluates expert model bundles.
//
//	go run ./cmd/expert build -bot models-big/best.bin -games 200000 -out models-expert/expert.bin
//	go run ./cmd/expert inspect models-expert/expert.bin
//	go run ./cmd/expert eval -bundle models-expert/expert.bin -games 200000 -seed 2
//	go run ./cmd/expert iterate -start models-expert/expert.bin -rounds 3
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"cribbage/internal/expert"
	"cribbage/internal/players"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "build":
		err = build(ctx, os.Args[2:])
	case "inspect":
		err = inspect(os.Args[2:])
	case "iterate":
		err = iterate(ctx, os.Args[2:])
	case "eval":
		err = eval(ctx, os.Args[2:])
	case "keepx":
		err = keepx(ctx, os.Args[2:])
	case "keepcheck":
		err = keepcheck(ctx, os.Args[2:])
	case "pegfit":
		err = pegfit(os.Args[2:])
	case "corpus":
		err = corpus(ctx, os.Args[2:])
	case "rollout":
		err = rollout(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "expert:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: expert build|inspect|eval|iterate|rollout|corpus|keepx|keepcheck|pegfit [flags]")
	os.Exit(2)
}

// selfPlay runs games of bot against itself, reporting progress.
func selfPlay(ctx context.Context, bot string, games, workers int, seed uint64) (*expert.SelfPlay, error) {
	newBot, err := players.Resolve(bot)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	fmt.Printf("self-play: %s, %d games, seed %d, %d workers\n", bot, games, seed, workers)
	sp, err := expert.Collect(ctx, newBot, games, workers, seed)
	if err != nil {
		return nil, err
	}
	fmt.Printf("  %d games, %d hands recorded, in %v\n", sp.Games, sp.Hands.Len(), time.Since(start).Round(time.Second))
	return sp, nil
}

func build(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var spec buildSpec
	fs.StringVar(&spec.bot, "bot", "models-big/best.bin", "bot whose self-play the tables are built from")
	tables := fs.String("tables", "win,discard,keep", "tables to build: win, discard (crib, pegging and show), keep (opponent discard model)")
	fs.StringVar(&spec.base, "base", "", "bundle to take the tables not being built from")
	fs.IntVar(&spec.games, "games", 500000, "self-play games")
	fs.IntVar(&spec.workers, "workers", runtime.NumCPU(), "parallel games")
	fs.Uint64Var(&spec.seed, "seed", 1, "base seed")
	out := fs.String("out", "models-expert/expert.bin", "bundle file to write")
	fs.IntVar(&spec.window, "window", expert.DefaultWindowHands, "hands used to value each score (fewer: more local, noisier)")
	fs.Parse(args)

	spec.want = map[string]bool{}
	for _, t := range strings.Split(*tables, ",") {
		if t != "win" && t != "discard" && t != "keep" {
			return fmt.Errorf("unknown table set %q", t)
		}
		spec.want[t] = true
	}
	b, err := makeBundle(ctx, spec)
	if err != nil {
		return err
	}
	if err := b.Save(*out); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

// buildSpec says which tables to build from whose self-play.
type buildSpec struct {
	bot     string
	want    map[string]bool // win, discard, keep
	base    string          // bundle supplying the other tables
	games   int
	workers int
	seed    uint64
	window  int
}

func makeBundle(ctx context.Context, spec buildSpec) (*expert.Bundle, error) {
	want, bot := spec.want, &spec.bot
	b := &expert.Bundle{Meta: expert.Meta{Sources: map[string]string{}, Metrics: map[string]float64{}}}
	if spec.base != "" {
		base, err := expert.LoadBundle(spec.base)
		if err != nil {
			return nil, err
		}
		b.Win, b.Discard, b.KeepTemp = base.Win, base.Discard, base.KeepTemp
		for k, v := range base.Meta.Metrics {
			b.Meta.Metrics[k] = v
		}
		for k, v := range base.Meta.Sources {
			b.Meta.Sources[k] = v
		}
		if b.Meta.Sources["win"] == "" {
			b.Meta.Sources["win"] = base.Meta.Source
		}
	}
	if !want["win"] && b.Win == nil {
		return nil, fmt.Errorf("no win table: build it or give -base")
	}

	sp, err := selfPlay(ctx, *bot, spec.games, spec.workers, spec.seed)
	if err != nil {
		return nil, err
	}
	if want["win"] {
		start := time.Now()
		win, err := expert.BuildWinTable(&sp.Hands, spec.window)
		if err != nil {
			return nil, err
		}
		fmt.Printf("win table built in %v\n", time.Since(start).Round(time.Millisecond))
		viol, _ := win.Monotonicity()
		_, loss, base := win.Calibration(&sp.Outcomes, 10, 1)
		b.Win = win
		b.Meta.Sources["win"] = *bot
		b.Meta.Metrics["win_monotone_violations"] = float64(viol)
		b.Meta.Metrics["win_window_hands"] = float64(spec.window)
		b.Meta.Metrics["win_log_loss"] = loss
		b.Meta.Metrics["win_base_log_loss"] = base
	}
	if want["discard"] {
		b.Discard = expert.BuildDiscardTables(sp.Details)
		b.Meta.Sources["discard"] = *bot
		b.Meta.Metrics["discard_hands"] = float64(len(sp.Details))
		fmt.Printf("discard tables built from %d hands\n", len(sp.Details))
	}
	if want["keep"] {
		if b.Discard == nil {
			return nil, fmt.Errorf("the keep model needs discard tables: build them or give -base")
		}
		temps, ll := expert.FitKeepTemps(b.Discard, sp.Details)
		b.KeepTemp = temps
		b.Meta.Sources["keep"] = *bot
		b.Meta.Metrics["keep_temp_dealer"], b.Meta.Metrics["keep_temp_pone"] = temps[0], temps[1]
		b.Meta.Metrics["keep_loglik_dealer"], b.Meta.Metrics["keep_loglik_pone"] = ll[0], ll[1]
		fmt.Printf("keep model: temperature %.3f (dealer), %.3f (pone); mean log-likelihood %.3f, %.3f\n", temps[0], temps[1], ll[0], ll[1])
	}
	b.Meta.Saved = time.Now().UTC().Format(time.RFC3339)
	b.Meta.Source, b.Meta.Games, b.Meta.Hands, b.Meta.Seed = *bot, sp.Games, sp.Hands.Len(), spec.seed
	return b, nil
}

func inspect(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: expert inspect FILE")
	}
	b, err := expert.LoadBundle(args[0])
	if err != nil {
		return err
	}
	m := b.Meta
	fmt.Printf("%s: bundle v%d, saved %s\n  built from %d games of %s (seed %d), %d hands\n",
		args[0], m.Version, m.Saved, m.Games, m.Source, m.Seed, m.Hands)
	for _, t := range m.Tables {
		fmt.Printf("  table %-8s %v\n", t.Name, t.Shape)
	}
	for k, v := range m.Sources {
		fmt.Printf("  %s table from %s\n", k, v)
	}
	for k, v := range m.Metrics {
		fmt.Printf("  %-26s %g\n", k, v)
	}
	if d := b.Discard; d != nil {
		fmt.Println("\naverage crib (over starter ranks) for a discarded pair:")
		fmt.Printf("  %-6s %8s %8s\n", "pair", "dealer", "pone")
		for _, p := range [][2]int{{5, 5}, {5, 11}, {5, 10}, {2, 3}, {7, 8}, {1, 4}, {10, 13}, {9, 13}, {13, 13}} {
			fmt.Printf("  %2d-%-3d %8.2f %8.2f\n", p[0], p[1], d.CribMean(0, p[0], p[1]), d.CribMean(1, p[0], p[1]))
		}
		fmt.Println("average pegging, mine vs theirs, for a kept shape:")
		for _, k := range [][4]int{{1, 2, 3, 4}, {5, 5, 10, 10}, {7, 7, 8, 8}, {10, 11, 12, 13}, {2, 4, 9, 13}} {
			fmt.Printf("  %v  dealer %.2f vs %.2f   pone %.2f vs %.2f\n", k,
				d.PegMean(0, k, 0), d.PegMean(0, k, 1), d.PegMean(1, k, 0), d.PegMean(1, k, 1))
		}
	}
	w := b.Win
	viol, worst := w.Monotonicity()
	fmt.Printf("\nmonotonicity violations: %d (worst %.4f)\n", viol, worst)
	fmt.Printf("first dealer wins: %.2f%%\n", 100*w.Win(0, 0, true))
	fmt.Println("\nP(dealer wins), rows = dealer's score, columns = pone's score:")
	fmt.Printf("%5s", "")
	for y := 0; y < expert.Scores; y += 10 {
		fmt.Printf("%6d", y)
	}
	fmt.Println()
	for x := 0; x < expert.Scores; x += 10 {
		fmt.Printf("%5d", x)
		for y := 0; y < expert.Scores; y += 10 {
			fmt.Printf("%6.1f", 100*w.V[x][y])
		}
		fmt.Println()
	}
	return nil
}

func eval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	path := fs.String("bundle", "models-expert/expert.bin", "bundle to evaluate")
	bot := fs.String("bot", "", "bot whose self-play to compare with (default: the bundle's source)")
	games := fs.Int("games", 500000, "self-play games")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel games")
	seed := fs.Uint64("seed", 2, "base seed; use one the bundle was not built from")
	size := fs.Int("bin", 10, "score bin size")
	minN := fs.Int64("min", 2000, "smallest bin to report")
	fs.Parse(args)

	b, err := expert.LoadBundle(*path)
	if err != nil {
		return err
	}
	if *bot == "" {
		*bot = b.Meta.Source
	}
	if *seed == b.Meta.Seed && *bot == b.Meta.Source {
		fmt.Println("warning: same bot and seed as the bundle was built from; this is an in-sample check")
	}
	sp, err := selfPlay(ctx, *bot, *games, *workers, *seed)
	if err != nil {
		return err
	}
	bins, loss, base := b.Win.Calibration(&sp.Outcomes, *size, *minN)
	within, chi2 := 0, 0.0
	for _, x := range bins {
		if math.Abs(x.Z) < 1.96 {
			within++
		}
		chi2 += x.Z * x.Z
	}
	fmt.Printf("\nwin table vs observed results, %d bins of %dx%d scores with at least %d hand starts:\n", len(bins), *size, *size, *minN)
	fmt.Printf("  within 95%% interval: %d of %d (%.1f%%; 95%% expected)\n", within, len(bins), 100*float64(within)/float64(max(1, len(bins))))
	fmt.Printf("  chi-squared / bins:  %.2f (about 1 if calibrated)\n", chi2/float64(max(1, len(bins))))
	fmt.Printf("  log loss:            %.4f (constant prediction: %.4f)\n", loss, base)
	fmt.Println("  worst bins (dealer score, pone score):")
	for _, x := range expert.WorstBins(bins, 8) {
		fmt.Printf("    %3d-%-3d %3d-%-3d  n %-8d predicted %5.1f%%  observed %5.1f%%  z %+.2f\n",
			x.X, min(x.X+*size, expert.Scores)-1, x.Y, min(x.Y+*size, expert.Scores)-1, x.N, 100*x.Predicted, 100*x.Observed, x.Z)
	}
	return nil
}
