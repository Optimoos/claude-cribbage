package ai

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"cribbage/internal/bots"
)

// ArenaResult summarizes a match between two players, from A's side.
type ArenaResult struct {
	Games, WinsA int
	PointsA      int // sum of final scores
	PointsB      int
}

// WinRate returns A's win fraction and the half-width of its 95%
// confidence interval.
func (r ArenaResult) WinRate() (rate, ci float64) {
	if r.Games == 0 {
		return 0, 0
	}
	p := float64(r.WinsA) / float64(r.Games)
	return p, 1.96 * math.Sqrt(p*(1-p)/float64(r.Games))
}

// Arena plays games between bots made by newA and newB, alternating seats.
// Each worker calls the constructors once, so bots need not be safe for
// concurrent use.
func Arena(ctx context.Context, newA, newB func(worker int) bots.Bot, games, workers int, seed uint64) (ArenaResult, error) {
	if workers < 1 {
		workers = 1
	}
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
			for ctx.Err() == nil {
				i := next.Add(1) - 1
				if i >= int64(games) {
					break
				}
				seats, aSeat := [2]bots.Bot{a, b}, 0
				if i%2 == 1 {
					seats, aSeat = [2]bots.Bot{b, a}, 1
				}
				r, err := bots.PlayGame(seed+uint64(i), seats, nil)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				local.Games++
				if r.Winner == aSeat {
					local.WinsA++
				}
				local.PointsA += r.Scores[aSeat]
				local.PointsB += r.Scores[1-aSeat]
			}
			mu.Lock()
			total.Games += local.Games
			total.WinsA += local.WinsA
			total.PointsA += local.PointsA
			total.PointsB += local.PointsB
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return total, firstErr
}
