package expert

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// BuildExactKeep runs a discarder (each worker makes its own with newBot,
// usually the bundle's expert with the options it plays with) on every
// six-card hand, on both sides, with the given scores (mine, theirs), and
// returns the share of each six-rank shape's hands that keep each option,
// in keepLayout's order. Hands that differ only by relabelling suits
// discard alike, so each class is decided once and weighted by the hands
// it stands for. progress, if not nil, is called with the shapes done so
// far.
func BuildExactKeep(ctx context.Context, newBot func() bots.Bot, scores [2]int, workers int, progress func(done int)) (*[2][]float32, error) {
	l := keepLayout()
	out := &[2][]float32{make([]float32, len(l.shapes)), make([]float32, len(l.shapes))}
	var shapes [][6]int8
	forEachShape6(func(r [6]int8) { shapes = append(shapes, r) })
	var next, done atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot := newBot()
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(shapes) {
					return
				}
				r := shapes[i]
				k := rank6(r)
				opts := l.shapes[l.start[k]:l.start[k+1]]
				var total float64
				var tally [2][15]float64
				for _, c := range suitClasses(r) {
					total += float64(c.n)
					for side := 0; side < 2; side++ {
						kept := discardShape(bot, c.cards, side, scores)
						j := slices.Index(opts, kept)
						tally[side][j] += float64(c.n)
					}
				}
				for side := 0; side < 2; side++ {
					for j := range opts {
						out[side][int(l.start[k])+j] = float32(tally[side][j] / total)
					}
				}
				if n := done.Add(1); progress != nil && n%1000 == 0 {
					progress(int(n))
				}
			}
		}()
	}
	wg.Wait()
	return out, ctx.Err()
}

// discardShape returns the rank shape bot keeps from six on side (0 the
// dealer) at scores (mine, theirs).
func discardShape(bot bots.Bot, six [6]engine.Card, side int, scores [2]int) int16 {
	var hands [2][6]engine.Card
	hands[0] = six
	n := 0
	var starter engine.Card
	for c := engine.Card(0); c < 52 && n < 7; c++ {
		if slices.Contains(six[:], c) {
			continue
		}
		if n < 6 {
			hands[1][n] = c
		} else {
			starter = c
		}
		n++
	}
	g := engine.NewHand(1, side, scores, hands, starter) // dealer is seat side: 0 when I deal
	var o engine.Observation
	g.ObserveInto(0, &o)
	a := bot.Choose(&o, g.Legal(0))
	var kept [4]engine.Card
	m := 0
	for _, c := range six {
		if c != a.Cards[0] && c != a.Cards[1] {
			kept[m] = c
			m++
		}
	}
	return int16(shapeOf(kept))
}

// suitClass is a six-card hand standing for n hands that differ from it
// only by relabelling suits.
type suitClass struct {
	cards [6]engine.Card
	n     int
}

// suitPerms lists the 24 relabellings of the four suits.
var suitPerms = func() (ps [][4]int) {
	for a := 0; a < 4; a++ {
		for b := 0; b < 4; b++ {
			for c := 0; c < 4; c++ {
				for d := 0; d < 4; d++ {
					if a != b && a != c && a != d && b != c && b != d && c != d {
						ps = append(ps, [4]int{a, b, c, d})
					}
				}
			}
		}
	}
	return ps
}()

// suitClasses lists the hands with sorted ranks r, one per class of
// suit relabellings, with the size of each class.
func suitClasses(r [6]int8) []suitClass {
	counts := map[uint64]int{}
	var suits [6]int
	var rec func(i int)
	rec = func(i int) {
		if i == 6 {
			counts[canonical(r, suits)]++
			return
		}
		for s := 0; s < 4; s++ {
			// Cards of one rank take increasing suits, so each set of
			// suits is counted once.
			if i > 0 && r[i] == r[i-1] && s <= suits[i-1] {
				continue
			}
			suits[i] = s
			rec(i + 1)
		}
	}
	rec(0)
	out := make([]suitClass, 0, len(counts))
	for key, n := range counts {
		var c suitClass
		for i := range c.cards {
			c.cards[i] = engine.Card(key >> (8 * i) & 0xff)
		}
		c.n = n
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b suitClass) int { return slices.Compare(a.cards[:], b.cards[:]) })
	return out
}

// canonical is the least packed sorted hand over every suit relabelling.
func canonical(r [6]int8, suits [6]int) uint64 {
	best := ^uint64(0)
	for _, p := range suitPerms {
		var cs [6]engine.Card
		for i := range cs {
			cs[i] = engine.NewCard(int(r[i]), engine.Suit(p[suits[i]]))
		}
		slices.Sort(cs[:])
		var key uint64
		for i, c := range cs {
			key |= uint64(c) << (8 * i)
		}
		best = min(best, key)
	}
	return best
}

// KeepProb is the opponent model's probability that a player on side (0
// the dealer) dealt six keeps the ranks of kept: with the softmax at weight
// eps against the exact table, or alone with softmaxOnly. It is 0 without
// a model.
func (b *Bundle) KeepProb(eps float64, softmaxOnly bool, side int, six [6]engine.Card, kept [4]engine.Card) float64 {
	m := b.keepModelMix(eps, softmaxOnly)
	if m == nil {
		return 0
	}
	var r [6]int8
	for i, c := range six {
		r[i] = int8(c.Rank())
	}
	slices.Sort(r[:])
	return m.prob(side, r, int16(shapeOf(kept)))
}
