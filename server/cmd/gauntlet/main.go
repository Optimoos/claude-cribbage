// Command gauntlet plays one bot against a list of opponents in
// duplicate-deal matches and prints one table: paired win rate and
// points margin with 95% intervals, and the bot's decision latency.
//
//	go run ./cmd/gauntlet -bot models-ft/best.bin -vs random,heuristic -n 100000
//
// A player is a registered bot name (random, heuristic) or the path of a
// network model file, which plays as the nn bot does.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
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
	seed := flag.Uint64("seed", 1, "base seed; the same seed replays the same deals")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *bot, strings.Split(*vs, ","), *n, *workers, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "gauntlet:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, bot string, opponents []string, n, workers int, seed uint64) error {
	newA, err := players.Resolve(bot)
	if err != nil {
		return err
	}
	type row struct {
		name    string
		r       ai.ArenaResult
		elapsed time.Duration
	}
	var rows []row
	var discard, play ai.Latency
	fmt.Printf("%s: %d games per opponent, seed %d, %d workers\n", bot, n, seed, workers)
	for _, name := range opponents {
		name = strings.TrimSpace(name)
		newB, err := players.Resolve(name)
		if err != nil {
			return err
		}
		timed := make([]*ai.Timed, workers)
		start := time.Now()
		r, err := ai.Arena(ctx,
			func(w int) bots.Bot { timed[w] = &ai.Timed{Bot: newA(w)}; return timed[w] },
			newB, n, workers, seed)
		for _, t := range timed {
			if t != nil {
				discard.Add(&t.Discard)
				play.Add(&t.Play)
			}
		}
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("vs %s: %w", name, err)
		}
		rows = append(rows, row{name, r, time.Since(start)})
		fmt.Fprintf(os.Stderr, "  vs %s done in %v\n", name, time.Since(start).Round(time.Second))
		if ctx.Err() != nil {
			fmt.Println("interrupted; partial results")
			break
		}
	}

	fmt.Println("\n± is the paired 95% interval; (± ...) is what independent deals would give.")
	fmt.Printf("%-24s %8s  %-24s  %-24s  %8s\n", "opponent", "games", "win %", "margin per game", "games/s")
	for _, x := range rows {
		rate, ci := x.r.WinRate()
		m, mci := x.r.Margin()
		fmt.Printf("%-24s %8d  %6.2f ± %-5.2f %-9s  %+6.2f ± %-5.2f %-9s  %8.0f\n",
			x.name, x.r.Games, 100*rate, 100*ci, fmt.Sprintf("(± %.2f)", 100*x.r.UnpairedCI()),
			m, mci, fmt.Sprintf("(± %.2f)", x.r.UnpairedMarginCI()),
			float64(x.r.Games)/x.elapsed.Seconds())
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
