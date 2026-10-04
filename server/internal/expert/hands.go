// Package expert is the search-and-tables cribbage player: learned tables
// stored in a bundle file, and (from M11) a bot that searches with them.
package expert

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// Scores is the number of scores a hand can start from, 0 to 120.
const Scores = engine.WinScore

// Hands holds hands from self-play. Each is the score it started from and
// its ordered scoring steps relative to the dealer: a positive step is
// points for the dealer, a negative one points for the pone. A hand cut
// short by the end of the game stops at the winning step.
type Hands struct {
	steps []int8
	start []int32   // hand i is steps[start[i]:start[i+1]]
	from  [][2]int8 // dealer's and pone's score when hand i started
}

func (h *Hands) Len() int { return len(h.from) }

func (h *Hands) Hand(i int) []int8 { return h.steps[h.start[i]:h.start[i+1]] }

// From returns the dealer's and the pone's scores when hand i started.
func (h *Hands) From(i int) (dealer, pone int) { return int(h.from[i][0]), int(h.from[i][1]) }

func (h *Hands) add(dealer, pone int, steps []int8) {
	if len(h.start) == 0 {
		h.start = append(h.start, 0)
	}
	h.steps = append(h.steps, steps...)
	h.start = append(h.start, int32(len(h.steps)))
	h.from = append(h.from, [2]int8{int8(dealer), int8(pone)})
}

func (h *Hands) addAll(o *Hands) {
	for i := 0; i < o.Len(); i++ {
		x, y := o.From(i)
		h.add(x, y, o.Hand(i))
	}
}

// Replay plays a hand's steps from the dealer on x and the pone on y. It
// returns +1 if the dealer reaches 121 first, -1 if the pone does, and
// otherwise 0 with the scores at the end of the hand. A hand that started
// at or below (x, y) in both scores always has a result: if it was cut
// short, its winner gets there at least as soon from here.
func Replay(steps []int8, x, y int) (result, x2, y2 int) {
	for _, s := range steps {
		if s > 0 {
			if x += int(s); x >= Scores {
				return 1, x, y
			}
		} else if y -= int(s); y >= Scores {
			return -1, x, y
		}
	}
	return 0, x, y
}

// Outcomes counts, for every hand start, how often the dealer went on to
// win the game. Index [dealer score][pone score].
type Outcomes struct {
	N, DealerWins [Scores][Scores]int64
}

func (o *Outcomes) add(p *Outcomes) {
	for x := range o.N {
		for y := range o.N[x] {
			o.N[x][y] += p.N[x][y]
			o.DealerWins[x][y] += p.DealerWins[x][y]
		}
	}
}

// SelfPlay is the data from a run of self-play games.
type SelfPlay struct {
	Games    int
	Hands    Hands      // every hand played
	Outcomes Outcomes   // every hand start, with the game's result
	Details  []HandStat // complete hands that started with both scores below DetailMaxStart
}

// DetailMaxStart bounds the scores a hand may start from to be kept in
// SelfPlay.Details. No hand gains a player 52 points, so these hands all run
// to completion and the details are not biased by games ending part way.
const DetailMaxStart = 70

// HandStat is one complete hand in detail. Index 0 is the dealer, 1 the
// pone.
type HandStat struct {
	Starter  engine.Card
	Kept     [2][4]engine.Card
	Discards [2][2]engine.Card
	Show     [2]int8
	Peg      [2]int8
	Crib     int8
}

// Collect plays games between two bots made by newBot (one per seat, per
// worker) and records every hand. Game i deals from ai.DealSeed(seed, i), so
// the result does not depend on the number of workers.
func Collect(ctx context.Context, newBot func(worker int) bots.Bot, games, workers int, seed uint64) (*SelfPlay, error) {
	workers = max(workers, 1)
	var next atomic.Int64
	var mu sync.Mutex
	var firstErr error
	// Hands are gathered per game index so their order is reproducible.
	type gameData struct {
		hands   Hands
		details []HandStat
	}
	perGame := make([]gameData, games)
	total := &SelfPlay{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			players := [2]bots.Bot{newBot(w), newBot(w)}
			local := new(Outcomes)
			n := 0
			for ctx.Err() == nil {
				i := next.Add(1) - 1
				if i >= int64(games) {
					break
				}
				d := &perGame[i]
				if err := playRecorded(ai.DealSeed(seed, i), players, &d.hands, &d.details, local); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					break
				}
				n++
			}
			mu.Lock()
			total.Games += n
			total.Outcomes.add(local)
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	for i := range perGame {
		total.Hands.addAll(&perGame[i].hands)
		total.Details = append(total.Details, perGame[i].details...)
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return total, firstErr
}

// playRecorded plays one game, appending its hands to hands, its complete
// early hands to details and its hand starts to out.
func playRecorded(seed uint64, players [2]bots.Bot, hands *Hands, details *[]HandStat, out *Outcomes) error {
	g := engine.NewGame(seed)
	var starts [][3]int // dealer, dealer's score, pone's score at each hand start
	var steps []int8
	prev := [2]int{}
	dealer := -1
	var hs HandStat
	side := func(p int) int { // 0 for the dealer, 1 for the pone
		if p == dealer {
			return 0
		}
		return 1
	}
	finish := func() {
		if dealer >= 0 {
			s := starts[len(starts)-1]
			hands.add(s[1], s[2], steps)
		}
		steps = steps[:0]
	}
	handle := func(evs []engine.Event) {
		for _, e := range evs {
			switch e.Kind {
			case engine.EvStarter:
				hs.Starter = e.Card
			case engine.EvPlay, engine.EvGoPoint, engine.EvLastCard:
				hs.Peg[side(e.Player)] += int8(e.Points)
			case engine.EvShowHand:
				hs.Show[side(e.Player)] = int8(e.Points)
				hs.Kept[side(e.Player)] = e.Cards
			case engine.EvShowCrib:
				hs.Crib = int8(e.Points)
				if s := starts[len(starts)-1]; s[1] < DetailMaxStart && s[2] < DetailMaxStart {
					*details = append(*details, hs)
				}
			}
			if e.Kind == engine.EvDeal {
				finish()
				dealer = e.Player
				hs = HandStat{}
				starts = append(starts, [3]int{dealer, e.Scores[dealer], e.Scores[1-dealer]})
				prev = e.Scores
				continue
			}
			for p := 0; p < 2; p++ {
				if d := e.Scores[p] - prev[p]; d > 0 {
					if p == dealer {
						steps = append(steps, int8(d))
					} else {
						steps = append(steps, int8(-d))
					}
				}
			}
			prev = e.Scores
			if e.Kind == engine.EvGameOver {
				finish()
				dealer = -1
			}
		}
	}
	handle(g.Events())
	buf := make([]engine.Action, 0, 16)
	o := new(engine.Observation)
	for !g.Over() {
		for p := 0; p < 2 && !g.Over(); p++ {
			if !g.ToAct(p) {
				continue
			}
			g.ObserveInto(p, o)
			buf = g.AppendLegal(p, buf[:0])
			a := players[p].Choose(o, buf)
			if err := g.Apply(p, a); err != nil {
				return fmt.Errorf("seed %d: %s (P%d) chose %v: %w", seed, players[p].Name(), p, a, err)
			}
			if a.Kind == engine.ActDiscard {
				hs.Discards[side(p)] = a.Cards
			}
			handle(g.Events())
		}
	}
	for _, s := range starts {
		out.N[s[1]][s[2]]++
		if s[0] == g.Winner() {
			out.DealerWins[s[1]][s[2]]++
		}
	}
	return nil
}
