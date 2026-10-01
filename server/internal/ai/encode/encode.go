// Package encode turns a player's observation into neural network features
// and computes afterstates: the observation a player would have right after
// taking an action, using only information that player can see.
package encode

import (
	"cribbage/internal/ai/nn"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// Version changes whenever the feature layout changes; models record the
// version they were trained with.
const Version = 2

// Feature layout. Card features are 52-way (suit*13+rank-1); rank features
// are 13-way counts scaled by 1/4. All are from the observer's perspective.
const (
	offHand       = 0               // cards still held
	offKept       = 52 * 1          // the four cards kept for the show
	offDiscards   = 52 * 2          // our two crib cards
	offStarter    = 52 * 3          // the starter, once cut
	offOppPlayed  = 52 * 4          // cards the opponent has played this hand
	offSeq        = 52 * 5          // cards in the current count (either player)
	offRanks      = 52 * 6          // six 13-way rank histograms, same order as above
	offCount      = offRanks + 13*6 // one-hot count 0..31
	offCountScale = offCount + 32
	offScores     = offCountScale + 1 // mine/121, theirs/121, (mine-theirs)/121
	offThermo     = offScores + 3     // 12 buckets per player: score >= 10k
	offFlags      = offThermo + 24
	// Flags: dealer, phase discard, phase pegging, opp hand/4, my hand/6,
	// opp discarded, I played the last card of the count, crib is full.
	numFlags = 8
	// Engineered features, so the network need not learn hand scoring from
	// scratch: my show value (exact once the starter is cut, otherwise the
	// average over possible starters), the expected crib value of my
	// discards (split by whose crib it is), the points my last play
	// pegged, and the opponent's expected points in reply.
	offEngineered = offFlags + numFlags
	numEngineered = 5
	// Size is the number of input features.
	Size = offEngineered + numEngineered
)

// Encode writes o's features into x.
func Encode(o *engine.Observation, x *nn.Sparse) {
	x.Reset()
	var ranks [6][14]int
	card := func(group, off int, c engine.Card) {
		if c.Valid() {
			x.Set(off+int(c), 1)
			ranks[group][c.Rank()]++
		}
	}
	for _, c := range o.HandCards() {
		card(0, offHand, c)
	}
	if o.Discarded {
		for _, c := range o.Kept {
			card(1, offKept, c)
		}
		for _, c := range o.Discards {
			card(2, offDiscards, c)
		}
	}
	card(3, offStarter, o.Starter)
	for _, pc := range o.Played[:o.PlayedN] {
		if pc.Player != o.Player {
			card(4, offOppPlayed, pc.Card)
		}
	}
	for _, pc := range o.Sequence() {
		card(5, offSeq, pc.Card)
	}
	for g := range ranks {
		for r := 1; r <= 13; r++ {
			x.Set(offRanks+13*g+r-1, float32(ranks[g][r])/4)
		}
	}

	x.Set(offCount+o.Count, 1)
	x.Set(offCountScale, float32(o.Count)/31)

	me, them := o.Scores[o.Player], o.Scores[1-o.Player]
	x.Set(offScores, float32(me)/engine.WinScore)
	x.Set(offScores+1, float32(them)/engine.WinScore)
	x.Set(offScores+2, float32(me-them)/engine.WinScore)
	for k := 0; k < 12; k++ {
		if me >= 10*(k+1) {
			x.Set(offThermo+k, 1)
		}
		if them >= 10*(k+1) {
			x.Set(offThermo+12+k, 1)
		}
	}

	f := offFlags
	if o.Dealer == o.Player {
		x.Set(f, 1)
	}
	switch o.Phase {
	case engine.PhaseDiscard:
		x.Set(f+1, 1)
	case engine.PhasePegging:
		x.Set(f+2, 1)
	}
	x.Set(f+3, float32(o.OppHandN)/4)
	x.Set(f+4, float32(o.HandN)/6)
	if o.OppDiscarded {
		x.Set(f+5, 1)
	}
	if seq := o.Sequence(); len(seq) > 0 && seq[len(seq)-1].Player == o.Player {
		x.Set(f+6, 1)
	}
	if o.Discarded && o.OppDiscarded {
		x.Set(f+7, 1)
	}
	engineered(o, x)
}

func engineered(o *engine.Observation, x *nn.Sparse) {
	e := offEngineered
	if o.Discarded {
		if o.Starter.Valid() {
			x.Set(e, float32(engine.ScoreHand(o.Kept, o.Starter, false).Total())/20)
		} else {
			var seen [52]bool
			for _, c := range o.Kept {
				seen[c] = true
			}
			for _, c := range o.Discards {
				seen[c] = true
			}
			x.Set(e, float32(bots.HandEV(o.Kept, &seen))/20)
		}
		crib := float32(bots.CribEV(o.Discards[0], o.Discards[1])) / 10
		if o.Dealer == o.Player {
			x.Set(e+1, crib)
		} else {
			x.Set(e+2, crib)
		}
	}
	if o.Phase != engine.PhasePegging {
		return
	}
	var seq [9]engine.Card
	n := 0
	for _, pc := range o.Sequence() {
		seq[n] = pc.Card
		n++
	}
	if n > 0 && o.Played[o.PlayedN-1].Player == o.Player {
		x.Set(e+3, float32(engine.ScorePeg(seq[:n]).Total())/6)
	}
	if o.OppHandN > 0 {
		x.Set(e+4, float32(oppReply(o, seq[:n]))/4)
	}
}

// oppReply is the expected number of points the opponent can peg with
// their next card, assuming each unseen card is in their hand with equal
// probability.
func oppReply(o *engine.Observation, seq []engine.Card) float64 {
	var seen [52]bool
	mark := func(c engine.Card) {
		if c.Valid() {
			seen[c] = true
		}
	}
	for _, c := range o.Kept {
		mark(c)
	}
	for _, c := range o.Discards {
		mark(c)
	}
	mark(o.Starter)
	for _, pc := range o.Played[:o.PlayedN] {
		mark(pc.Card)
	}
	var buf [10]engine.Card
	copy(buf[:], seq)
	total, unseen := 0, 0
	for c := engine.Card(0); c < 52; c++ {
		if seen[c] {
			continue
		}
		unseen++
		if o.Count+c.Value() <= 31 {
			buf[len(seq)] = c
			total += engine.ScorePeg(buf[:len(seq)+1]).Total()
		}
	}
	if unseen == 0 {
		return 0
	}
	return float64(total) * float64(o.OppHandN) / float64(unseen)
}

// Afterstate returns the observation o would become right after the
// observer takes action a, and whether that action wins the game outright.
// Only points the observer can be sure of are applied (pegging points and
// the last card); a go depends on the opponent's hidden cards and is left
// for the network to judge.
func Afterstate(o *engine.Observation, a engine.Action) (engine.Observation, bool) {
	after := *o
	after.MustAct = false
	switch a.Kind {
	case engine.ActDiscard:
		n := 0
		var kept [6]engine.Card
		for _, c := range o.HandCards() {
			if c != a.Cards[0] && c != a.Cards[1] {
				kept[n] = c
				n++
			}
		}
		after.Hand = kept
		for i := n; i < len(after.Hand); i++ {
			after.Hand[i] = engine.NoCard
		}
		after.HandN = n
		copy(after.Kept[:], kept[:4])
		after.Discards = [2]engine.Card{a.Cards[0], a.Cards[1]}
		after.Discarded = true
		return after, false

	case engine.ActPlay:
		c := a.Cards[0]
		n := 0
		for _, h := range o.HandCards() {
			if h != c {
				after.Hand[n] = h
				n++
			}
		}
		for i := n; i < len(after.Hand); i++ {
			after.Hand[i] = engine.NoCard
		}
		after.HandN = n
		after.Played[after.PlayedN] = engine.PlayedCard{Card: c, Player: o.Player}
		after.PlayedN++
		after.Count += c.Value()

		var seq [8]engine.Card
		k := 0
		for _, pc := range after.Sequence() {
			seq[k] = pc.Card
			k++
		}
		pts := engine.ScorePeg(seq[:k]).Total()
		if after.HandN == 0 && after.OppHandN == 0 && after.Count != 31 {
			pts++ // last card
		}
		if after.Count == 31 {
			after.Count = 0
			after.SeqStart = after.PlayedN
		}
		after.Scores[o.Player] += pts
		if after.Scores[o.Player] >= engine.WinScore {
			after.Scores[o.Player] = engine.WinScore
			return after, true
		}
		return after, false
	}
	return after, false
}
