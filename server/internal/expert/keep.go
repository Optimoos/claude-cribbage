package expert

import (
	"math"
	"sort"
	"sync"

	"cribbage/internal/engine"
)

// keepModel says how likely a player is to keep each four ranks of a
// six-card rank shape: a softmax, at a temperature fitted to self-play, over
// the option's expected hand score, its pegging edge and the crib it makes.
type keepModel struct {
	temp [2]float64 // by side; 0 means the model is not fitted
	rows [2]map[uint32]keepRow
}

type keepRow struct {
	shapes []int16   // kept four-rank shapes
	probs  []float32 // probability of keeping each
}

// handEVByShape[shape][starter rank-1] is the hand's fifteens, pairs and
// runs with that starter (suits, so flushes and nobs, are ignored).
var (
	handEVOnce    sync.Once
	handEVByShape [numShapes][13]float32
	shapeRanks    [numShapes][4]int8
)

func handEVTable() {
	handEVOnce.Do(func() {
		for a := 1; a <= 13; a++ {
			for b := a; b <= 13; b++ {
				for c := b; c <= 13; c++ {
					for d := c; d <= 13; d++ {
						s := shapeIdx[((a*14+b)*14+c)*14+d]
						shapeRanks[s] = [4]int8{int8(a), int8(b), int8(c), int8(d)}
						kept := [4]engine.Card{engine.NewCard(a, 0), engine.NewCard(b, 1), engine.NewCard(c, 2), engine.NewCard(d, 3)}
						for sr := 1; sr <= 13; sr++ {
							bd := engine.ScoreHand(kept, engine.NewCard(sr, 0), false)
							handEVByShape[s][sr-1] = float32(bd.Fifteens + bd.Pairs + bd.Runs)
						}
					}
				}
			}
		}
	})
}

// key6 packs six sorted ranks.
func key6(r [6]int8) uint32 {
	var k uint32
	for _, x := range r {
		k = k<<4 | uint32(x)
	}
	return k
}

// keepValues caches the parts of an option's value that come from the
// discard tables.
type keepValues struct {
	crib    [2][numPairs]float64  // average crib by side and discarded pair
	pegEdge [2][numShapes]float64 // my average pegging minus theirs
}

func newKeepValues(t *DiscardTables) *keepValues {
	handEVTable()
	kv := new(keepValues)
	for side := 0; side < 2; side++ {
		for a := 1; a <= 13; a++ {
			for b := a; b <= 13; b++ {
				kv.crib[side][pairIdx[a][b]] = t.CribMean(side, a, b)
			}
		}
		for s := 0; s < numShapes; s++ {
			kv.pegEdge[side][s] = pegMean(t, side, s, 0) - pegMean(t, side, s, 1)
		}
	}
	return kv
}

// keepOptions lists the distinct ways to keep four of six sorted ranks,
// with each option's value for a player on side.
func (kv *keepValues) keepOptions(r [6]int8, side int) (shapes []int16, values []float64) {
	var held [14]int
	for _, x := range r {
		held[x]++
	}
	seen := map[int16]bool{}
	for i := 0; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			var k [4]int
			n := 0
			for m := 0; m < 6; m++ {
				if m != i && m != j {
					k[n] = int(r[m])
					n++
				}
			}
			shape := shapeIdx[((k[0]*14+k[1])*14+k[2])*14+k[3]]
			if seen[shape] {
				continue
			}
			seen[shape] = true
			// Expected hand over the starters the player cannot see.
			var ev, n2 float64
			for sr := 1; sr <= 13; sr++ {
				w := float64(4 - held[sr])
				ev += w * float64(handEVByShape[shape][sr-1])
				n2 += w
			}
			v := ev/n2 + kv.pegEdge[side][shape]
			crib := kv.crib[side][pairIdx[r[i]][r[j]]]
			if side == 0 {
				v += crib
			} else {
				v -= crib
			}
			shapes = append(shapes, shape)
			values = append(values, v)
		}
	}
	return shapes, values
}

func pegMean(t *DiscardTables, side, shape, who int) float64 {
	var m float64
	for k, p := range t.peg(side, shape, who) {
		m += float64(k) * float64(p)
	}
	return m
}

func softmax(values []float64, temp float64, out []float32) {
	best := math.Inf(-1)
	for _, v := range values {
		best = max(best, v)
	}
	var z float64
	for i, v := range values {
		e := math.Exp((v - best) / temp)
		out[i] = float32(e)
		z += e
	}
	for i := range out {
		out[i] /= float32(z)
	}
}

// newKeepModel precomputes the keep probabilities of every six-rank shape.
func newKeepModel(t *DiscardTables, temp [2]float64) *keepModel {
	m := &keepModel{temp: temp}
	kv := newKeepValues(t)
	for side := 0; side < 2; side++ {
		m.rows[side] = map[uint32]keepRow{}
		forEachShape6(func(r [6]int8) {
			shapes, values := kv.keepOptions(r, side)
			row := keepRow{shapes: shapes, probs: make([]float32, len(shapes))}
			softmax(values, temp[side], row.probs)
			m.rows[side][key6(r)] = row
		})
	}
	return m
}

// forEachShape6 calls f with every sorted six-rank multiset (at most four
// of a rank).
func forEachShape6(f func([6]int8)) {
	var r [6]int8
	var rec func(i int, from int8)
	rec = func(i int, from int8) {
		if i == 6 {
			f(r)
			return
		}
		for x := from; x <= 13; x++ {
			if i >= 4 && r[i-4] == x { // a fifth card of a rank
				continue
			}
			r[i] = x
			rec(i+1, x)
		}
	}
	rec(0, 1)
}

// prob is the probability that a player on side holding six sorted ranks
// keeps the four-rank shape.
func (m *keepModel) prob(side int, six [6]int8, shape int16) float64 {
	row := m.rows[side][key6(six)]
	for i, s := range row.shapes {
		if s == shape {
			return float64(row.probs[i])
		}
	}
	return 0
}

// FitKeepTemps finds, for each side, the softmax temperature under which
// the observed discards are most likely.
func FitKeepTemps(t *DiscardTables, hands []HandStat) (temps [2]float64, logLik [2]float64) {
	type obs struct {
		values []float64
		chosen int
	}
	kv := newKeepValues(t)
	for side := 0; side < 2; side++ {
		var data []obs
		for i := range hands {
			h := &hands[i]
			var r [6]int8
			for j, c := range h.Kept[side] {
				r[j] = int8(c.Rank())
			}
			r[4], r[5] = int8(h.Discards[side][0].Rank()), int8(h.Discards[side][1].Rank())
			sort.Slice(r[:], func(a, b int) bool { return r[a] < r[b] })
			shapes, values := kv.keepOptions(r, side)
			if len(shapes) < 2 {
				continue
			}
			kept := int16(shapeOf(h.Kept[side]))
			for j, s := range shapes {
				if s == kept {
					data = append(data, obs{values, j})
				}
			}
			if len(data) >= 50000 {
				break
			}
		}
		ll := func(temp float64) float64 {
			var sum float64
			p := make([]float32, 15)
			for _, o := range data {
				softmax(o.values, temp, p[:len(o.values)])
				sum += math.Log(max(float64(p[o.chosen]), 1e-12))
			}
			return sum / float64(len(data))
		}
		// Golden-section search on log temperature.
		lo, hi := math.Log(0.05), math.Log(20.0)
		g := (math.Sqrt(5) - 1) / 2
		a, b := hi-g*(hi-lo), lo+g*(hi-lo)
		fa, fb := ll(math.Exp(a)), ll(math.Exp(b))
		for i := 0; i < 40; i++ {
			if fa > fb {
				hi, b, fb = b, a, fa
				a = hi - g*(hi-lo)
				fa = ll(math.Exp(a))
			} else {
				lo, a, fa = a, b, fb
				b = lo + g*(hi-lo)
				fb = ll(math.Exp(b))
			}
		}
		temps[side] = math.Exp((lo + hi) / 2)
		logLik[side] = ll(temps[side])
	}
	return temps, logLik
}
