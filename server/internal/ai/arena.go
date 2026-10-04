package ai

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"cribbage/internal/bots"
)

// ArenaResult summarizes a duplicate-deal match between two players, from
// A's side. Every deal is played twice with the seats swapped, so a pair
// of games sees the same cards with the roles reversed and most of the
// deal luck cancels.
type ArenaResult struct {
	Pairs, Games, WinsA int
	PointsA, PointsB    int // sums of final scores

	// Per-pair sums for the paired intervals: w is A's wins in the pair
	// (0, 1 or 2) and d is A's total margin over the pair's two games.
	sumW2, sumD, sumD2 float64
	// Per-game sums for the unpaired interval of the margin.
	sumG2 float64
}

func (r *ArenaResult) add(o ArenaResult) {
	r.Pairs += o.Pairs
	r.Games += o.Games
	r.WinsA += o.WinsA
	r.PointsA += o.PointsA
	r.PointsB += o.PointsB
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

// Arena plays games between bots made by newA and newB: games/2 deals
// (rounded up), each played once with A in seat 0 and once with A in seat
// 1. Each worker calls the constructors once, so bots need not be safe for
// concurrent use.
func Arena(ctx context.Context, newA, newB func(worker int) bots.Bot, games, workers int, seed uint64) (ArenaResult, error) {
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
			a, b := newA(w), newB(w)
			var local ArenaResult
			defer func() {
				mu.Lock()
				total.add(local)
				mu.Unlock()
			}()
			for ctx.Err() == nil {
				i := next.Add(1) - 1
				if i >= pairs {
					break
				}
				s := DealSeed(seed, i)
				var pr ArenaResult
				var wins, margin float64
				for aSeat := 0; aSeat < 2; aSeat++ {
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
					pr.Games++
					if r.Winner == aSeat {
						pr.WinsA++
						wins++
					}
					pa, pb := r.Scores[aSeat], r.Scores[1-aSeat]
					pr.PointsA += pa
					pr.PointsB += pb
					g := float64(pa - pb)
					margin += g
					pr.sumG2 += g * g
				}
				pr.Pairs = 1
				pr.sumW2 = wins * wins
				pr.sumD = margin
				pr.sumD2 = margin * margin
				local.add(pr)
			}
		}(w)
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return total, firstErr
}
