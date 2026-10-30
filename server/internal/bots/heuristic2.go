package bots

import (
	"math"
	"sync"

	"cribbage/internal/engine"
)

//go:generate go run h2stats_gen.go

// DefaultEndgame is the score from which Heuristic2 plays its discards for
// the win rather than for points.
const DefaultEndgame = 90

// Heuristic2 is Heuristic with score-aware discards: once either player
// has Endgame points or more, it keeps the four cards that maximize its
// probability of winning the game rather than its expected points. That
// probability follows the counting order (pegging, the pone's show, the
// dealer's show, the crib) and then a win table for the hands after,
// all built from heuristic self-play statistics (h2stats.go). Pegging
// looks three plays ahead (heuristic2_peg.go).
type Heuristic2 struct {
	Endgame int

	// Scratch space for one discard decision.
	hand  [64]float64     // my show (dealer: plus my crib and heels)
	qmix  [30]float64     // the opponent's show
	cribH [30][32]float64 // pone: P(my show, their crib and heels)
	their [30][64]float64 // pone: P(my show, their show, crib and heels)
	g     [32][64]float64 // dealer: my win probability after my show and crib, by my pegging and their pegging plus show

	// Scratch space for one play.
	replies []reply
	seqBuf  [16]engine.Card
}

// tieBreak weighs Heuristic's points value against the win probability.
const tieBreak = 1e-4

func NewHeuristic2() *Heuristic2 { return &Heuristic2{Endgame: DefaultEndgame} }

func (h *Heuristic2) Name() string { return "heuristic2" }

func (h *Heuristic2) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if len(legal) == 1 {
		return legal[0]
	}
	if o.Phase != engine.PhaseDiscard {
		return h.choosePlay(o, legal)
	}
	if max(o.Scores[0], o.Scores[1]) < h.Endgame {
		return chooseDiscard(o, legal)
	}
	return h.chooseWinDiscard(o, legal)
}

// chooseWinDiscard returns the discard with the highest win probability.
// Options within about a ten-thousandth of each other are told apart by
// Heuristic's points value, so a game that is already won or lost is still
// played for points.
func (h *Heuristic2) chooseWinDiscard(o *engine.Observation, legal []engine.Action) engine.Action {
	h2Once.Do(buildH2)
	hand := o.HandCards()
	var held [52]bool
	for _, c := range hand {
		held[c] = true
	}
	me, opp := o.Scores[o.Player], o.Scores[1-o.Player]
	iDeal := o.Dealer == o.Player
	best, bestV := legal[0], math.Inf(-1)
	for _, a := range legal {
		kept := keptAfter(hand, a)
		v := h.winValue(kept, a.Cards, &held, me, opp, iDeal) + tieBreak*discardEV(kept, a, &held, iDeal)
		if v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// winValue is my probability of winning after keeping kept and discarding
// disc, from scores me and opp at the start of the hand.
func (h *Heuristic2) winValue(kept [4]engine.Card, disc [2]engine.Card, held *[52]bool, me, opp int, iDeal bool) float64 {
	oside := 0
	if iDeal {
		oside = 1
	}
	r1, r2 := disc[0].Rank(), disc[1].Rank()
	clear(h.hand[:])
	clear(h.qmix[:])
	clear(h.cribH[:])
	const w = 1.0 / 46
	for s := engine.Card(0); s < 52; s++ {
		if held[s] {
			continue
		}
		sr := s.Rank()
		show := engine.ScoreHand(kept, s, false).Total()
		heels := 0
		if sr == 11 {
			heels = 2
		}
		for q, p := range h2Show[oside][sr-1] {
			h.qmix[q] += w * float64(p)
		}
		crib := &h2Crib[r1][r2][sr-1]
		for c, p := range crib {
			if p == 0 {
				continue
			}
			if iDeal {
				h.hand[show+heels+c] += w * p
			} else {
				h.cribH[show][c+heels] += w * p
			}
		}
		if !iDeal {
			h.hand[show] += w
		}
	}

	if iDeal {
		// g[i][j]: I have pegged i, they have pegged and shown j.
		for i := range h.g {
			a := me + i
			if a >= engine.WinScore {
				continue
			}
			for j := range h.g[i] {
				b := opp + j
				if b >= engine.WinScore {
					break
				}
				var x float64
				for m, pm := range h.hand {
					if pm == 0 {
						continue
					}
					x += pm * h2Win(a+m, b, false)
				}
				h.g[i][j] = x
			}
		}
	} else {
		for s := range h.their {
			clear(h.their[s][:])
			if h.hand[s] == 0 {
				continue
			}
			for c, pc := range h.cribH[s] {
				if pc == 0 {
					continue
				}
				for q, pq := range h.qmix {
					h.their[s][c+q] += pc * pq
				}
			}
		}
	}

	var v float64
	for i := range h2Peg {
		for j := range h2Peg[i] {
			// h2Peg is indexed dealer, pone.
			pmi, poj := i, j
			if !iDeal {
				pmi, poj = j, i
			}
			p := float64(h2Peg[i][j])
			if p == 0 {
				continue
			}
			a, b := me+pmi, opp+poj
			var x float64
			switch {
			case a >= engine.WinScore && b >= engine.WinScore:
				x = 0.5 // both peg out; which first is not modelled
			case a >= engine.WinScore:
				x = 1
			case b >= engine.WinScore:
				x = 0
			case iDeal:
				// Their show, then my show, crib and heels together.
				for q, pq := range h.qmix {
					if pq != 0 && b+q < engine.WinScore {
						x += pq * h.g[pmi][poj+q]
					}
				}
			default:
				// My show, then their show, crib and heels.
				for s, ps := range h.hand {
					if ps == 0 {
						continue
					}
					if a+s >= engine.WinScore {
						x += ps
						continue
					}
					for t, pt := range h.their[s] {
						if pt != 0 && b+t < engine.WinScore {
							x += pt * h2Win(a+s, b+t, true)
						}
					}
				}
			}
			v += p * x
		}
	}
	return v
}

var (
	h2Once sync.Once
	// h2V[x][y] is the probability that the dealer wins from the start of
	// a hand with the dealer on x and the pone on y.
	h2V [engine.WinScore][engine.WinScore]float64
	// h2Crib[r1][r2][sr-1][c] is the probability that a crib holding
	// discards of ranks r1 and r2 scores c with a starter of rank sr, the
	// other two cards being random. Suits are fixed, so crib flushes are
	// not counted.
	h2Crib [14][14][13][30]float64
)

// h2Win is my probability of winning from the start of a hand; scores of
// 121 or more mean that player has won.
func h2Win(me, them int, iDeal bool) float64 {
	switch {
	case me >= engine.WinScore:
		return 1
	case them >= engine.WinScore:
		return 0
	case iDeal:
		return h2V[me][them]
	}
	return 1 - h2V[them][me]
}

func buildH2() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); buildH2Win() }()
	for r1 := 1; r1 <= 13; r1++ {
		for r2 := r1; r2 <= 13; r2++ {
			wg.Add(1)
			go func(r1, r2 int) {
				defer wg.Done()
				buildH2Crib(r1, r2)
				h2Crib[r2][r1] = h2Crib[r1][r2]
			}(r1, r2)
		}
	}
	wg.Wait()
}

func buildH2Crib(r1, r2 int) {
	a, b := engine.NewCard(r1, engine.Clubs), engine.NewCard(r2, engine.Diamonds)
	for sr := 1; sr <= 13; sr++ {
		s := engine.NewCard(sr, engine.Hearts)
		var rest []engine.Card
		for c := engine.Card(0); c < 52; c++ {
			if c != a && c != b && c != s {
				rest = append(rest, c)
			}
		}
		dist := &h2Crib[r1][r2][sr-1]
		n := 0
		for i := 0; i < len(rest); i++ {
			for j := i + 1; j < len(rest); j++ {
				dist[engine.ScoreHand([4]engine.Card{a, b, rest[i], rest[j]}, s, true).Total()]++
				n++
			}
		}
		for c := range dist {
			dist[c] /= float64(n)
		}
	}
}

// buildH2Win fills h2V by dynamic programming over the scores, from the
// highest total down. A hand is the joint pegging, the pone's show, then
// the dealer's show, crib and heels, each drawn independently from
// heuristic self-play; after it the pone deals.
func buildH2Win() {
	const n = engine.WinScore
	peg := h2Peg
	peg[0][0] = 0 // impossible: someone scores the last card
	var pegSum float64
	for i := range peg {
		for _, p := range peg[i] {
			pegSum += float64(p)
		}
	}
	var pone [30]float64
	for r := range h2Show[1] {
		for q, p := range h2Show[1][r] {
			pone[q] += float64(p) / 13
		}
	}
	// s1[d][p]: dealer's win probability after pegging, before the shows.
	// s2[d][p]: the same after the pone's show.
	var s1, s2 [n][n]float64
	for sum := 2 * (n - 1); sum >= 0; sum-- {
		lo, hi := max(0, sum-(n-1)), min(sum, n-1)
		for x := lo; x <= hi; x++ {
			y := sum - x
			var v float64
			for i := range peg {
				for j, p := range peg[i] {
					if p == 0 {
						continue
					}
					d, q := x+i, y+j
					var r float64
					switch {
					case d >= n && q >= n:
						r = 0.5
					case d >= n:
						r = 1
					case q >= n:
						r = 0
					default:
						r = s1[d][q]
					}
					v += float64(p) * r
				}
			}
			h2V[x][y] = v / pegSum
		}
		for d := lo; d <= hi; d++ {
			p := sum - d
			var v float64
			for k, pk := range h2Dealer {
				if d+k >= n {
					v += float64(pk)
				} else {
					v += float64(pk) * (1 - h2V[p][d+k])
				}
			}
			s2[d][p] = v
		}
		for d := lo; d <= hi; d++ {
			p := sum - d
			var v float64
			for q, pq := range pone {
				if p+q < n {
					v += pq * s2[d][p+q]
				}
			}
			s1[d][p] = v
		}
	}
}
