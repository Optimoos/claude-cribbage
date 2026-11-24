package ai

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// ArenaResult summarizes a duplicate-deal match between two players, from
// A's side. Every deal is played twice with the seats swapped, so a pair
// of games sees the same cards with the roles reversed and most of the
// deal luck cancels.
type ArenaResult struct {
	Pairs, Games, WinsA int
	PointsA, PointsB    int // sums of final scores
	PegA, PegB          int // sums of pegging points
	Hands               int // hands dealt, including each game's last

	// Per-pair sums for the paired intervals: w is A's wins in the pair
	// (0, 1 or 2) and d is A's total margin over the pair's two games.
	sumW2, sumD, sumD2 float64
	// Per-pair pegging margin and its square, for the paired interval.
	sumP, sumP2 float64
	// Per-game sums for the unpaired interval of the margin.
	sumG2 float64
}

func (r *ArenaResult) add(o ArenaResult) {
	r.Pairs += o.Pairs
	r.Games += o.Games
	r.WinsA += o.WinsA
	r.PointsA += o.PointsA
	r.PointsB += o.PointsB
	r.PegA += o.PegA
	r.PegB += o.PegB
	r.Hands += o.Hands
	r.sumP += o.sumP
	r.sumP2 += o.sumP2
	r.sumW2 += o.sumW2
	r.sumD += o.sumD
	r.sumD2 += o.sumD2
	r.sumG2 += o.sumG2
}

// WinRate returns A's win fraction and the half-width of its 95%
// confidence interval, computed from the variance between pairs.
func (r ArenaResult) WinRate() (rate, ci float64) {
	if r.Pairs == 0 {
		return 0, 0
	}
	n := float64(r.Pairs)
	// Per-pair win fraction x = w/2.
	mean := float64(r.WinsA) / 2 / n
	return mean, ci95(r.sumW2/4, mean, n)
}

// UnpairedCI is the half-width the win rate's interval would have if the
// same number of games had been played on independent deals.
func (r ArenaResult) UnpairedCI() float64 {
	if r.Games == 0 {
		return 0
	}
	p := float64(r.WinsA) / float64(r.Games)
	return 1.96 * math.Sqrt(p*(1-p)/float64(r.Games))
}

// Margin returns A's average final-score margin per game and the
// half-width of its paired 95% confidence interval.
func (r ArenaResult) Margin() (mean, ci float64) {
	if r.Pairs == 0 {
		return 0, 0
	}
	n := float64(r.Pairs)
	// Per-pair margin per game x = d/2.
	mean = r.sumD / 2 / n
	return mean, ci95(r.sumD2/4, mean, n)
}

// PegMargin returns A's pegging margin per hand and the half-width of its
// paired 95% confidence interval (treating hands per pair as fixed).
func (r ArenaResult) PegMargin() (mean, ci float64) {
	if r.Pairs == 0 || r.Hands == 0 {
		return 0, 0
	}
	n := float64(r.Pairs)
	perPair := r.sumP / n
	hpp := float64(r.Hands) / n // hands per pair
	return perPair / hpp, ci95(r.sumP2, perPair, n) / hpp
}

// UnpairedMarginCI is the margin's interval half-width as if the games had
// been independent deals.
func (r ArenaResult) UnpairedMarginCI() float64 {
	if r.Games == 0 {
		return 0
	}
	n := float64(r.Games)
	return ci95(r.sumG2, r.sumD/n, n)
}

// ci95 returns the 95% half-width for the mean of n samples given the sum
// of their squares and their mean.
func ci95(sumSq, mean, n float64) float64 {
	if n < 2 {
		return 0
	}
	v := (sumSq - n*mean*mean) / (n - 1)
	return 1.96 * math.Sqrt(max(v, 0)/n)
}

// DealSeed returns the engine seed for deal i of a match with the given base
// seed. Different base seeds give unrelated deals.
func DealSeed(seed uint64, i int64) uint64 {
	z := seed + uint64(i+1)*0x9e3779b97f4a7c15
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

// Pair is the result of one deal played twice, from A's side.
type Pair struct {
	Deal             int64 // index of the deal in the match
	WinsA            int   // games A won: 0, 1 or 2
	Margin           int   // A's final score less B's, summed over both games
	Peg              int   // A's pegging points less B's, summed over both games
	Hands            int   // hands dealt in both games
	PointsA, PointsB int   // final scores summed over both games
	PegA, PegB       int   // pegging points summed over both games
	// Trace hashes every action in both games. Every bot here is
	// deterministic and the deal fixes the cards, so two matches on the
	// same deal played the same way exactly when their traces agree.
	Trace uint64
}

// result is the pair as an ArenaResult of one pair.
func (p Pair) result() ArenaResult {
	return ArenaResult{
		Pairs: 1, Games: 2, WinsA: p.WinsA,
		PointsA: p.PointsA, PointsB: p.PointsB, PegA: p.PegA, PegB: p.PegB, Hands: p.Hands,
		sumW2: float64(p.WinsA * p.WinsA),
		sumD:  float64(p.Margin), sumD2: float64(p.Margin * p.Margin),
		sumP: float64(p.Peg), sumP2: float64(p.Peg * p.Peg),
	}
}

// reseed restarts a randomized bot's choices from a stream fixed by the
// deal and the bot, so every deal replays exactly whichever worker plays
// it, and changing the other bot leaves this one's stream as it was.
func reseed(b bots.Bot, deal uint64, which uint64) {
	for {
		switch x := b.(type) {
		case bots.Reseeder:
			x.Reseed(DealSeed(deal, int64(which)))
			return
		case interface{ Unwrap() bots.Bot }:
			b = x.Unwrap()
		default:
			return
		}
	}
}

// tracer passes a bot's choices through, mixing each into a shared hash.
type tracer struct {
	bots.Bot
	h *uint64
}

func (t tracer) Unwrap() bots.Bot { return t.Bot }

func (t tracer) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	a := t.Bot.Choose(o, legal)
	x := uint64(a.Kind)<<16 | uint64(a.Cards[0])<<8 | uint64(a.Cards[1])
	*t.h = (*t.h ^ x) * 0x100000001b3
	return a
}

// Arena plays games between bots made by newA and newB: games/2 deals
// (rounded up), each played once with A in seat 0 and once with A in seat
// 1. Each worker calls the constructors once, so bots need not be safe for
// concurrent use.
func Arena(ctx context.Context, newA, newB func(worker int) bots.Bot, games, workers int, seed uint64) (ArenaResult, error) {
	return ArenaPairs(ctx, newA, newB, games, workers, seed, nil)
}

// ArenaPairs is Arena, also passing each pair to onPair (when not nil) as
// it finishes. Pairs finish out of order; onPair is never called
// concurrently. Cancelling ctx stops the match after the pairs in play.
func ArenaPairs(ctx context.Context, newA, newB func(worker int) bots.Bot, games, workers int, seed uint64, onPair func(Pair)) (ArenaResult, error) {
	if workers < 1 {
		workers = 1
	}
	pairs := int64((games + 1) / 2)
	var next atomic.Int64
	var mu sync.Mutex
	var total ArenaResult
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var h uint64
			a, b := tracer{newA(w), &h}, tracer{newB(w), &h}
			for ctx.Err() == nil {
				i := next.Add(1) - 1
				if i >= pairs {
					break
				}
				s := DealSeed(seed, i)
				pr := Pair{Deal: i}
				var g2 float64
				h = 0xcbf29ce484222325
				for aSeat := 0; aSeat < 2; aSeat++ {
					reseed(a, s, 0)
					reseed(b, s, 1)
					seats := [2]bots.Bot{a, b}
					if aSeat == 1 {
						seats = [2]bots.Bot{b, a}
					}
					r, err := bots.PlayGame(s, seats, nil)
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					if r.Winner == aSeat {
						pr.WinsA++
					}
					pa, pb := r.Scores[aSeat], r.Scores[1-aSeat]
					pr.PointsA += pa
					pr.PointsB += pb
					pr.Margin += pa - pb
					pr.PegA += r.Pegged[aSeat]
					pr.PegB += r.Pegged[1-aSeat]
					pr.Peg += r.Pegged[aSeat] - r.Pegged[1-aSeat]
					pr.Hands += r.Hands
					g2 += float64((pa - pb) * (pa - pb)) // for the unpaired margin interval
				}
				pr.Trace = h
				mu.Lock()
				res := pr.result()
				res.sumG2 = g2
				total.add(res)
				if onPair != nil {
					onPair(pr)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return total, firstErr
}
