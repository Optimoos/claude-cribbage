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
// Every unseen card is equally likely to be in their hand, and they play
// the reply that scores them most, ties going to the one that leaves me
// least. (Valuing the go as everything I could then score alone did no
// better; weighting every reply they might hold, as Heuristic's risk does,
// did much worse once my answers were counted.)
func (h *Heuristic2) choosePlay(o *engine.Observation, legal []engine.Action) engine.Action {
	var seq [16]engine.Card
	n := 0
	for _, pc := range o.Sequence() {
		seq[n] = pc.Card
		n++
	}
	var unseen [52]engine.Card
	nu := unseenCards(o, &unseen)
	k := o.OppHandN
	// After my own card it is my turn again only if they said go.
	if n > 0 && o.Played[o.PlayedN-1].Player == o.Player {
		k = 0
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
			v += h.lookahead(seq[:n+1], count, rest[:nr], unseen[:nu], k)
		}
		// Slight preference for shedding high cards early, as Heuristic.
		v += 0.01 * float64(c.Value())
		if v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// reply is one card the opponent might answer with.
type reply struct{ theirs, mine int }

// lookahead values the position after my card took the count to count
// below 31: seq is the current sequence ending with it, rest my remaining
// cards, and the opponent holds k of the unseen cards.
func (h *Heuristic2) lookahead(seq []engine.Card, count int, rest, unseen []engine.Card, k int) float64 {
	h.replies = h.replies[:0]
	buf := append(h.seqBuf[:0], seq...)
	for _, u := range unseen {
		if count+u.Value() > 31 {
			continue
		}
		s := append(buf, u)
		r := reply{theirs: engine.ScorePeg(s).Total()}
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

	// noneOf(m) is the probability that they hold none of m given unseen cards.
	nu := len(unseen)
	noneOf := func(m int) float64 {
		p := 1.0
		for t := 0; t < k; t++ {
			if nu-m-t <= 0 {
				return 0
			}
			p *= float64(nu-m-t) / float64(nu-t)
		}
		return p
	}
	var v float64
	prev := 1.0
	for i, r := range h.replies {
		next := noneOf(i + 1)
		v += (prev - next) * float64(r.mine-r.theirs)
		prev = next
	}
	return v + prev
}
