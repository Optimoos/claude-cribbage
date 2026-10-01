// Package train improves a value network by self-play.
//
// Actors play games with the latest published weights, choosing moves by
// afterstate value with epsilon-greedy exploration. When a game ends, every
// decision of each player becomes a training sample whose target is the
// TD(lambda) return: a blend of later value estimates and the final result
// (1 for a win, 0 for a loss). A learner samples minibatches from a replay
// buffer, takes Adam steps (gradients computed in parallel), and
// periodically publishes the new weights to the actors.
package train

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/ai/encode"
	"cribbage/internal/ai/nn"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// Config controls a training run. Zero values get sensible defaults.
type Config struct {
	Out    string // directory for checkpoints and train.csv
	Hidden []int  // hidden layer sizes; default 128,64
	Init   string // model file to continue from
	Seed   uint64

	Actors         int     // self-play goroutines; default NumCPU-LearnerThreads
	LearnerThreads int     // goroutines computing gradients; default 4
	Batch          int     // minibatch size; default 512
	LR             float64 // Adam learning rate; default 1e-3
	Lambda         float64 // TD(lambda); default 0.7
	EpsilonStart   float64 // exploration at the start; default 0.1
	EpsilonEnd     float64 // exploration after EpsilonGames; default 0.02
	EpsilonGames   int64   // games over which epsilon decays; default 500k
	MixHeuristic   float64 // fraction of games against the heuristic bot; default 0.1
	ReplaySize     int     // replay buffer capacity in samples; default 300k
	ReplayRatio    float64 // samples trained per sample generated; default 1
	PublishEvery   int     // learner steps between weight publications; default 10

	EvalEvery int64 // games between evaluations; default 25k
	EvalGames int   // games per evaluation opponent; default 2000
	Games     int64 // stop after this many games; 0 runs until ctx ends

	Logger *slog.Logger
}

func (c *Config) defaults() {
	setInt := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	setF := func(p *float64, v float64) {
		if *p == 0 {
			*p = v
		}
	}
	if len(c.Hidden) == 0 {
		c.Hidden = []int{128, 64}
	}
	setInt(&c.LearnerThreads, 4)
	setInt(&c.Actors, max(1, runtime.NumCPU()-c.LearnerThreads))
	setInt(&c.Batch, 512)
	setF(&c.LR, 1e-3)
	setF(&c.Lambda, 0.7)
	setF(&c.EpsilonStart, 0.1)
	setF(&c.EpsilonEnd, 0.02)
	if c.EpsilonGames == 0 {
		c.EpsilonGames = 500_000
	}
	if c.MixHeuristic == 0 {
		c.MixHeuristic = 0.1
	}
	if c.MixHeuristic < 0 {
		c.MixHeuristic = 0
	}
	setInt(&c.ReplaySize, 300_000)
	setF(&c.ReplayRatio, 1)
	setInt(&c.PublishEvery, 10)
	if c.EvalEvery == 0 {
		c.EvalEvery = 25_000
	}
	setInt(&c.EvalGames, 2000)
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Out == "" {
		c.Out = "models"
	}
}

type sample struct {
	x      nn.Sparse
	target float32
}

type step struct {
	x nn.Sparse
	v float32 // value estimated when the move was chosen
}

// lambdaReturns converts one player's decisions into training samples.
// Working backwards from the result z: G_last = z, and
// G_t = (1-lambda)*V(s_{t+1}) + lambda*G_{t+1}.
func lambdaReturns(steps []step, z, lambda float32, out []sample) []sample {
	nextV, nextG := z, z
	start := len(out)
	for i := len(steps) - 1; i >= 0; i-- {
		g := (1-lambda)*nextV + lambda*nextG
		out = append(out, sample{x: steps[i].x, target: g})
		nextV, nextG = steps[i].v, g
	}
	// Keep chronological order (not required, but easier to inspect).
	for i, j := start, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// trainer holds state shared by actors, the learner and the evaluator.
type trainer struct {
	cfg      Config
	log      *slog.Logger
	latest   atomic.Pointer[nn.Net]
	games    atomic.Int64 // total games, including those from Init
	samples  atomic.Int64
	steps    atomic.Int64
	lossEMA  atomic.Uint64 // float64 bits
	baseMeta nn.Meta
}

func (t *trainer) epsilon(games int64) float64 {
	c := t.cfg
	if games >= c.EpsilonGames {
		return c.EpsilonEnd
	}
	f := float64(games) / float64(c.EpsilonGames)
	return c.EpsilonStart + f*(c.EpsilonEnd-c.EpsilonStart)
}

// Run trains until ctx is cancelled or cfg.Games games have been played,
// then writes a final checkpoint.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	t := &trainer{cfg: cfg, log: cfg.Logger}
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x7a17))

	var net *nn.Net
	if cfg.Init != "" {
		n, meta, err := nn.Load(cfg.Init)
		if err != nil {
			return err
		}
		if err := ai.CheckNet(n, meta); err != nil {
			return err
		}
		net, t.baseMeta = n, meta
		t.games.Store(meta.Games)
		t.steps.Store(meta.Steps)
		t.log.Info("continuing from model", "path", cfg.Init, "games", meta.Games, "sizes", n.Sizes())
	} else {
		sizes := append(append([]int{encode.Size}, cfg.Hidden...), 1)
		n, err := nn.New(sizes, rng)
		if err != nil {
			return err
		}
		net = n
		t.log.Info("new model", "sizes", sizes)
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	t.latest.Store(net.Clone())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	episodes := make(chan []sample, 4*cfg.Actors)
	var actors sync.WaitGroup
	for i := 0; i < cfg.Actors; i++ {
		actors.Add(1)
		go func(i int) {
			defer actors.Done()
			t.actor(ctx, uint64(i)+cfg.Seed*1000, episodes)
		}(i)
	}
	go func() {
		actors.Wait()
		close(episodes)
	}()

	ev := newEvaluator(t)
	defer ev.wait()
	start := time.Now()
	startGames := t.games.Load()
	lastLog, lastEval := time.Now(), t.games.Load()
	t.learn(ctx, net, episodes, func() {
		g := t.games.Load()
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			rate := float64(g-startGames) / time.Since(start).Seconds()
			t.log.Info("training", "games", g, "samples", t.samples.Load(), "steps", t.steps.Load(),
				"loss", fmt.Sprintf("%.4f", t.loss()), "games_per_sec", fmt.Sprintf("%.0f", rate),
				"epsilon", fmt.Sprintf("%.3f", t.epsilon(g)))
		}
		if g-lastEval >= cfg.EvalEvery {
			lastEval = g
			ev.start(net.Clone(), g, t.steps.Load())
		}
		if cfg.Games > 0 && g-startGames >= cfg.Games {
			cancel()
		}
	})
	ev.wait()
	// Final evaluation and checkpoint.
	ev.run(net.Clone(), t.games.Load(), t.steps.Load())
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

func (t *trainer) loss() float64 {
	return float64frombits(t.lossEMA.Load())
}

// actor plays self-play games and sends their samples to the learner.
func (t *trainer) actor(ctx context.Context, seed uint64, out chan<- []sample) {
	rng := rand.New(rand.NewPCG(seed, 0xac70))
	agent := ai.NewAgent(t.latest.Load(), 0, seed)
	heur := bots.NewHeuristic()
	var traj [2][]step
	legal := make([]engine.Action, 0, 16)
	var o engine.Observation
	for ctx.Err() == nil {
		agent.SetNet(t.latest.Load())
		agent.Epsilon = t.epsilon(t.games.Load())
		// In mixed games the heuristic bot takes one seat; only the
		// network's decisions are learned from.
		heurSeat := -1
		if rng.Float64() < t.cfg.MixHeuristic {
			heurSeat = rng.IntN(2)
		}
		g := engine.NewGame(rng.Uint64())
		g.SetQuiet(true)
		traj[0], traj[1] = traj[0][:0], traj[1][:0]
		for !g.Over() {
			for p := 0; p < 2 && !g.Over(); p++ {
				if !g.ToAct(p) {
					continue
				}
				g.ObserveInto(p, &o)
				legal = g.AppendLegal(p, legal[:0])
				var a engine.Action
				if p == heurSeat {
					a = heur.Choose(&o, legal)
				} else {
					var x nn.Sparse
					var v float32
					a, x, v = agent.Decide(&o, legal)
					traj[p] = append(traj[p], step{x: x.Clone(), v: v})
				}
				if err := g.Apply(p, a); err != nil {
					t.log.Error("illegal move in self-play", "err", err)
					return
				}
			}
		}
		var samples []sample
		for p := 0; p < 2; p++ {
			z := float32(0)
			if g.Winner() == p {
				z = 1
			}
			samples = lambdaReturns(traj[p], z, float32(t.cfg.Lambda), samples)
		}
		t.games.Add(1)
		select {
		case out <- samples:
		case <-ctx.Done():
			return
		}
	}
}

// learn consumes episodes until the channel closes, training as it goes.
// tick runs after every episode on the learner goroutine.
func (t *trainer) learn(ctx context.Context, net *nn.Net, episodes <-chan []sample, tick func()) {
	cfg := t.cfg
	opt := nn.NewAdam(net, cfg.LR)
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x1ea7))
	replay := make([]sample, 0, cfg.ReplaySize)
	pos := 0
	budget := 0.0

	threads := cfg.LearnerThreads
	grads := make([]*nn.Grad, threads)
	scratch := make([]*nn.Scratch, threads)
	losses := make([]float64, threads)
	for i := range grads {
		grads[i] = net.NewGrad()
		scratch[i] = net.NewScratch()
	}
	batch := make([]int, cfg.Batch)
	ema, haveEMA := 0.0, false

	trainStep := func() {
		for i := range batch {
			batch[i] = rng.IntN(len(replay))
		}
		var wg sync.WaitGroup
		per := (len(batch) + threads - 1) / threads
		for k := 0; k < threads; k++ {
			lo, hi := k*per, min((k+1)*per, len(batch))
			wg.Add(1)
			go func(k, lo, hi int) {
				defer wg.Done()
				g := grads[k]
				g.Zero()
				sum := 0.0
				for _, idx := range batch[lo:hi] {
					s := replay[idx]
					sum += float64(net.Backward(s.x, s.target, scratch[k], g))
				}
				losses[k] = sum
			}(k, lo, hi)
		}
		wg.Wait()
		total := 0.0
		for k := 1; k < threads; k++ {
			grads[0].Add(grads[k])
		}
		for _, l := range losses {
			total += l
		}
		opt.Step(net, grads[0])
		steps := t.steps.Add(1)
		l := total / float64(len(batch))
		if !haveEMA {
			ema, haveEMA = l, true
		} else {
			ema = 0.99*ema + 0.01*l
		}
		t.lossEMA.Store(float64bits(ema))
		if steps%int64(cfg.PublishEvery) == 0 {
			t.latest.Store(net.Clone())
		}
	}

	for ep := range episodes {
		for _, s := range ep {
			if len(replay) < cfg.ReplaySize {
				replay = append(replay, s)
			} else {
				replay[pos] = s
				pos = (pos + 1) % cfg.ReplaySize
			}
		}
		t.samples.Add(int64(len(ep)))
		budget += float64(len(ep)) * cfg.ReplayRatio / float64(cfg.Batch)
		for budget >= 1 && len(replay) >= cfg.Batch && ctx.Err() == nil {
			trainStep()
			budget--
		}
		tick()
	}
}

// evaluator measures snapshots against fixed opponents in the background,
// writes checkpoints and train.csv, and tracks the best model.
type evaluator struct {
	t       *trainer
	mu      sync.Mutex
	running sync.WaitGroup
	busy    atomic.Bool
	best    float64
	csvPath string
}

func newEvaluator(t *trainer) *evaluator {
	e := &evaluator{t: t, best: -1, csvPath: filepath.Join(t.cfg.Out, "train.csv")}
	if m, ok := t.baseMeta.Metrics["vs_heuristic"]; ok {
		e.best = m
	}
	return e
}

func (e *evaluator) start(net *nn.Net, games, steps int64) {
	if !e.busy.CompareAndSwap(false, true) {
		return // previous evaluation still running; skip this one
	}
	e.running.Add(1)
	go func() {
		defer e.running.Done()
		defer e.busy.Store(false)
		e.run(net, games, steps)
	}()
}

func (e *evaluator) wait() { e.running.Wait() }

func (e *evaluator) run(net *nn.Net, games, steps int64) {
	t := e.t
	cfg := t.cfg
	workers := max(2, cfg.LearnerThreads)
	vs := func(opponent string) (float64, float64) {
		r, err := ai.Arena(context.Background(),
			func(w int) bots.Bot { return ai.NewAgent(net, 0, uint64(w)) },
			func(w int) bots.Bot { b, _ := bots.New(opponent, uint64(w)); return b },
			cfg.EvalGames, workers, uint64(games))
		if err != nil {
			t.log.Error("evaluation failed", "err", err)
		}
		return r.WinRate()
	}
	h, hci := vs("heuristic")
	rnd, _ := vs("random")

	meta := nn.Meta{
		EncodingVersion: encode.Version,
		Games:           games,
		Steps:           steps,
		Saved:           time.Now().UTC().Format(time.RFC3339),
		Metrics:         map[string]float64{"vs_heuristic": h, "vs_heuristic_ci": hci, "vs_random": rnd, "loss": t.loss()},
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	save := func(name string) {
		if err := nn.Save(filepath.Join(cfg.Out, name), net, meta); err != nil {
			t.log.Error("saving model", "file", name, "err", err)
		}
	}
	save(fmt.Sprintf("ckpt-%010d.bin", games))
	save("latest.bin")
	improved := h > e.best
	if improved {
		e.best = h
		save("best.bin")
	}
	t.log.Info("evaluation", "games", games, "vs_heuristic", fmt.Sprintf("%.3f±%.3f", h, hci),
		"vs_random", fmt.Sprintf("%.3f", rnd), "best", improved)
	e.appendCSV(games, steps, h, hci, rnd)
}

func (e *evaluator) appendCSV(games, steps int64, h, hci, rnd float64) {
	_, err := os.Stat(e.csvPath)
	header := os.IsNotExist(err)
	f, err := os.OpenFile(e.csvPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.log.Error("train.csv", "err", err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if header {
		w.Write([]string{"time", "games", "samples", "steps", "loss", "epsilon", "vs_heuristic", "vs_heuristic_ci", "vs_random"})
	}
	ff := func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
	w.Write([]string{
		time.Now().UTC().Format(time.RFC3339), strconv.FormatInt(games, 10), strconv.FormatInt(e.t.samples.Load(), 10),
		strconv.FormatInt(steps, 10), ff(e.t.loss()), ff(e.t.epsilon(games)), ff(h), ff(hci), ff(rnd),
	})
	w.Flush()
}

func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }
