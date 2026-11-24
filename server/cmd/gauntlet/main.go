// Command gauntlet plays one bot against a list of opponents in
// duplicate-deal matches and prints one table: paired win rate and
// points margin with 95% intervals, and the bot's decision latency.
//
//	go run ./cmd/gauntlet -bot models-ft/best.bin -vs random,heuristic -n 100000
//
// A player is a registered bot name (random, heuristic) or the path of a
// network model file, which plays as the nn bot does.
//
// Coupled comparison: -save-pairs DIR records every deal of a baseline's
// matches; a later run of a candidate with -base DIR, the same opponents,
// seed and n, reports the candidate's change deal by deal. Deals both
// played the same way differ by exactly zero, so small changes resolve
// with far fewer games.
//
// Sequential test: -sprt LO,HI stops each match once it can tell a win
// rate of 50+LO from 50+HI percent (or, with -base, a change of LO from HI
// points), at error rates -alpha and -beta; n is then the most games.
//
// Seeds below 900000 are for building tables; evaluate on the default
// 900001 and above, so tables are never scored on the deals they came from.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/players"
)

func main() {
	bot := flag.String("bot", "heuristic", "the bot under test")
	vs := flag.String("vs", "random,heuristic,models-ft/best.bin", "comma-separated opponents")
	n := flag.Int("n", 20000, "games per opponent (each deal is played twice, seats swapped)")
	workers := flag.Int("workers", runtime.NumCPU(), "parallel games")
	seed := flag.Uint64("seed", 900001, "base seed; the same seed replays the same deals (below 900000 is for building tables)")
	var c config
	flag.StringVar(&c.savePairs, "save-pairs", "", "save every deal's result per opponent in this directory, as a baseline for -base")
	flag.StringVar(&c.base, "base", "", "compare deal by deal with the baseline saved in this directory by -save-pairs")
	sprt := flag.String("sprt", "", "LO,HI in percentage points: stop each match when it can tell LO from HI (win rate less 50, or with -base the change in win rate)")
	flag.Float64Var(&c.alpha, "alpha", 0.005, "sequential test: chance of accepting HI when LO is true (0.05 over ten candidates)")
	flag.Float64Var(&c.beta, "beta", 0.05, "sequential test: chance of accepting LO when HI is true")
	flag.IntVar(&c.minPairs, "min-pairs", 2000, "sequential test: pairs before any decision")
	start := flag.String("start", "", "LO,HI: start every game at a random hand with each score in [LO, HI], to test the endgame")
	flag.Parse()
	if *start != "" {
		if _, err := fmt.Sscanf(*start, "%d,%d", &c.startMin, &c.startMax); err != nil || c.startMin < 0 || c.startMax > 120 || c.startMin > c.startMax || c.startMax == 0 {
			fmt.Fprintln(os.Stderr, "gauntlet: -start wants LO,HI within 0..120")
			os.Exit(2)
		}
	}
	if *sprt != "" {
		if _, err := fmt.Sscanf(*sprt, "%g,%g", &c.lo, &c.hi); err != nil || c.lo >= c.hi {
			fmt.Fprintln(os.Stderr, "gauntlet: -sprt wants LO,HI with LO < HI")
			os.Exit(2)
		}
		c.sprt = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *bot, splitSpecs(*vs), *n, *workers, *seed, c); err != nil {
		fmt.Fprintln(os.Stderr, "gauntlet:", err)
		os.Exit(1)
	}
}

// config holds the options for coupled and sequential testing.
type config struct {
	savePairs, base string
	sprt            bool
	lo, hi          float64 // percentage points
	alpha, beta     float64
	minPairs        int
	startMin        int // with startMax > 0, games start at random scores in [startMin, startMax]
	startMax        int
}

// pairPath names the file holding the deals against one opponent.
func pairPath(dir, opponent string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "@", "_", ",", "_", ":", "_")
	return filepath.Join(dir, r.Replace(opponent)+".csv")
}

func run(ctx context.Context, bot string, opponents []string, n, workers int, seed uint64, c config) error {
	newA, err := players.Resolve(bot)
	if err != nil {
		return err
	}
	if c.savePairs != "" {
		if err := os.MkdirAll(c.savePairs, 0o755); err != nil {
			return err
		}
	}
	type row struct {
		name     string
		r        ai.ArenaResult
		elapsed  time.Duration
		coupled  *ai.Coupled
		decision string
	}
	var rows []row
	var discard, play ai.Latency
	fmt.Printf("%s: %d games per opponent, seed %d, %d workers\n", bot, n, seed, workers)
	if c.startMax > 0 {
		fmt.Printf("every game starts at a random hand with scores in [%d, %d]\n", c.startMin, c.startMax)
	}
	for _, name := range opponents {
		name = strings.TrimSpace(name)
		newB, err := players.Resolve(name)
		if err != nil {
			return err
		}
		var base map[int64]ai.Pair
		var coupled *ai.Coupled
		if c.base != "" {
			f, err := ai.LoadPairFile(pairPath(c.base, name))
			if err != nil {
				return fmt.Errorf("baseline vs %s: %w", name, err)
			}
			if f.Seed != seed || len(f.Pairs) < (n+1)/2 {
				return fmt.Errorf("baseline vs %s has seed %d and %d pairs; this run wants seed %d and %d", name, f.Seed, len(f.Pairs), seed, (n+1)/2)
			}
			base = make(map[int64]ai.Pair, len(f.Pairs))
			for _, p := range f.Pairs {
				base[p.Deal] = p
			}
			coupled = new(ai.Coupled)
		}
		var test *ai.SPRT
		if c.sprt {
			mu := 0.5 // a win fraction against the opponent
			if base != nil {
				mu = 0 // a change in it
			}
			test = &ai.SPRT{Mu0: mu + c.lo/100, Mu1: mu + c.hi/100, Alpha: c.alpha, Beta: c.beta, MinPairs: c.minPairs}
		}
		save := &ai.PairFile{Bot: bot, Opponent: name, Seed: seed}
		mctx, cancel := context.WithCancel(ctx)
		decision := ""
		onPair := func(p ai.Pair) {
			if c.savePairs != "" {
				save.Pairs = append(save.Pairs, p)
			}
			x := float64(p.WinsA) / 2
			if base != nil {
				b := base[p.Deal]
				coupled.Add(p, b)
				x = float64(p.WinsA-b.WinsA) / 2
			}
			if test != nil && decision == "" {
				test.Add(x)
				switch test.Decision() {
				case 1:
					decision = fmt.Sprintf("H1 (%+g) after %d pairs", c.hi, test.N())
					cancel()
				case -1:
					decision = fmt.Sprintf("H0 (%+g) after %d pairs", c.lo, test.N())
					cancel()
				}
			}
		}
		timed := make([]*ai.Timed, workers)
		start := time.Now()
		r, err := ai.ArenaFrom(mctx,
			func(w int) bots.Bot { timed[w] = &ai.Timed{Bot: newA(w)}; return timed[w] },
			newB, n, workers, seed, c.startMin, c.startMax, onPair)
		cancel()
		for _, t := range timed {
			if t != nil {
				discard.Add(&t.Discard)
				play.Add(&t.Play)
			}
		}
		if err != nil && ctx.Err() == nil && mctx.Err() == nil {
			return fmt.Errorf("vs %s: %w", name, err)
		}
		if test != nil && decision == "" {
			decision = fmt.Sprintf("undecided after %d pairs (LLR %.2f)", test.N(), test.LLR())
		}
		if c.savePairs != "" {
			if err := save.Save(pairPath(c.savePairs, name)); err != nil {
				return err
			}
		}
		rows = append(rows, row{name, r, time.Since(start), coupled, decision})
		fmt.Fprintf(os.Stderr, "  vs %s done in %v\n", name, time.Since(start).Round(time.Second))
		if ctx.Err() != nil {
			fmt.Println("interrupted; partial results")
			break
		}
	}

	fmt.Println("\n± is the paired 95% interval; (± ...) is what independent deals would give.")
	fmt.Printf("%-24s %8s  %-24s  %-24s  %-15s  %8s\n", "opponent", "games", "win %", "margin per game", "pegging/hand", "games/s")
	for _, x := range rows {
		rate, ci := x.r.WinRate()
		m, mci := x.r.Margin()
		p, pci := x.r.PegMargin()
		fmt.Printf("%-24s %8d  %6.2f ± %-5.2f %-9s  %+6.2f ± %-5.2f %-9s  %+6.3f ± %-5.3f  %8.0f\n",
			x.name, x.r.Games, 100*rate, 100*ci, fmt.Sprintf("(± %.2f)", 100*x.r.UnpairedCI()),
			m, mci, fmt.Sprintf("(± %.2f)", x.r.UnpairedMarginCI()), p, pci,
			float64(x.r.Games)/x.elapsed.Seconds())
	}
	for _, x := range rows {
		if x.coupled != nil || x.decision != "" {
			if c.base != "" {
				fmt.Printf("\nChange from the baseline in %s, deal by deal (95%% intervals):\n", c.base)
			} else {
				fmt.Println("\nSequential test (no baseline: the change columns are empty):")
			}
			fmt.Printf("%-24s %8s %9s  %-16s  %-16s  %-17s  %s\n", "opponent", "pairs", "deviated", "win %", "margin per game", "pegging/hand", "sequential test")
			break
		}
	}
	for _, x := range rows {
		if x.coupled == nil && x.decision == "" {
			continue
		}
		var k ai.Coupled
		if x.coupled != nil {
			k = *x.coupled
		}
		w, wci := k.WinRate()
		m, mci := k.Margin()
		p, pci := k.PegMargin()
		fmt.Printf("%-24s %8d %9d  %+6.2f ± %-6.2f  %+6.2f ± %-6.2f  %+7.3f ± %-6.3f  %s\n",
			x.name, k.Pairs, k.Deviated, 100*w, 100*wci, m, mci, p, pci, x.decision)
	}
	fmt.Printf("\n%s decision latency (under %d-way load):\n", bot, workers)
	for _, l := range []struct {
		phase string
		l     *ai.Latency
	}{{"discard", &discard}, {"pegging", &play}} {
		fmt.Printf("  %-8s n %-10d mean %-10v p50 %-10v p99 %-10v max %v\n", l.phase, l.l.N,
			l.l.Mean(), l.l.Quantile(0.5), l.l.Quantile(0.99), l.l.Max)
	}
	return nil
}

// splitSpecs splits a comma-separated list of player specs. Expert options
// also use commas (bundle@100,nokeep), so a piece that is an option joins
// the spec before it.
func splitSpecs(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		_, numErr := strconv.Atoi(p)
		isOpt := numErr == nil || p == "nokeep" || p == "nopeg" || p == "heurdiscard" || strings.HasPrefix(p, "pegtemp=") || strings.HasPrefix(p, "oracle=") || p == "noexact" || p == "nofixes" || p == "is" || p == "nopegpol" || strings.HasPrefix(p, "islambda=") || strings.HasPrefix(p, "keepeps=")
		if isOpt && len(out) > 0 && strings.Contains(out[len(out)-1], "@") {
			out[len(out)-1] += "," + p
			continue
		}
		out = append(out, p)
	}
	return out
}
