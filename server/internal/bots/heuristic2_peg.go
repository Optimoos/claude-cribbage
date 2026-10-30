package bots

import (
	"math"
	"slices"

	"cribbage/internal/engine"
)

// choosePlay looks three plays ahead: my card, the opponent's reply and my
// answer to it. A card is worth
//
//	points now
//	+ P(they cannot play), for the go point I can then expect
//	+ Σ P(their best scoring card is u) × (my best answer to u − their points for u)
//
// Which cards they hold comes from holdings (M19). They play the reply
// that scores them most, ties going to the one that leaves me least. (Valuing the go as everything I could then score alone did no
// better; weighting every reply they might hold, as Heuristic's risk does,
// did much worse once my answers were counted.)
func (h *Heuristic2) choosePlay(o *engine.Observation, legal []engine.Action) engine.Action {
	var seq [16]engine.Card
	n := 0
	for _, pc := range o.Sequence() {
		seq[n] = pc.Card
		n++
	}
	h.opp.observe(o, true, true)
	// After my own card it is my turn again only if they said go.
	if n > 0 && o.Played[o.PlayedN-1].Player == o.Player {
		h.opp.k = 0
	}
	mine := o.HandCards()

	best, bestV := legal[0], math.Inf(-1)
	for _, a := range legal {
		c := a.Cards[0]
		seq[n] = c
		v := float64(engine.ScorePeg(seq[:n+1]).Total())
		count := o.Count + c.Value()
		if count < 31 {
			var rest [4]engine.Card
			nr := 0
			for _, d := range mine {
				if d != c {
					rest[nr] = d
					nr++
				}
			}
			v += h.lookahead(seq[:n+1], count, rest[:nr])
		}
		// Slight preference for shedding high cards early, as Heuristic.
		v += 0.01 * float64(c.Value())
		if v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// holdings is what I believe about the opponent's hand: they hold k of the
// cards, each in proportion to its weight.
//
// The weight of a card is how much likelier heuristic self-play is to keep
// its rank than a random card's, on the opponent's side of the deal. A
// card the gos rule out (worth no more than GoFloor) weighs nothing.
type holdings struct {
	cards [52]engine.Card
	w     [52]float64
	n     int     // cards with weight
	total float64 // their weight
	k     int
}

// observe sets the holdings from o, using the rank prior and the go floor
// as asked (without both, it is plain card counting).
func (m *holdings) observe(o *engine.Observation, prior, floor bool) {
	var unseen [52]engine.Card
	nu := unseenCards(o, &unseen)
	side := 0
	if o.Dealer == o.Player {
		side = 1
	}
	minValue := 1
	if floor {
		minValue = GoFloor(o) + 1
	}
	m.k, m.n, m.total = o.OppHandN, 0, 0
	for _, u := range unseen[:nu] {
		if u.Value() < minValue {
			continue
		}
		w := 1.0
		if prior {
			w = float64(h2Keep[side][u.Rank()-1])
		}
		m.cards[m.n], m.w[m.n] = u, w
		m.total += w
		m.n++
	}
}

// noneOf is the probability that they hold none of n cards of total
// weight w. Drawing their k cards in proportion to weight, each draw
// misses the set with probability (weight left outside it) / (weight
// left), taking each card drawn to weigh the average of what it was drawn
// from. With equal weights this is the exact hypergeometric probability.
func (m *holdings) noneOf(n int, w float64) float64 {
	p := 1.0
	out, outW := m.n-n, m.total-w
	for t := 0; t < m.k; t++ {
		if out-t <= 0 {
			return 0
		}
		p *= outW * float64(out-t) / float64(out) / (m.total * float64(m.n-t) / float64(m.n))
	}
	return p
}

// reply is one card the opponent might answer with: their points, my best
// answer's and its weight in holdings.
type reply struct {
	theirs, mine int
	w            float64
}

// lookahead values the position after my card took the count to count
// below 31: seq is the current sequence ending with it and rest my
// remaining cards.
func (h *Heuristic2) lookahead(seq []engine.Card, count int, rest []engine.Card) float64 {
	h.replies = h.replies[:0]
	buf := append(h.seqBuf[:0], seq...)
	for i, u := range h.opp.cards[:h.opp.n] {
		if count+u.Value() > 31 {
			continue
		}
		s := append(buf, u)
		r := reply{theirs: engine.ScorePeg(s).Total(), w: h.opp.w[i]}
		for _, d := range rest {
			if count+u.Value()+d.Value() <= 31 {
				r.mine = max(r.mine, engine.ScorePeg(append(s, d)).Total())
			}
		}
		h.replies = append(h.replies, r)
	}
	slices.SortFunc(h.replies, func(a, b reply) int {
		if a.theirs != b.theirs {
			return b.theirs - a.theirs
		}
		return a.mine - b.mine
	})

	var v, w float64
	prev := 1.0
	for i, r := range h.replies {
		w += r.w
		next := h.opp.noneOf(i+1, w)
		v += (prev - next) * float64(r.mine-r.theirs)
		prev = next
	}
	return v + prev
}
