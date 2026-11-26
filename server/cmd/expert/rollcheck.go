package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
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

// rollcheck scores discard policies against stored rollouts (from expert
// rollout -out): each policy's regret, and each policy's value less the
// first's, decision by decision.
func rollcheck(args []string) error {
	fs := flag.NewFlagSet("rollcheck", flag.ExitOnError)
	in := fs.String("csv", "runs/e2-rollout.csv", "rollouts written by expert rollout -out")
	pols := fs.String("policies", "models-expert/expert.bin@400", "player specs, separated by ';'; the first is the reference")
	minScore := fs.Int("min-score", 0, "only decisions where either score is at least this")
	fs.Parse(args)
	decs, vals, err := readRollouts(*in)
	if err != nil {
		return err
	}
	if *minScore > 0 {
		var d2 []discardDecision
		var v2 []decisionValues
		for i, d := range decs {
			if max(d.scores[0], d.scores[1]) >= *minScore {
				d2, v2 = append(d2, d), append(v2, vals[i])
			}
		}
		decs, vals = d2, v2
	}
	specs := strings.Split(*pols, ";")
	picks := make([][]int, len(specs))
	for p, spec := range specs {
		newBot, err := players.Resolve(spec)
		if err != nil {
			return err
		}
		bot := newBot(0)
		for i, d := range decs {
			a := chooseDiscard(bot, d)
			k := slices.IndexFunc(vals[i].options, func(o [2]engine.Card) bool {
				return (o[0] == a.Cards[0] && o[1] == a.Cards[1]) || (o[0] == a.Cards[1] && o[1] == a.Cards[0])
			})
			if k < 0 {
				return fmt.Errorf("%s chose %v, not an option", spec, a)
			}
			picks[p] = append(picks[p], k)
		}
	}
	fmt.Printf("%d decisions from %s; win %% from the rollouts (95%% intervals)\n", len(decs), *in)
	fmt.Println("regret is biased high: the best option is chosen and scored on the same samples; change from first is not")
	fmt.Printf("%-50s %8s  %-20s %8s  %-20s\n", "policy", "best", "regret", "differs", "change from first")
	for p, spec := range specs {
		var best int
		var reg, diff meanVar
		differs := 0
		for i := range decs {
			v := vals[i].winAll
			b := vals[i].bestOf(v)
			if picks[p][i] == b {
				best++
			}
			reg.add(v[b] - v[picks[p][i]])
			diff.add(v[picks[p][i]] - v[picks[0][i]])
			if picks[p][i] != picks[0][i] {
				differs++
			}
		}
		fmt.Printf("%-50s %7.1f%%  %-20s %8d  %-20s\n", spec, 100*float64(best)/float64(len(decs)), reg.fmt(100, 3), differs, diff.fmt(100, 3))
	}
	return nil
}

// chooseDiscard asks bot for its discard in decision d (the opponent's
// cards and the starter are never seen by a discard; any will do).
func chooseDiscard(bot bots.Bot, d discardDecision) engine.Action {
	var hands [2][6]engine.Card
	hands[0] = d.six
	var others []engine.Card
	for c := engine.Card(0); c < 52; c++ {
		if !slices.Contains(d.six[:], c) {
			others = append(others, c)
		}
	}
	copy(hands[1][:], others[:6])
	g := engine.NewHand(1, d.dealer, d.scores, hands, others[6])
	var o engine.Observation
	g.ObserveInto(0, &o)
	return bot.Choose(&o, g.Legal(0))
}

// readRollouts reads a file written by writeRollouts.
func readRollouts(path string) ([]discardDecision, []decisionValues, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	var decs []discardDecision
	var vals []decisionValues
	last := -1
	for sc.Scan() {
		// decision,[six],my,their,i_deal,discard,win,points
		line := sc.Text()
		lb, rb := strings.IndexByte(line, '['), strings.IndexByte(line, ']')
		if lb < 0 || rb < lb {
			return nil, nil, fmt.Errorf("%s: bad line %q", path, line)
		}
		id, err := strconv.Atoi(strings.TrimSuffix(line[:lb], ","))
		if err != nil {
			return nil, nil, err
		}
		rest := strings.Split(line[rb+2:], ",")
		if len(rest) != 6 {
			return nil, nil, fmt.Errorf("%s: bad line %q", path, line)
		}
		if id != last {
			var d discardDecision
			six := strings.Fields(line[lb+1 : rb])
			if len(six) != 6 {
				return nil, nil, fmt.Errorf("%s: bad line %q", path, line)
			}
			for i, s := range six {
				if d.six[i], err = engine.ParseCard(s); err != nil {
					return nil, nil, err
				}
			}
			d.scores[0], _ = strconv.Atoi(rest[0])
			d.scores[1], _ = strconv.Atoi(rest[1])
			if rest[2] != "true" {
				d.dealer = 1
			}
			decs = append(decs, d)
			vals = append(vals, decisionValues{})
			last = id
		}
		v := &vals[len(vals)-1]
		cs := strings.Fields(rest[3])
		if len(cs) != 2 {
			return nil, nil, fmt.Errorf("%s: bad line %q", path, line)
		}
		var o [2]engine.Card
		for i := range o {
			if o[i], err = engine.ParseCard(cs[i]); err != nil {
				return nil, nil, err
			}
		}
		w, err := strconv.ParseFloat(rest[4], 64)
		if err != nil {
			return nil, nil, err
		}
		p, _ := strconv.ParseFloat(rest[5], 64)
		v.options = append(v.options, o)
		v.winAll = append(v.winAll, w)
		v.ptsAll = append(v.ptsAll, p)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(decs) == 0 {
		return nil, nil, fmt.Errorf("%s: no decisions", path)
	}
	return decs, vals, nil
}

// rollpair compares two discard policies where they differ: it collects
// self-play discard decisions, keeps those where the policies disagree,
// and plays out just their two choices over shared samples.
func rollpair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollpair", flag.ExitOnError)
	bundle := fs.String("bundle", "models-expert/expert.bin", "bundle whose self-play supplies the decisions, whose discarder the opponent uses and whose win table values the hand's end")
	pols := fs.String("policies", "", "two player specs separated by ';' (required)")
	decisions := fs.Int("decisions", 40000, "self-play decisions to search for disagreements")
	maxDiff := fs.Int("max", 2000, "disagreements to play out at most")
	samples := fs.Int("samples", 1000, "opponent hands and starters per decision")
	pegEffort := fs.Int("peg-effort", 25, "search effort of both players' pegging in the playouts")
	seed := fs.Uint64("seed", 960001, "base seed for the games and the samples")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel workers")
	fs.Parse(args)
	specs := strings.Split(*pols, ";")
	if len(specs) != 2 {
		return fmt.Errorf("rollpair: -policies wants two specs")
	}
	b, err := expert.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	t0 := time.Now()
	decs, err := collectDiscards(ctx, b, *decisions, *workers, *seed)
	if err != nil {
		return err
	}
	var newBots [2]func(int) bots.Bot
	for i, s := range specs {
		if newBots[i], err = players.Resolve(s); err != nil {
			return err
		}
	}
	// Each decision's two choices, in parallel.
	picks := make([][2]engine.Action, len(decs))
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			bs := [2]bots.Bot{newBots[0](w), newBots[1](w)}
			for {
				i := int(next.Add(1) - 1)
				if i >= len(decs) || ctx.Err() != nil {
					return
				}
				for k := range bs {
					picks[i][k] = chooseDiscard(bs[k], decs[i])
				}
			}
		}(w)
	}
	wg.Wait()
	same := func(a, b engine.Action) bool {
		return (a.Cards[0] == b.Cards[0] && a.Cards[1] == b.Cards[1]) || (a.Cards[0] == b.Cards[1] && a.Cards[1] == b.Cards[0])
	}
	var diff []int
	for i := range decs {
		if !same(picks[i][0], picks[i][1]) {
			diff = append(diff, i)
		}
	}
	fmt.Printf("%d of %d decisions differ (%.2f%%), in %v\n", len(diff), len(decs), 100*float64(len(diff))/float64(len(decs)), time.Since(t0).Round(time.Second))
	rate := float64(len(diff)) / float64(len(decs)) // before -max cuts the list
	if len(diff) > *maxDiff {
		diff = diff[:*maxDiff]
	}
	newPeg, err := players.Resolve(fmt.Sprintf("%s@%d", *bundle, *pegEffort))
	if err != nil {
		return err
	}
	res := make([]decisionValues, len(diff))
	next.Store(0)
	t1 := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := roller{win: b.Win, oppDisc: expert.NewBot(b, expert.DefaultEffort), peg: [2]bots.Bot{newPeg(w), newPeg(w)}}
			for ctx.Err() == nil {
				j := int(next.Add(1) - 1)
				if j >= len(diff) {
					return
				}
				i := diff[j]
				opts := [][2]engine.Card{picks[i][0].Cards, picks[i][1].Cards}
				res[j] = r.valuesOf(decs[i], opts, *samples, ai.DealSeed(*seed^0x5a5a, int64(i)))
			}
		}(w)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var dw, dp meanVar
	var dealer, pone meanVar
	for j, i := range diff {
		x := res[j].winAll[1] - res[j].winAll[0]
		dw.add(x)
		dp.add(res[j].ptsAll[1] - res[j].ptsAll[0])
		if decs[i].dealer == 0 {
			dealer.add(x)
		} else {
			pone.add(x)
		}
	}
	fmt.Printf("played out %d disagreements x %d samples in %v\n", len(diff), *samples, time.Since(t1).Round(time.Second))
	fmt.Printf("%s less %s where they differ (95%% intervals):\n", specs[1], specs[0])
	fmt.Printf("  win %%     %s\n  points    %s\n  as dealer %s (%d)\n  as pone   %s (%d)\n", dw.fmt(100, 3), dp.fmt(1, 4),
		dealer.fmt(100, 3), int(dealer.n), pone.fmt(100, 3), int(pone.n))
	fmt.Printf("per decision overall: %.4f win %% (rate %.4f x mean)\n", rate*100*dw.sum/dw.n, rate)
	return nil
}
