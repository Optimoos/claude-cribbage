package expert

import (
	"sync"

	"cribbage/internal/engine"
)

// Conditional discards (proposal 4, first part). The discard tables give
// the opponent's show and the crib as averages over the hands opponents
// held in self-play. Here they come instead from the opponent's likely
// hands given the six cards I hold: once per decision, every (kept four,
// thrown pair) the opponent may hold is weighted by the ways it can be
// dealt from the cards I cannot see and by the keep model's chance of that
// keep. Their show then follows exactly from their four and the starter,
// and the crib from my pair, their pair and the starter, with the two
// joint as they are for the pone's total. Shows are counted by ranks, with
// nobs from hidden jacks at its chance of a suit match and hidden flushes
// left out; nobs from my own discarded jack is exact. Pegging stays with
// the tables.

// condPost is the opponent's posterior for one decision, by starter rank:
// m[sr-1][q][pair] is the weight on their show scoring q and their
// throwing pair, with the starter of rank sr (its count left after their
// six); z[sr-1] is its total.
type condPost struct {
	m   [13][maxShow][numPairs]float64
	z   [13]float64
	qHi [13]int // q < qHi wherever m is non-zero
	// avail is each rank's count among the cards I cannot see; pairJ is
	// how many jacks each pair holds.
	avail [14]int
}

// cribRank[a][b][sr-1] is the crib's fifteens, pairs and runs when the two
// discards make pairs a and b and the starter has rank sr.
var cribRank = sync.OnceValue(func() *[numPairs][numPairs][13]int8 {
	handEVTable()
	t := new([numPairs][numPairs][13]int8)
	var pr [numPairs][2]int
	for a := 1; a <= 13; a++ {
		for b := a; b <= 13; b++ {
			pr[pairIdx[a][b]] = [2]int{a, b}
		}
	}
	for i := range pr {
		for j := range pr {
			r := [4]int{pr[i][0], pr[i][1], pr[j][0], pr[j][1]}
			var cnt [14]int
			ok := true
			for _, x := range r {
				if cnt[x]++; cnt[x] > 4 {
					ok = false
				}
			}
			if !ok {
				continue
			}
			for x := 1; x < 4; x++ {
				for y := x; y > 0 && r[y] < r[y-1]; y-- {
					r[y], r[y-1] = r[y-1], r[y]
				}
			}
			s := shapeIdx[((r[0]*14+r[1])*14+r[2])*14+r[3]]
			for sr := 0; sr < 13; sr++ {
				t[i][j][sr] = int8(handEVByShape[s][sr])
			}
		}
	}
	return t
})

var pairJacks = func() (t [numPairs]int) {
	for a := 1; a <= 13; a++ {
		for b := a; b <= 13; b++ {
			if a == 11 {
				t[pairIdx[a][b]]++
			}
			if b == 11 {
				t[pairIdx[a][b]]++
			}
		}
	}
	return t
}()

var smallBinom = [5][5]float64{{1}, {1, 1}, {1, 2, 1}, {1, 3, 3, 1}, {1, 4, 6, 4, 1}}

// posterior fills p for an opponent on side oside when I hold six.
func (p *condPost) posterior(keep *keepModel, oside int, six [6]engine.Card) {
	handEVTable()
	for r := 1; r <= 13; r++ {
		p.avail[r] = 4
	}
	for _, c := range six {
		p.avail[c.Rank()]--
	}
	clear(p.m[:])
	clear(p.z[:])
	clear(p.qHi[:])
	byKept := keep.byKept[oside]
	for k := 0; k < numShapes; k++ {
		kr := shapeRanks[k]
		var cnt [14]int
		jk := 0
		for _, r := range kr {
			cnt[r]++
			if r == 11 {
				jk++
			}
		}
		if cnt[kr[0]] > p.avail[kr[0]] || cnt[kr[1]] > p.avail[kr[1]] || cnt[kr[2]] > p.avail[kr[2]] || cnt[kr[3]] > p.avail[kr[3]] {
			continue
		}
		probs := byKept[k*numPairs : (k+1)*numPairs]
		for r1 := 1; r1 <= 13; r1++ {
			for r2 := r1; r2 <= 13; r2++ {
				pi := pairIdx[r1][r2]
				pk := float64(probs[pi])
				if pk == 0 {
					continue
				}
				cnt[r1]++
				cnt[r2]++
				w := pk
				for _, r := range [6]int{int(kr[0]), int(kr[1]), int(kr[2]), int(kr[3]), r1, r2} {
					if cnt[r] > p.avail[r] {
						w = 0
						break
					}
				}
				if w != 0 {
					// The ways to deal these six ranks: a product over the
					// distinct ranks (each counted once).
					var seen uint16
					for _, r := range [6]int{int(kr[0]), int(kr[1]), int(kr[2]), int(kr[3]), r1, r2} {
						if seen&(1<<r) == 0 {
							seen |= 1 << r
							w *= smallBinom[p.avail[r]][cnt[r]]
						}
					}
					for sr := 1; sr <= 13; sr++ {
						left := float64(p.avail[sr] - cnt[sr])
						if left <= 0 {
							continue
						}
						ws := w * left
						q := int(handEVByShape[k][sr-1])
						pn := 0.0
						if sr != 11 {
							pn = float64(jk) / 4 // one of their jacks may share the starter's suit
						}
						row := &p.m[sr-1]
						row[q][pi] += ws * (1 - pn)
						hi := q + 1
						if pn > 0 {
							row[q+1][pi] += ws * pn
							hi = q + 2
						}
						p.qHi[sr-1] = max(p.qHi[sr-1], hi)
						p.z[sr-1] += ws
					}
				}
				cnt[r1]--
				cnt[r2]--
			}
		}
	}
}

// condOption fills the discarder's per-option distributions (d.qmix,
// d.hand and, for the pone, d.theirsByH) from the posterior instead of
// the show and crib tables, for keeping kept and throwing disc, over the
// starters in that I cannot see.
func (d *discarder) condOption(kept [4]engine.Card, disc [2]engine.Card, held *[52]bool, in func(engine.Card) bool, heelsInShow bool) {
	p := d.post
	cr := cribRank()
	myPair := pairOf(disc[0], disc[1])
	clear(d.qmix[:])
	clear(d.hand[:])
	if !d.iDeal {
		for h := range d.theirsByH {
			clear(d.theirsByH[h][:])
		}
	}
	// Starter weights: a card of rank sr is left with chance z[sr]
	// shared among the avail[sr] I cannot see.
	var total float64
	for s := engine.Card(0); s < 52; s++ {
		if !held[s] && in(s) {
			total += p.z[s.Rank()-1] / float64(p.avail[s.Rank()])
		}
	}
	// Per starter rank, the crib's distribution given my pair (dealer) or
	// their show plus their crib (pone), before nobs from my discards.
	var tsum [13][2 * maxShow]float64
	var cmix [13][maxShow]float64
	var done [13]bool
	for s := engine.Card(0); s < 52; s++ {
		if held[s] || !in(s) {
			continue
		}
		sr := s.Rank()
		if p.z[sr-1] == 0 {
			continue
		}
		ws := p.z[sr-1] / float64(p.avail[sr]) / total
		row := &p.m[sr-1]
		inv := 1 / p.z[sr-1]
		if !done[sr-1] {
			done[sr-1] = true
			for q := 0; q < p.qHi[sr-1]; q++ {
				for pi, x := range row[q] {
					if x == 0 {
						continue
					}
					x *= inv
					c := int(cr[myPair][pi][sr-1])
					pn := 0.0
					if sr != 11 {
						pn = float64(pairJacks[pi]) / 4
					}
					if d.iDeal {
						cmix[sr-1][c] += x * (1 - pn)
						cmix[sr-1][min(c+1, maxShow-1)] += x * pn
					} else {
						tsum[sr-1][q+c] += x * (1 - pn)
						tsum[sr-1][min(q+c+1, 2*maxShow-1)] += x * pn
					}
				}
			}
		}
		for q := 0; q < p.qHi[sr-1]; q++ {
			var x float64
			for _, v := range row[q] {
				x += v
			}
			d.qmix[q] += ws * x * inv
		}
		h := engine.ScoreHand(kept, s, false).Total()
		heels := 0
		if sr == 11 && heelsInShow {
			heels = 2
		}
		nobs := 0 // my discarded jack of the starter's suit
		if sr != 11 && ((disc[0].Rank() == 11 && disc[0].Suit() == s.Suit()) || (disc[1].Rank() == 11 && disc[1].Suit() == s.Suit())) {
			nobs = 1
		}
		if d.iDeal {
			for c, x := range cmix[sr-1] {
				if x != 0 {
					d.hand[min(h+heels+c+nobs, maxShow-1)] += ws * x
				}
			}
			continue
		}
		d.hand[h] += ws
		dst := &d.theirsByH[h]
		for t, x := range tsum[sr-1] {
			if x != 0 {
				dst[min(t+heels+nobs, 2*maxShow-1)] += ws * x
			}
		}
	}
}
