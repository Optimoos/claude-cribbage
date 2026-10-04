package expert

import (
	"sync"

	"cribbage/internal/engine"
)

// Index sizes for the discard tables.
const (
	numPairs  = 91   // unordered pairs of ranks, 13*14/2
	numShapes = 1820 // multisets of four ranks
	pegBins   = 32   // pegging points 0..31; more is counted as 31
)

var (
	pairIdx  [14][14]int16
	shapeIdx = func() []int16 {
		// Index every sorted 4-rank multiset, keyed in base 14.
		idx := make([]int16, 14*14*14*14)
		n := int16(0)
		for a := 1; a <= 13; a++ {
			for b := a; b <= 13; b++ {
				for c := b; c <= 13; c++ {
					for d := c; d <= 13; d++ {
						idx[((a*14+b)*14+c)*14+d] = n
						n++
					}
				}
			}
		}
		return idx
	}()
)

func init() {
	n := int16(0)
	for a := 1; a <= 13; a++ {
		for b := a; b <= 13; b++ {
			pairIdx[a][b], pairIdx[b][a] = n, n
			n++
		}
	}
}

// pairOf indexes two discards by their ranks.
func pairOf(a, b engine.Card) int { return int(pairIdx[a.Rank()][b.Rank()]) }

// shapeOf indexes four kept cards by their ranks.
func shapeOf(kept [4]engine.Card) int {
	r := [4]int{kept[0].Rank(), kept[1].Rank(), kept[2].Rank(), kept[3].Rank()}
	for i := 1; i < 4; i++ { // insertion sort
		for j := i; j > 0 && r[j] < r[j-1]; j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
	return int(shapeIdx[((r[0]*14+r[1])*14+r[2])*14+r[3]])
}

// DiscardTables are the learned distributions the discard engine uses.
// Side 0 is the dealer, 1 the pone. Each distribution sums to 1.
type DiscardTables struct {
	// Crib[side][pair][starter rank-1][points]: the crib's score given the
	// ranks one player discarded, that player's side and the starter.
	Crib []float32
	// Peg[side][shape][who][points]: pegging points for the player keeping
	// a hand of this rank shape (who 0) and for their opponent (who 1).
	Peg []float32
	// Show[side][starter rank-1][points]: the show score of a player's
	// hand on that side.
	Show []float32
}

var (
	cribShape = []int{2, numPairs, 13, maxShow}
	pegShape  = []int{2, numShapes, 2, pegBins}
	showShape = []int{2, 13, maxShow}
)

func (t *DiscardTables) crib(side, pair, starterRank int) []float32 {
	i := ((side*numPairs+pair)*13 + starterRank - 1) * maxShow
	return t.Crib[i : i+maxShow]
}

func (t *DiscardTables) peg(side, shape, who int) []float32 {
	i := ((side*numShapes+shape)*2 + who) * pegBins
	return t.Peg[i : i+pegBins]
}

func (t *DiscardTables) show(side, starterRank int) []float32 {
	i := (side*13 + starterRank - 1) * maxShow
	return t.Show[i : i+maxShow]
}

// smoothing is the weight, in hands, of the coarser distribution each
// table cell is shrunk towards.
const smoothing = 20

// BuildDiscardTables estimates the tables from detailed self-play hands.
// Each cell is shrunk towards a coarser distribution (the crib towards the
// same discards with any starter, and that towards any discards), so rare
// cells borrow strength from common ones.
func BuildDiscardTables(hands []HandStat) *DiscardTables {
	cribN := make([]float64, 2*numPairs*13*maxShow)
	pegN := make([]float64, 2*numShapes*2*pegBins)
	showN := make([]float64, 2*13*maxShow)
	for i := range hands {
		h := &hands[i]
		sr := h.Starter.Rank()
		for side := 0; side < 2; side++ {
			pair := pairOf(h.Discards[side][0], h.Discards[side][1])
			cribN[((side*numPairs+pair)*13+sr-1)*maxShow+int(h.Crib)]++
			shape := shapeOf(h.Kept[side])
			for who := 0; who < 2; who++ {
				pts := min(int(h.Peg[side^who]), pegBins-1)
				pegN[((side*numShapes+shape)*2+who)*pegBins+pts]++
			}
			showN[(side*13+sr-1)*maxShow+int(h.Show[side])]++
		}
	}
	t := &DiscardTables{
		Crib: make([]float32, len(cribN)),
		Peg:  make([]float32, len(pegN)),
		Show: make([]float32, len(showN)),
	}
	// Crib: each cell is shrunk towards the crib with the other two cards
	// drawn at random, which is right on average but not for how players
	// really discard.
	prior := randomCrib()
	for side := 0; side < 2; side++ {
		for pair := 0; pair < numPairs; pair++ {
			for sr := 1; sr <= 13; sr++ {
				i := ((side*numPairs+pair)*13 + sr - 1) * maxShow
				store(t.Crib[i:i+maxShow], shrink(cribN[i:i+maxShow], prior[pair][sr-1][:]))
			}
		}
	}
	// Pegging: cell <- (side, who) over all shapes.
	for side := 0; side < 2; side++ {
		for who := 0; who < 2; who++ {
			all := make([]float64, pegBins)
			for shape := 0; shape < numShapes; shape++ {
				for k, c := range pegN[((side*numShapes+shape)*2+who)*pegBins:][:pegBins] {
					all[k] += c
				}
			}
			allP := shrink(all, nil)
			for shape := 0; shape < numShapes; shape++ {
				i := ((side*numShapes+shape)*2 + who) * pegBins
				store(t.Peg[i:i+pegBins], shrink(pegN[i:i+pegBins], allP))
			}
		}
	}
	// Show: cell <- side over all starters.
	for side := 0; side < 2; side++ {
		all := make([]float64, maxShow)
		for sr := 1; sr <= 13; sr++ {
			for k, c := range showN[(side*13+sr-1)*maxShow:][:maxShow] {
				all[k] += c
			}
		}
		allP := shrink(all, nil)
		for sr := 1; sr <= 13; sr++ {
			i := (side*13 + sr - 1) * maxShow
			store(t.Show[i:i+maxShow], shrink(showN[i:i+maxShow], allP))
		}
	}
	return t
}

// shrink turns counts into a distribution, adding smoothing hands' worth of
// prior (or nothing if prior is nil). An empty cell with no prior is put
// all at zero.
func shrink(counts, prior []float64) []float64 {
	p := make([]float64, len(counts))
	n := 0.0
	for _, c := range counts {
		n += c
	}
	k := 0.0
	if prior != nil {
		k = smoothing
	}
	if n+k == 0 {
		p[0] = 1
		return p
	}
	for i, c := range counts {
		p[i] = c
		if prior != nil {
			p[i] += k * prior[i]
		}
		p[i] /= n + k
	}
	return p
}

func store(dst []float32, src []float64) {
	for i, v := range src {
		dst[i] = float32(v)
	}
}

// CribMean is the average crib score when the player on side discards
// ranks r1 and r2, over starter ranks equally weighted.
func (t *DiscardTables) CribMean(side, r1, r2 int) float64 {
	var m float64
	for sr := 1; sr <= 13; sr++ {
		for k, p := range t.crib(side, int(pairIdx[r1][r2]), sr) {
			m += float64(k) * float64(p)
		}
	}
	return m / 13
}

// PegMean is the average pegging score of the player keeping ranks on
// side (who 0) or of their opponent (who 1).
func (t *DiscardTables) PegMean(side int, ranks [4]int, who int) float64 {
	var kept [4]engine.Card
	for i, r := range ranks {
		kept[i] = engine.NewCard(r, engine.Clubs)
	}
	var m float64
	for k, p := range t.peg(side, shapeOf(kept), who) {
		m += float64(k) * float64(p)
	}
	return m
}

var (
	randomCribOnce sync.Once
	randomCribDist [numPairs][13][maxShow]float64
)

// randomCrib returns, for every discarded rank pair and starter rank, the
// distribution of the crib's score when the other two cards are drawn at
// random from the rest of the deck. Suits are fixed (the discards and the
// starter are of different suits), so crib flushes are not counted.
func randomCrib() *[numPairs][13][maxShow]float64 {
	randomCribOnce.Do(func() {
		var wg sync.WaitGroup
		for r1 := 1; r1 <= 13; r1++ {
			for r2 := r1; r2 <= 13; r2++ {
				wg.Add(1)
				go func(r1, r2 int) {
					defer wg.Done()
					a, b := engine.NewCard(r1, engine.Clubs), engine.NewCard(r2, engine.Diamonds)
					pair := pairIdx[r1][r2]
					for sr := 1; sr <= 13; sr++ {
						s := engine.NewCard(sr, engine.Hearts)
						if s == a || s == b {
							s = engine.NewCard(sr, engine.Spades)
						}
						var rest []engine.Card
						for c := engine.Card(0); c < 52; c++ {
							if c != a && c != b && c != s {
								rest = append(rest, c)
							}
						}
						dist := &randomCribDist[pair][sr-1]
						n := 0.0
						for i := range rest {
							for j := i + 1; j < len(rest); j++ {
								dist[engine.ScoreHand([4]engine.Card{a, b, rest[i], rest[j]}, s, true).Total()]++
								n++
							}
						}
						for k := range dist {
							dist[k] /= n
						}
					}
				}(r1, r2)
			}
		}
		wg.Wait()
	})
	return &randomCribDist
}
