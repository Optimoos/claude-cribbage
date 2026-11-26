package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
	"cribbage/internal/expert"
	"cribbage/internal/players"
)

// rollout is the E2 discard oracle: it values every discard option of
// stored decisions by playing out the hand over shared samples of the
// opponent's cards and the starter, and reports how much each discard
// policy gives up against the best option (its regret).
func rollout(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollout", flag.ExitOnError)
	bundle := fs.String("bundle", "models-expert/expert.bin", "bundle whose games supply the decisions, whose discarder the opponent uses and whose win table values the hand's end")
	decisions := fs.Int("decisions", 2000, "discard decisions to value")
	samples := fs.Int("samples", 1000, "opponent hands and starters per decision, shared by its options")
	pegEffort := fs.Int("peg-effort", 25, "search effort of both players' pegging in the playouts")
	policies := fs.String("policies", "expert,heuristic,heuristic2", "discard policies to score: expert (the bundle's discarder with default options: not cond or islambda, as the very hard level uses) or registered bot names")
	seed := fs.Uint64("seed", 950001, "base seed for the games the decisions come from and for the samples")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel decisions")
	out := fs.String("out", "", "write each option's values here as CSV (for testing other discarders later)")
	fs.Parse(args)

	b, err := expert.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	start := time.Now()
	decs, err := collectDiscards(ctx, b, *decisions, *workers, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("collected %d discard decisions from %s self-play in %v\n", len(decs), *bundle, time.Since(start).Round(time.Second))

	pols := strings.Split(*policies, ",")
	newPeg, err := players.Resolve(fmt.Sprintf("%s@%d", *bundle, *pegEffort))
	if err != nil {
		return err
	}
	results := make([]decisionValues, len(decs))
	var next, done atomic.Int64
	var wg sync.WaitGroup
	start = time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := roller{
				win:     b.Win,
				oppDisc: expert.NewBot(b, expert.DefaultEffort),
				peg:     [2]bots.Bot{newPeg(w), newPeg(w)},
			}
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(decs) {
					return
				}
				results[i] = r.values(decs[i], *samples, ai.DealSeed(*seed^0x5a5a, int64(i)))
				if n := done.Add(1); n%100 == 0 {
					el := time.Since(start)
					fmt.Fprintf(os.Stderr, "  %d/%d decisions, %v, about %v left\n", n, len(decs), el.Round(time.Second),
						(time.Duration(float64(el) / float64(n) * float64(int64(len(decs))-n))).Round(time.Minute))
				}
			}
		}(w)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Printf("valued %d decisions x %d samples in %v\n", len(decs), *samples, time.Since(start).Round(time.Second))

	if *out != "" {
		if err := writeRollouts(*out, decs, results); err != nil {
			return err
		}
	}

	fmt.Println("\nRegret per decision against the best option (95% intervals). 'naive' picks the best")
	fmt.Println("option on all samples (biased high); 'split' picks on one half and scores on the other")
	fmt.Println("(biased low). Points are the hand's points less the opponent's.")
	fmt.Printf("%-12s %8s  %-22s %-22s  %-22s %-22s\n", "policy", "agrees", "win % naive", "win % split", "points naive", "points split")
	for _, name := range pols {
		choose, err := discardPolicy(b, name)
		if err != nil {
			return err
		}
		var agree int
		var st [4]meanVar
		for i, d := range decs {
			c := choose(d)
			k := results[i].option(c)
			if k < 0 {
				return fmt.Errorf("policy %s chose %v, not an option", name, c)
			}
			if k == results[i].bestOf(results[i].winAll) {
				agree++
			}
			st[0].add(results[i].regret(results[i].winAll, results[i].winAll, k))
			st[1].add(results[i].splitRegret(results[i].win, k))
			st[2].add(results[i].regret(results[i].ptsAll, results[i].ptsAll, k))
			st[3].add(results[i].splitRegret(results[i].pts, k))
		}
		fmt.Printf("%-12s %7.1f%%  %-22s %-22s  %-22s %-22s\n", name, 100*float64(agree)/float64(len(decs)),
			st[0].fmt(100, 3), st[1].fmt(100, 3), st[2].fmt(1, 4), st[3].fmt(1, 4))
	}
	return nil
}

// discardDecision is one discard to value: my six cards, the scores and
// who deals. I am seat 0.
type discardDecision struct {
	six    [6]engine.Card
	scores [2]int
	dealer int
}

// collectDiscards records the first n discard decisions of seat 0 in
// self-play of the bundle at full effort, from games seeded
// away from those tables are built from.
func collectDiscards(ctx context.Context, b *expert.Bundle, n, workers int, seed uint64) ([]discardDecision, error) {
	var mu sync.Mutex
	var decs []discardDecision
	newBot := func(int) bots.Bot { return expert.NewBot(b, expert.DefaultEffort) }
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := ai.Arena(cctx, func(w int) bots.Bot {
		return &recorder{Bot: newBot(w), record: func(d discardDecision) {
			mu.Lock()
			defer mu.Unlock()
			if len(decs) < n {
				decs = append(decs, d)
			}
			if len(decs) >= n {
				cancel()
			}
		}}
	}, newBot, 1<<30, workers, seed)
	if err != nil && ctx.Err() == nil && cctx.Err() == nil {
		return nil, err
	}
	return decs, ctx.Err()
}

// recorder passes a bot's decisions through, recording its discards.
type recorder struct {
	bots.Bot
	record func(discardDecision)
}

func (r *recorder) Unwrap() bots.Bot { return r.Bot }

func (r *recorder) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if o.Phase == engine.PhaseDiscard && o.HandN == 6 {
		d := discardDecision{dealer: 1}
		copy(d.six[:], o.HandCards())
		d.scores = [2]int{o.Scores[o.Player], o.Scores[1-o.Player]}
		if o.Dealer == o.Player {
			d.dealer = 0
		}
		r.record(d)
	}
	return r.Bot.Choose(o, legal)
}

// decisionValues holds each option's playout values: win probability and
// points margin, over all samples and over each half.
type decisionValues struct {
	options        [][2]engine.Card
	winAll, ptsAll []float64
	win, pts       [2][]float64 // by half: even and odd samples
}

func (v *decisionValues) option(disc engine.Action) int {
	for k, o := range v.options {
		if (o[0] == disc.Cards[0] && o[1] == disc.Cards[1]) || (o[0] == disc.Cards[1] && o[1] == disc.Cards[0]) {
			return k
		}
	}
	return -1
}

func (v *decisionValues) bestOf(x []float64) int {
	best := 0
	for k := range x {
		if x[k] > x[best] {
			best = k
		}
	}
	return best
}

// regret scores option k against the best option by pick, on score.
func (v *decisionValues) regret(pick, score []float64, k int) float64 {
	return score[v.bestOf(pick)] - score[k]
}

// splitRegret picks the best on each half and scores it on the other.
func (v *decisionValues) splitRegret(x [2][]float64, k int) float64 {
	return (v.regret(x[0], x[1], k) + v.regret(x[1], x[0], k)) / 2
}

// roller plays out hands; it is not safe for concurrent use.
type roller struct {
	win     *expert.WinTable
	oppDisc bots.Bot
	peg     [2]bots.Bot
}

// values plays every option of d over the same samples of the opponent's
// six cards and the starter.
func (r *roller) values(d discardDecision, samples int, seed uint64) decisionValues {
	var opts [][2]engine.Card
	for i := 0; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			opts = append(opts, [2]engine.Card{d.six[i], d.six[j]})
		}
	}
	return r.valuesOf(d, opts, samples, seed)
}

// valuesOf is values for the given options only.
func (r *roller) valuesOf(d discardDecision, opts [][2]engine.Card, samples int, seed uint64) decisionValues {
	v := decisionValues{options: opts}
	nopt := len(v.options)
	v.winAll, v.ptsAll = make([]float64, nopt), make([]float64, nopt)
	for h := 0; h < 2; h++ {
		v.win[h], v.pts[h] = make([]float64, nopt), make([]float64, nopt)
	}
	var unseen []engine.Card
	var mine [52]bool
	for _, c := range d.six {
		mine[c] = true
	}
	for c := engine.Card(0); c < 52; c++ {
		if !mine[c] {
			unseen = append(unseen, c)
		}
	}
	rng := rand.New(rand.NewPCG(seed, 0x7011))
	var o engine.Observation
	buf := make([]engine.Action, 0, 16)
	for s := 0; s < samples; s++ {
		// Their six cards and the starter: a partial shuffle of the unseen.
		for i := 0; i < 7; i++ {
			j := i + rng.IntN(len(unseen)-i)
			unseen[i], unseen[j] = unseen[j], unseen[i]
		}
		var hands [2][6]engine.Card
		hands[0] = d.six
		copy(hands[1][:], unseen[:6])
		g := engine.NewHand(rng.Uint64(), d.dealer, d.scores, hands, unseen[6])
		// Discards are simultaneous: theirs does not depend on mine.
		g.ObserveInto(1, &o)
		buf = g.AppendLegal(1, buf[:0])
		theirs := r.oppDisc.Choose(&o, buf)
		half := s & 1
		for k, opt := range v.options {
			c := g.Clone()
			c.SetQuiet(true)
			if err := c.Apply(1, theirs); err != nil {
				panic(err)
			}
			if err := c.Apply(0, engine.Discard(opt[0], opt[1])); err != nil {
				panic(err)
			}
			w, p := r.playOut(c, d.scores)
			v.winAll[k] += w
			v.ptsAll[k] += p
			v.win[half][k] += w
			v.pts[half][k] += p
		}
	}
	for k := range v.options {
		v.winAll[k] /= float64(samples)
		v.ptsAll[k] /= float64(samples)
		for h := 0; h < 2; h++ {
			n := float64((samples + 1 - h) / 2)
			v.win[h][k] /= n
			v.pts[h][k] /= n
		}
	}
	return v
}

// playOut plays the rest of the hand and returns my win probability at its
// end (through the win table) and my points less theirs.
func (r *roller) playOut(g *engine.Game, before [2]int) (win, pts float64) {
	var o engine.Observation
	buf := make([]engine.Action, 0, 16)
	for !g.Over() && g.HandNumber() == 1 {
		for p := 0; p < 2; p++ {
			if g.Over() || g.HandNumber() != 1 || !g.ToAct(p) {
				continue
			}
			g.ObserveInto(p, &o)
			buf = g.AppendLegal(p, buf[:0])
			a := buf[0]
			if len(buf) > 1 {
				a = r.peg[p].Choose(&o, buf)
			}
			if err := g.Apply(p, a); err != nil {
				panic(err)
			}
		}
	}
	sc := g.Scores()
	pts = float64((sc[0] - before[0]) - (sc[1] - before[1]))
	if g.Over() {
		if g.Winner() == 0 {
			return 1, pts
		}
		return 0, pts
	}
	return r.win.Win(sc[0], sc[1], g.Dealer() == 0), pts
}

// discardPolicy returns a function choosing a discard for a decision.
func discardPolicy(b *expert.Bundle, name string) (func(discardDecision) engine.Action, error) {
	var bot bots.Bot
	if name == "expert" {
		bot = expert.NewBot(b, expert.DefaultEffort)
	} else {
		x, err := bots.New(name, 0)
		if err != nil {
			return nil, err
		}
		bot = x
	}
	return func(d discardDecision) engine.Action {
		return chooseDiscard(bot, d)
	}, nil
}

func writeRollouts(path string, decs []discardDecision, res []decisionValues) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "decision,six,my_score,their_score,i_deal,discard,win,points")
	for i, d := range decs {
		for k, o := range res[i].options {
			fmt.Fprintf(w, "%d,%v,%d,%d,%v,%v %v,%.6f,%.4f\n", i, d.six, d.scores[0], d.scores[1], d.dealer == 0,
				o[0], o[1], res[i].winAll[k], res[i].ptsAll[k])
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// meanVar accumulates a mean and its 95% interval.
type meanVar struct {
	n, sum, sumSq float64
}

func (m *meanVar) add(x float64) { m.n++; m.sum += x; m.sumSq += x * x }

func (m *meanVar) fmt(scale float64, prec int) string {
	if m.n < 2 {
		return "-"
	}
	mean := m.sum / m.n
	v := (m.sumSq - m.n*mean*mean) / (m.n - 1)
	return fmt.Sprintf("%.*f ± %.*f", prec, scale*mean, prec, scale*1.96*math.Sqrt(max(v, 0)/m.n))
}
