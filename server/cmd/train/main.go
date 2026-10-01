// Command train trains and evaluates the neural network player.
//
//	go run ./cmd/train run -out models                 # train until Ctrl-C
//	go run ./cmd/train run -out models -init models/latest.bin
//	go run ./cmd/train eval -model models/best.bin -vs heuristic -n 10000
//	go run ./cmd/train eval -model models/best.bin -vs models/ckpt-0000100000.bin
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/ai/nn"
	"cribbage/internal/ai/train"
	"cribbage/internal/bots"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: train run [flags] | train eval [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "eval":
		err = evalCmd(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("bad layer size %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}

func runCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c train.Config
	hidden := fs.String("hidden", "128,64", "hidden layer sizes")
	fs.StringVar(&c.Out, "out", "models", "directory for checkpoints and train.csv")
	fs.StringVar(&c.Init, "init", "", "model to continue training from")
	fs.Uint64Var(&c.Seed, "seed", uint64(time.Now().UnixNano()), "random seed")
	fs.IntVar(&c.Actors, "actors", 0, "self-play goroutines (default NumCPU - learner threads)")
	fs.IntVar(&c.LearnerThreads, "learner-threads", 4, "goroutines computing gradients")
	fs.IntVar(&c.Batch, "batch", 512, "minibatch size")
	fs.Float64Var(&c.LR, "lr", 1e-3, "Adam learning rate")
	fs.Float64Var(&c.Lambda, "lambda", 0.7, "TD(lambda)")
	fs.Float64Var(&c.EpsilonStart, "epsilon-start", 0.1, "initial exploration rate")
	fs.Float64Var(&c.EpsilonEnd, "epsilon-end", 0.02, "final exploration rate")
	fs.Int64Var(&c.EpsilonGames, "epsilon-games", 500_000, "games over which exploration decays")
	fs.Float64Var(&c.MixHeuristic, "mix-heuristic", 0.1, "fraction of games against the heuristic bot (negative for none)")
	fs.IntVar(&c.ReplaySize, "replay", 300_000, "replay buffer size in samples")
	fs.Float64Var(&c.ReplayRatio, "replay-ratio", 1, "samples trained per sample generated")
	fs.Int64Var(&c.EvalEvery, "eval-every", 25_000, "games between evaluations and checkpoints")
	fs.IntVar(&c.EvalGames, "eval-games", 2000, "games per evaluation opponent")
	fs.Int64Var(&c.Games, "games", 0, "stop after this many games (0 = until Ctrl-C)")
	fs.Parse(args)
	var err error
	if c.Hidden, err = parseInts(*hidden); err != nil {
		return err
	}
	c.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	return train.Run(ctx, c)
}

func loadAgent(path string) (func(int) bots.Bot, nn.Meta, error) {
	net, meta, err := nn.Load(path)
	if err != nil {
		return nil, meta, err
	}
	if err := ai.CheckNet(net, meta); err != nil {
		return nil, meta, err
	}
	return func(w int) bots.Bot { return ai.NewAgent(net, 0, uint64(w)) }, meta, nil
}

func evalCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	model := fs.String("model", "models/best.bin", "model to evaluate")
	vs := fs.String("vs", "heuristic", "opponent: a bot name ("+strings.Join(bots.Names(), ", ")+") or a model file")
	n := fs.Int("n", 10000, "games")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel games")
	seed := fs.Uint64("seed", 1, "base seed")
	lookahead := fs.Bool("lookahead", false, "average discards over possible starters")
	fs.Parse(args)

	newA, meta, err := loadAgent(*model)
	if err == nil && *lookahead {
		plain := newA
		newA = func(w int) bots.Bot { a := plain(w).(*ai.Agent); a.Lookahead = true; return a }
	}
	if err != nil {
		return err
	}
	var newB func(int) bots.Bot
	if _, err := bots.New(*vs, 0); err == nil {
		name := *vs
		newB = func(w int) bots.Bot { b, _ := bots.New(name, uint64(w)); return b }
	} else if newB, _, err = loadAgent(*vs); err != nil {
		return fmt.Errorf("opponent %q is neither a bot nor a loadable model: %w", *vs, err)
	}
	fmt.Printf("model %s: %d games trained, sizes %v, saved %s\n", *model, meta.Games, meta.Sizes, meta.Saved)
	start := time.Now()
	r, err := ai.Arena(ctx, newA, newB, *n, *workers, *seed)
	if err != nil && ctx.Err() == nil {
		return err
	}
	rate, ci := r.WinRate()
	fmt.Printf("vs %s: won %d of %d (%.2f%% ± %.2f%%), avg score %.1f to %.1f, %.0f games/s\n",
		*vs, r.WinsA, r.Games, 100*rate, 100*ci,
		float64(r.PointsA)/float64(max(1, r.Games)), float64(r.PointsB)/float64(max(1, r.Games)),
		float64(r.Games)/time.Since(start).Seconds())
	return nil
}
