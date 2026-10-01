// Command sim plays bots against each other in bulk and reports statistics.
//
//	go run ./cmd/sim -a heuristic -b random -n 100000
//	go run ./cmd/sim -a heuristic -b heuristic -n 1 -trace
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"cribbage/internal/bots"
)

type stats struct {
	games, wins          int
	score                int // sum of final scores
	skunks, doubleSkunks int // inflicted on the opponent
	hands                int
}

func (s *stats) add(o stats) {
	s.games += o.games
	s.wins += o.wins
	s.score += o.score
	s.skunks += o.skunks
	s.doubleSkunks += o.doubleSkunks
	s.hands += o.hands
}

func main() {
	nameA := flag.String("a", "heuristic", "bot A")
	nameB := flag.String("b", "random", "bot B")
	n := flag.Int("n", 10000, "number of games")
	workers := flag.Int("workers", runtime.NumCPU(), "parallel workers")
	seed := flag.Uint64("seed", 1, "base seed; game i uses seed+i")
	trace := flag.Bool("trace", false, "print a full log of the first game")
	flag.Parse()

	for _, name := range []string{*nameA, *nameB} {
		if _, err := bots.New(name, 0); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *trace {
		a, _ := bots.New(*nameA, *seed)
		b, _ := bots.New(*nameB, *seed+1)
		fmt.Printf("P0 = %s (A), P1 = %s (B)\n", *nameA, *nameB)
		if _, err := bots.PlayGame(*seed, [2]bots.Bot{a, b}, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println()
	}

	var next atomic.Int64
	var mu sync.Mutex
	var total [2]stats // index 0 = A, 1 = B
	var firstErr error
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			a, _ := bots.New(*nameA, *seed+uint64(2*w))
			b, _ := bots.New(*nameB, *seed+uint64(2*w+1))
			var local [2]stats
			for {
				i := next.Add(1) - 1
				if i >= int64(*n) {
					break
				}
				// Alternate seats so neither bot always sits as player 0.
				seats := [2]bots.Bot{a, b}
				idx := [2]int{0, 1} // seat -> stats index
				if i%2 == 1 {
					seats = [2]bots.Bot{b, a}
					idx = [2]int{1, 0}
				}
				r, err := bots.PlayGame(*seed+uint64(i), seats, nil)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				for seat := 0; seat < 2; seat++ {
					s := &local[idx[seat]]
					s.games++
					s.score += r.Scores[seat]
					s.hands += r.Hands
					if r.Winner == seat {
						s.wins++
						switch r.Skunk {
						case 1:
							s.skunks++
						case 2:
							s.doubleSkunks++
						}
					}
				}
			}
			mu.Lock()
			total[0].add(local[0])
			total[1].add(local[1])
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if firstErr != nil {
		fmt.Fprintln(os.Stderr, "error:", firstErr)
		os.Exit(1)
	}

	games := total[0].games
	fmt.Printf("%d games in %v (%.0f games/s, %d workers), %.1f hands/game\n",
		games, elapsed.Round(time.Millisecond), float64(games)/elapsed.Seconds(), *workers,
		float64(total[0].hands)/float64(games))
	for i, name := range []string{*nameA, *nameB} {
		s := total[i]
		p := float64(s.wins) / float64(s.games)
		ci := 1.96 * math.Sqrt(p*(1-p)/float64(s.games))
		fmt.Printf("  %-10s wins %6d  %6.2f%% ± %.2f%%  avg score %6.1f  skunks %d  double %d\n",
			name, s.wins, 100*p, 100*ci, float64(s.score)/float64(s.games), s.skunks, s.doubleSkunks)
	}
}
