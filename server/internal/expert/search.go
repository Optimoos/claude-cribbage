package expert

import (
	"cribbage/internal/engine"
)

// world is one possible opponent hand, as ranks, with its weight: the
// number of card combinations it stands for (or, when sampled, the number
// of times it was drawn).
type world struct {
	ranks  [4]int8
	n      int8
	weight float64
	show   int // the opponent's show score with this hand
}

const (
	maxShow = 30 // a show scores at most 29
	maxPeg  = 64 // pegging points per player per hand, with room to spare
)

// searcher chooses pegging plays. It keeps scratch space between
// decisions, so it is not safe for concurrent use.
type searcher struct {
	win     *WinTable
	effort  int // most worlds searched per decision; 0 for all
	base    [2]int
	iDeal   bool
	myShow  int
	crib    [maxShow]float64 // probability of each crib score
	show    int              // the current world's opponent show
	worlds  []world
	sampled []world
	// end-of-hand values by (opponent show, my pegging, their pegging),
	// valid when stamp matches gen.
	endVal   []float64
	endStamp []uint32
	gen      uint32
}

func newSearcher(win *WinTable, effort int) *searcher {
	return &searcher{
		win:      win,
		effort:   effort,
		endVal:   make([]float64, maxShow*maxPeg*maxPeg),
		endStamp: make([]uint32, maxShow*maxPeg*maxPeg),
	}
}

// choose returns the pegging play in legal with the highest expected win
// probability; ties go to the earliest.
func (s *searcher) choose(o *engine.Observation, legal []engine.Action) engine.Action {
	me := o.Player
	s.base = [2]int{o.Scores[me], o.Scores[1-me]}
	s.iDeal = o.Dealer == me
	s.myShow = engine.ScoreHand(o.Kept, o.Starter, false).Total()
	s.gen++
	if s.gen == 0 { // wrapped: clear the stamps
		clear(s.endStamp)
		s.gen = 1
	}

	var seen [52]bool
	for _, c := range o.Kept {
		seen[c] = true
	}
	for _, c := range o.Discards {
		seen[c] = true
	}
	seen[o.Starter] = true
	var oppPlayed []engine.Card
	for _, pc := range o.Played[:o.PlayedN] {
		seen[pc.Card] = true
		if pc.Player != me {
			oppPlayed = append(oppPlayed, pc.Card)
		}
	}
	s.cribDistribution(o, &seen)
	s.enumerate(o, &seen, oppPlayed)
	if len(s.worlds) == 0 {
		return legal[0] // inconsistent observation; should not happen
	}
	worlds := s.sample()

	// The root: my hand, the current count and whose card was last.
	var root pegNode
	for _, c := range o.HandCards() {
		root.hand[0][root.n[0]] = int8(c.Rank())
		root.n[0]++
	}
	for _, pc := range o.Sequence() {
		root.seq[root.seqN] = int8(pc.Card.Rank())
		root.seqN++
	}
	root.count = int8(o.Count)
	root.last = -1
	if o.PlayedN > 0 {
		root.last = 0
		if o.Played[o.PlayedN-1].Player != me {
			root.last = 1
		}
	}

	best, bestV := legal[0], -1.0
	var tried uint16
	for _, a := range legal {
		r := int8(a.Cards[0].Rank())
		if tried&(1<<r) != 0 {
			continue
		}
		tried |= 1 << r
		idx := 0
		for root.hand[0][idx] != r {
			idx++
		}
		var v, total float64
		for i := range worlds {
			w := &worlds[i]
			n := root
			n.hand[1], n.n[1] = w.ranks, w.n
			n.turn = 0
			s.show = w.show
			v += w.weight * s.after(&n, idx, -1, 2)
			total += w.weight
		}
		if v /= total; v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// value is the minimax value of a node for player 0, who maximizes their
// win probability while the opponent minimizes it, searched with
// alpha-beta pruning: the result is exact when it lies inside (alpha,
// beta), and otherwise only a bound on the side it falls.
func (s *searcher) value(n *pegNode, alpha, beta float64) float64 {
	p := n.turn
	best := 2.0
	if p == 0 {
		best = -1
	}
	var tried uint16
	for i := 0; i < int(n.n[p]); i++ {
		r := n.hand[p][i]
		if tried&(1<<r) != 0 || n.count+rankValue(r) > 31 {
			continue
		}
		tried |= 1 << r
		c := *n
		v := s.after(&c, i, alpha, beta)
		if p == 0 {
			best = max(best, v)
			alpha = max(alpha, v)
		} else {
			best = min(best, v)
			beta = min(beta, v)
		}
		if alpha >= beta {
			break
		}
	}
	return best
}

// after plays card i for the player to move in c and values the result.
func (s *searcher) after(c *pegNode, i int, alpha, beta float64) float64 {
	switch c.apply(i, s.base) {
	case pegWon:
		if c.turn == 0 {
			return 1
		}
		return 0
	case pegOver:
		return s.end(int(c.pts[0]), int(c.pts[1]))
	}
	return s.value(c, alpha, beta)
}

// end values the end of pegging: the pone's show, the dealer's show and
// the crib in that order, each of which can win the game, then the next
// hand through the win table.
func (s *searcher) end(myPeg, oppPeg int) float64 {
	k := -1
	if myPeg < maxPeg && oppPeg < maxPeg {
		k = (s.show*maxPeg+myPeg)*maxPeg + oppPeg
		if s.endStamp[k] == s.gen {
			return s.endVal[k]
		}
	}
	me, opp := s.base[0]+myPeg, s.base[1]+oppPeg
	var v float64
	if s.iDeal {
		v = s.showThenCrib(opp+s.show, me+s.myShow, false)
	} else {
		v = s.showThenCrib(me+s.myShow, opp+s.show, true)
	}
	if k >= 0 {
		s.endVal[k], s.endStamp[k] = v, s.gen
	}
	return v
}

// showThenCrib takes the scores after the pone's and then the dealer's
// show and returns my win probability after the crib; poneIsMe says which
// player is the pone.
func (s *searcher) showThenCrib(pone, dealer int, poneIsMe bool) float64 {
	mine := func(p float64) float64 {
		if poneIsMe {
			return p
		}
		return 1 - p
	}
	if pone >= engine.WinScore {
		return mine(1)
	}
	if dealer >= engine.WinScore {
		return mine(0)
	}
	// P(the pone wins) after the crib: the pone deals the next hand.
	var pPone float64
	for c, pr := range s.crib {
		if pr == 0 {
			continue
		}
		if dealer+c < engine.WinScore {
			pPone += pr * s.win.Win(pone, dealer+c, true)
		}
	}
	return mine(pPone)
}

// cribDistribution fills s.crib with the distribution of the crib's score:
// my two discards, two of the cards I have not seen and the starter.
func (s *searcher) cribDistribution(o *engine.Observation, seen *[52]bool) {
	var pool []engine.Card
	for c := engine.Card(0); c < 52; c++ {
		if !seen[c] {
			pool = append(pool, c)
		}
	}
	clear(s.crib[:])
	var n float64
	for i := range pool {
		for j := i + 1; j < len(pool); j++ {
			crib := [4]engine.Card{o.Discards[0], o.Discards[1], pool[i], pool[j]}
			s.crib[engine.ScoreHand(crib, o.Starter, true).Total()]++
			n++
		}
	}
	for i := range s.crib {
		s.crib[i] /= n
	}
}

// enumerate fills s.worlds with every rank multiset the opponent may hold,
// consistent with the cards I have seen and with every go they have said.
func (s *searcher) enumerate(o *engine.Observation, seen *[52]bool, oppPlayed []engine.Card) {
	minValue := goFloor(o) + 1
	var avail [14]int
	for c := engine.Card(0); c < 52; c++ {
		if !seen[c] && c.Value() >= minValue {
			avail[c.Rank()]++
		}
	}
	s.worlds = s.worlds[:0]
	k := o.OppHandN
	var ranks [4]int8
	var rec func(r, left int, weight float64)
	rec = func(r, left int, weight float64) {
		if left == 0 {
			w := world{ranks: ranks, n: int8(k), weight: weight}
			w.show = oppShow(oppPlayed, ranks[:k], o.Starter)
			s.worlds = append(s.worlds, w)
			return
		}
		if r > 13 {
			return
		}
		at := k - left
		for m := 0; m <= min(left, avail[r]); m++ {
			for j := 0; j < m; j++ {
				ranks[at+j] = int8(r)
			}
			rec(r+1, left-m, weight*float64(choose(avail[r], m)))
		}
	}
	rec(1, k, 1)
}

// sample returns the worlds to search: all of them, or a systematic sample
// of s.effort drawn in proportion to weight, which is deterministic.
func (s *searcher) sample() []world {
	if s.effort <= 0 || len(s.worlds) <= s.effort {
		return s.worlds
	}
	var total float64
	for _, w := range s.worlds {
		total += w.weight
	}
	s.sampled = s.sampled[:0]
	step := total / float64(s.effort)
	next, cum := step/2, 0.0
	for _, w := range s.worlds {
		cum += w.weight
		picks := 0
		for next < cum {
			picks++
			next += step
		}
		if picks > 0 {
			w.weight = float64(picks)
			s.sampled = append(s.sampled, w)
		}
	}
	return s.sampled
}

// goFloor returns a card value v such that every card the opponent still
// holds is known to be worth more than v.
//
// The opponent could not play when I played twice running within one
// count, and neither player could when the count was reset below 31. The
// cards they hold now were in their hand then.
func goFloor(o *engine.Observation) int {
	me := o.Player
	floor := 0
	count, oppPlayed := 0, 0
	played := o.Played[:o.PlayedN]
	for i, pc := range played {
		v := pc.Card.Value()
		if count+v > 31 { // reset by a go before this card
			if oppPlayed < 4 {
				floor = max(floor, 31-count)
			}
			count = 0
		}
		count += v
		if pc.Player != me {
			oppPlayed++
		}
		if count == 31 {
			count = 0
			continue
		}
		if i+1 < len(played) && pc.Player == me && played[i+1].Player == me &&
			count+played[i+1].Card.Value() <= 31 && oppPlayed < 4 {
			floor = max(floor, 31-count)
		}
	}
	// It is my turn again right after my own card: the opponent said go.
	if len(played) > 0 && played[len(played)-1].Player == me && o.Count > 0 && oppPlayed < 4 {
		floor = max(floor, 31-o.Count)
	}
	return floor
}

// oppShow scores the opponent's hand: the cards they have played, which
// are known, and the hidden ranks. Suits of hidden cards are unknown, so a
// flush and nobs count only when every card involved is known.
func oppShow(known []engine.Card, hidden []int8, starter engine.Card) int {
	var hand [4]engine.Card
	n := copy(hand[:], known)
	for _, r := range hidden {
		hand[n] = engine.NewCard(int(r), engine.Clubs)
		n++
	}
	b := engine.ScoreHand(hand, starter, false)
	if len(hidden) == 0 {
		return b.Total()
	}
	show := b.Fifteens + b.Pairs + b.Runs
	for _, c := range known {
		if c.Rank() == 11 && c.Suit() == starter.Suit() {
			show++
			break
		}
	}
	return show
}

func choose(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	r := 1
	for i := 0; i < k; i++ {
		r = r * (n - i) / (i + 1)
	}
	return r
}
