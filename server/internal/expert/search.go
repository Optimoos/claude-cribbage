package expert

import (
	"math"
	"sort"
	"sync"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// world is one possible opponent hand, as ranks, with its weight: the
// number of card combinations it stands for (or, when sampled, the number
// of times it was drawn). Its show is filled in only for the worlds
// searched.
type world struct {
	ranks  [4]int8
	n      int8
	weight float64
	show   int // the opponent's show score with this hand, without hidden suits
	// The chances that hidden suits add nobs, or a flush of four or five
	// (taken as independent of each other).
	pNobs, pFlush4, pFlush5 float32
}

const (
	maxShow = 30 // a show scores at most 29
	maxPeg  = 32 // pegging points per player per hand cached; more is computed uncached
)

// searcher chooses pegging plays. It keeps scratch space between
// decisions, so it is not safe for concurrent use.
type searcher struct {
	win      *WinTable
	effort   int // most worlds searched per decision; 0 for all
	base     [2]int
	iDeal    bool
	myShow   int
	crib     [maxShow]float64   // probability of each crib score
	world    *world             // the world being searched
	cribs    [][maxShow]float64 // per searched world in the endgame: the crib given that world
	worlds   []world
	pool     []world
	sampled  []world
	weighted []world
	// Opponent modelling: worlds are weighted by how likely the opponent
	// was to keep that hand (keep, nil to skip) and to make the plays they
	// made with it (a softmax on immediate points at pegTemp, 0 to skip).
	keep      *keepModel
	pegTemp   float64
	pegPol    *PegPolicy // the fitted pegging policy (proposal 2); replaces pegTemp's softmax
	pegMix    float64    // the softmax's weight mixed into pegPol's chances, for robustness
	oppPlays  []oppPlay  // the opponent's plays this hand, prepared for pegPol
	keepKey   keepCacheKey
	keepGen   uint32 // keepVal[s] is valid for keepKey when keepStamp[s] == keepGen
	keepStamp [numShapes]uint32
	keepVal   [numShapes]float64
	ends      *endCache // borrowed from endCaches during choose

	// A test oracle sees the game: oracle says what it may use of the
	// opponent's hand.
	oracle Oracle

	mixFirst float64 // proposal 9: mix my first play among plays this close to the best
	seed     uint64  // draws the mixed plays
	game     *engine.Game
	seat     int

	noFixes bool // M14's search: no hidden suits in their show, one crib for every world

	isSearch bool // information-set search (issearch.go) instead of minimax per world
	isVals   []float64
	isb      *isBuf // borrowed from isBufs during an information-set search
	// isLambda blends the opponent's nodes: λ·(their best reply) +
	// (1-λ)·(their play by pegPol), 1 for best reply alone.
	isLambda float64
	isAvail  [14]int8 // each rank's count unseen from the public view at the root
}

// endCache holds end-of-hand values by (opponent show, my pegging, their
// pegging), valid when stamp matches gen. At about 370 KB it is borrowed
// only while choosing, so idle bots (one per game) do not each hold one.
type endCache struct {
	val   []float64
	stamp []uint32
	gen   uint32
}

var endCaches = sync.Pool{New: func() any {
	return &endCache{val: make([]float64, maxShow*maxPeg*maxPeg), stamp: make([]uint32, maxShow*maxPeg*maxPeg)}
}}

func newSearcher(win *WinTable, effort int) *searcher {
	return &searcher{win: win, effort: effort}
}

// choose returns the pegging play in legal with the highest expected win
// probability; ties go to the earliest.
func (s *searcher) choose(o *engine.Observation, legal []engine.Action) engine.Action {
	cands, probs := s.dist(o, legal)
	if len(cands) == 1 {
		return cands[0]
	}
	u := float64(mixHash(s.seed, o)>>11) / (1 << 53)
	for i, p := range probs {
		if u -= p; u < 0 {
			return cands[i]
		}
	}
	return cands[len(cands)-1]
}

// dist is the pegging policy: the plays it may make and their chances.
// Without mixing it is the best play (ties to the earliest) with chance 1.
// With mixFirst > 0, on my first play of a hand with both scores below
// mixBelow, it is uniform over the plays whose value is within mixFirst
// of the best (proposal 9: a deterministic first card tells an opponent
// who knows the strategy much about the rest of the hand).
func (s *searcher) dist(o *engine.Observation, legal []engine.Action) ([]engine.Action, []float64) {
	cands, vals := s.values(o, legal)
	best := 0
	for i := range vals {
		if vals[i] > vals[best] {
			best = i
		}
	}
	first := true
	for _, pc := range o.Played[:o.PlayedN] {
		if pc.Player == o.Player {
			first = false
		}
	}
	if s.mixFirst <= 0 || !first || max(o.Scores[0], o.Scores[1]) >= mixBelow || len(cands) < 2 {
		return cands[best : best+1], []float64{1}
	}
	var mc []engine.Action
	for i := range vals {
		if vals[i] >= vals[best]-s.mixFirst {
			mc = append(mc, cands[i])
		}
	}
	probs := make([]float64, len(mc))
	for i := range probs {
		probs[i] = 1 / float64(len(mc))
	}
	return mc, probs
}

// mixBelow: the first play is mixed only while both scores are below it.
const mixBelow = 90

// mixHash draws a mixed play from the bot's seed and what it sees, so a
// game replays exactly given the seed.
func mixHash(seed uint64, o *engine.Observation) uint64 {
	h := seed ^ 0x9e3779b97f4a7c15
	mix := func(x uint64) {
		h ^= x
		h *= 0xbf58476d1ce4e5b9
		h ^= h >> 31
	}
	mix(uint64(o.HandNum)<<32 | uint64(o.Scores[0])<<16 | uint64(o.Scores[1]))
	for _, c := range o.Kept {
		mix(uint64(c))
	}
	mix(uint64(o.Starter))
	for _, pc := range o.Played[:o.PlayedN] {
		mix(uint64(pc.Card)<<1 | uint64(pc.Player))
	}
	return h
}

// values returns each distinct play (one card per rank) and its value.
func (s *searcher) values(o *engine.Observation, legal []engine.Action) ([]engine.Action, []float64) {
	me := o.Player
	s.base = [2]int{o.Scores[me], o.Scores[1-me]}
	s.iDeal = o.Dealer == me
	s.myShow = engine.ScoreHand(o.Kept, o.Starter, false).Total()
	s.ends = endCaches.Get().(*endCache)
	defer func() {
		endCaches.Put(s.ends)
		s.ends = nil
	}()
	s.newEnds()

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
	if s.oracle != NoOracle && s.game != nil {
		s.peek(o, oppPlayed)
	}
	if len(s.worlds) == 0 {
		return legal[:1], []float64{0} // inconsistent observation; should not happen
	}
	// Draw a pool by card counts, weight it by the opponent model, then
	// draw the worlds to search by those weights. The model's cost is spent
	// on the pool rather than on every world.
	worlds := s.worlds
	if s.effort > 0 && len(worlds) > poolFactor*s.effort {
		worlds = systematic(worlds, poolFactor*s.effort, &s.pool)
	}
	worlds = s.reweight(o, oppPlayed, worlds)
	if s.effort > 0 && len(worlds) > s.effort {
		worlds = systematic(worlds, s.effort, &s.sampled)
	}
	for i := range worlds { // only the searched worlds need their show
		w := &worlds[i]
		w.show = oppShow(oppPlayed, w.ranks[:w.n], o.Starter)
		if !s.noFixes {
			w.pNobs, w.pFlush4, w.pFlush5 = hiddenSuits(oppPlayed, w.ranks[:w.n], o.Starter, &seen)
		}
	}

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

	// Near 121 the crib's distribution matters through the win table, and
	// it depends on the world: what they kept says what they threw.
	worldCribs := !s.noFixes && max(s.base[0], s.base[1]) >= cribByWorldFrom
	if worldCribs {
		s.worldCribs(o, oppPlayed, &seen, worlds)
	}
	var cands []engine.Action
	var vals []float64
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
		if s.isSearch {
			s.isAvail = publicAvail(o)
			s.isVals = append(s.isVals[:0], make([]float64, len(worlds))...)
			s.isRoot(root, idx, worlds, worldCribs, s.isVals)
			for i := range worlds {
				v += worlds[i].weight * s.isVals[i]
				total += worlds[i].weight
			}
			cands, vals = append(cands, a), append(vals, v/total)
			continue
		}
		for i := range worlds {
			w := &worlds[i]
			n := root
			n.hand[1], n.n[1] = w.ranks, w.n
			n.turn = 0
			s.world = w
			if worldCribs {
				s.crib = s.cribs[i]
				s.newEnds() // end values depend on the crib
			}
			v += w.weight * s.after(n, idx, -1, 2)
			total += w.weight
		}
		cands, vals = append(cands, a), append(vals, v/total)
	}
	return cands, vals
}

// value is the minimax value of a node for player 0, who maximizes their
// win probability while the opponent minimizes it, searched with
// alpha-beta pruning: the result is exact when it lies inside (alpha,
// beta), and otherwise only a bound on the side it falls.
//
// Nodes pass by value: a pointer to a copy would escape to the heap
// through the recursion, one allocation per node.
func (s *searcher) value(n pegNode, alpha, beta float64) float64 {
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
		v := s.after(n, i, alpha, beta)
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
func (s *searcher) after(c pegNode, i int, alpha, beta float64) float64 {
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
	w := s.world
	if w.pNobs == 0 && w.pFlush4 == 0 && w.pFlush5 == 0 {
		return s.endAt(w.show, myPeg, oppPeg)
	}
	pn, p4, p5 := float64(w.pNobs), float64(w.pFlush4), float64(w.pFlush5)
	var v float64
	for _, f := range [3]struct {
		pts int
		p   float64
	}{{0, 1 - p4 - p5}, {4, p4}, {5, p5}} {
		if f.p == 0 {
			continue
		}
		v += f.p * (1 - pn) * s.endAt(min(w.show+f.pts, maxShow-1), myPeg, oppPeg)
		if pn > 0 {
			v += f.p * pn * s.endAt(min(w.show+f.pts+1, maxShow-1), myPeg, oppPeg)
		}
	}
	return v
}

// endAt is end for an opponent show of exactly show.
func (s *searcher) endAt(show, myPeg, oppPeg int) float64 {
	k := -1
	if myPeg < maxPeg && oppPeg < maxPeg {
		k = (show*maxPeg+myPeg)*maxPeg + oppPeg
		if s.ends.stamp[k] == s.ends.gen {
			return s.ends.val[k]
		}
	}
	me, opp := s.base[0]+myPeg, s.base[1]+oppPeg
	var v float64
	if s.iDeal {
		v = s.showThenCrib(opp+show, me+s.myShow, false)
	} else {
		v = s.showThenCrib(me+s.myShow, opp+show, true)
	}
	if k >= 0 {
		s.ends.val[k], s.ends.stamp[k] = v, s.ends.gen
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
	minValue := bots.GoFloor(o) + 1
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
			s.worlds = append(s.worlds, world{ranks: ranks, n: int8(k), weight: weight})
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

// peek narrows the worlds to what the oracle may see of the opponent's
// hand. OracleHand keeps only the real hand. OracleSix keeps the hands
// that four of their six dealt cards make, each weighted by the number of
// ways to hold it and by the keep model's chance of keeping it from those
// six; reweight then skips the keep model, which assumes unknown cards.
func (s *searcher) peek(o *engine.Observation, oppPlayed []engine.Card) {
	var opp engine.Observation
	s.game.ObserveInto(1-s.seat, &opp)
	var held [14]int // ranks of what the oracle shows
	switch s.oracle {
	case OracleHand:
		for _, c := range opp.HandCards() {
			held[c.Rank()]++
		}
	case OracleSix:
		for _, c := range opp.Kept {
			held[c.Rank()]++
		}
		for _, c := range opp.Discards {
			held[c.Rank()]++
		}
		for _, c := range oppPlayed {
			held[c.Rank()]--
		}
	}
	oppSide := 1
	if o.Dealer != o.Player {
		oppSide = 0
	}
	var six [6]int8
	if s.oracle == OracleSix {
		n := 0
		for _, c := range opp.Kept {
			six[n] = int8(c.Rank())
			n++
		}
		for _, c := range opp.Discards {
			six[n] = int8(c.Rank())
			n++
		}
		sort.Slice(six[:], func(i, j int) bool { return six[i] < six[j] })
	}
	out := s.worlds[:0]
	for _, w := range s.worlds {
		var need [14]int
		for _, r := range w.ranks[:w.n] {
			need[r]++
		}
		ways := 1
		for r := 1; r <= 13; r++ {
			ways *= choose(held[r], need[r])
		}
		if ways == 0 || (s.oracle == OracleHand && w.n != int8(o.OppHandN)) {
			continue
		}
		if s.oracle == OracleHand {
			for r := 1; r <= 13; r++ {
				if need[r] != held[r] {
					ways = 0
				}
			}
			if ways == 0 {
				continue
			}
		}
		w.weight = float64(ways)
		if s.oracle == OracleSix && s.keep != nil {
			var k4 [4]int
			n := 0
			for _, c := range oppPlayed {
				k4[n] = c.Rank()
				n++
			}
			for _, r := range w.ranks[:w.n] {
				k4[n] = int(r)
				n++
			}
			sort.Ints(k4[:])
			w.weight *= s.keep.prob(oppSide, six, shapeIdx[((k4[0]*14+k4[1])*14+k4[2])*14+k4[3]])
		}
		if w.weight > 0 {
			out = append(out, w)
		}
	}
	s.worlds = out
}

// keepCacheKey identifies a hand from one player's side: the keep weight
// of an opponent shape depends only on the cards that player has seen
// before the play, which stay fixed through the hand.
type keepCacheKey struct {
	my      [6]engine.Card
	starter engine.Card
	oppSide int
}

// reweight multiplies each world's weight by the opponent model, dropping
// worlds the model rules out (unless it rules out all of them).
func (s *searcher) reweight(o *engine.Observation, oppPlayed []engine.Card, worlds []world) []world {
	if s.keep == nil && s.pegTemp <= 0 && s.pegPol == nil {
		return worlds
	}
	if s.pegPol != nil {
		s.prepPegPolicy(o)
	}
	useKeep := s.keep != nil && s.oracle == NoOracle // an oracle has weighted its worlds already
	key := keepCacheKey{starter: o.Starter, oppSide: 1}
	copy(key.my[:4], o.Kept[:])
	copy(key.my[4:], o.Discards[:])
	if o.Dealer != o.Player {
		key.oppSide = 0
	}
	if useKeep && (s.keepGen == 0 || key != s.keepKey) {
		s.keepKey = key
		if s.keepGen++; s.keepGen == 0 { // wrapped: clear the stamps
			clear(s.keepStamp[:])
			s.keepGen = 1
		}
	}
	s.weighted = s.weighted[:0]
	for _, w := range worlds {
		if useKeep {
			w.weight *= s.keepWeight(key, oppPlayed, w)
		}
		if s.pegPol != nil {
			w.weight *= s.pegPolicyLik(w)
		} else if s.pegTemp > 0 {
			w.weight *= s.pegLikelihood(o, w)
		}
		if w.weight > 0 {
			s.weighted = append(s.weighted, w)
		}
	}
	if len(s.weighted) == 0 {
		return worlds
	}
	return s.weighted
}

// keepWeight is the relative chance that the opponent, dealt six cards from
// those I have not seen, kept the four of this world (their played cards
// and its hidden ranks): the sum over the two cards they might have thrown.
func (s *searcher) keepWeight(key keepCacheKey, oppPlayed []engine.Card, w world) float64 {
	var k4 [4]int
	n := 0
	for _, c := range oppPlayed {
		k4[n] = c.Rank()
		n++
	}
	for _, r := range w.ranks[:w.n] {
		k4[n] = int(r)
		n++
	}
	sort.Ints(k4[:])
	shape := shapeIdx[((k4[0]*14+k4[1])*14+k4[2])*14+k4[3]]
	if s.keepStamp[shape] == s.keepGen {
		return s.keepVal[shape]
	}
	var avail [14]int
	for r := 1; r <= 13; r++ {
		avail[r] = 4
	}
	for _, c := range key.my {
		avail[c.Rank()]--
	}
	avail[key.starter.Rank()]--
	for _, r := range k4 {
		avail[r]--
	}
	var v float64
	probs := s.keep.byKept[key.oppSide][int(shape)*numPairs:][:numPairs]
	for r1 := 1; r1 <= 13; r1++ {
		for r2 := r1; r2 <= 13; r2++ {
			combos := avail[r1] * avail[r2]
			if r1 == r2 {
				combos = choose(avail[r1], 2)
			}
			if combos <= 0 {
				continue
			}
			v += float64(combos) * float64(probs[pairIdx[r1][r2]])
		}
	}
	s.keepVal[shape], s.keepStamp[shape] = v, s.keepGen
	return v
}

// pegLikelihood is the chance that the opponent, holding this world's
// ranks, played the cards they did this hand, if each play is chosen by a
// softmax on the points it scores at once.
func (s *searcher) pegLikelihood(o *engine.Observation, w world) float64 {
	me := o.Player
	played := o.Played[:o.PlayedN]
	// The opponent's hand before each of their plays: this world's ranks
	// plus everything they played from then on.
	var hand [8]int8
	nh := copy(hand[:], w.ranks[:w.n])
	for _, pc := range played {
		if pc.Player != me {
			hand[nh] = int8(pc.Card.Rank())
			nh++
		}
	}
	lik := 1.0
	var seq [8]int8
	ns, count := 0, 0
	for _, pc := range played {
		r := int8(pc.Card.Rank())
		v := pc.Card.Value()
		if count+v > 31 {
			count, ns = 0, 0
		}
		if pc.Player != me {
			var z, chosen float64
			var tried uint16
			for _, h := range hand[:nh] {
				if tried&(1<<h) != 0 || count+int(rankValue(h)) > 31 {
					continue
				}
				tried |= 1 << h
				seq[ns] = h
				e := math.Exp(float64(pegPoints(seq[:ns+1], int8(count)+rankValue(h))) / s.pegTemp)
				z += e
				if h == r {
					chosen = e
				}
			}
			lik *= chosen / z
			for i := range hand[:nh] { // this card is no longer held
				if hand[i] == r {
					hand[i] = hand[nh-1]
					nh--
					break
				}
			}
		}
		seq[ns] = r
		ns++
		count += v
		if count == 31 {
			count, ns = 0, 0
		}
	}
	return lik
}

// poolFactor is how many worlds per unit of effort are drawn by card
// counts before the opponent model weights them and the final sample is
// drawn by those weights.
const poolFactor = 4

// systematic draws n of worlds in proportion to weight, deterministically,
// into *out; a world drawn k times gets weight k.
func systematic(worlds []world, n int, out *[]world) []world {
	var total float64
	for _, w := range worlds {
		total += w.weight
	}
	*out = (*out)[:0]
	step := total / float64(n)
	next, cum := step/2, 0.0
	for _, w := range worlds {
		cum += w.weight
		picks := 0
		for next < cum {
			picks++
			next += step
		}
		if picks > 0 {
			w.weight = float64(picks)
			*out = append(*out, w)
		}
	}
	return *out
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

// merge6 returns the sorted ranks of a sorted four and r1 <= r2.
func merge6(k4 [4]int, r1, r2 int) [6]int8 {
	var six [6]int8
	i, n := 0, 0
	for _, r := range [2]int{r1, r2} {
		for i < 4 && k4[i] <= r {
			six[n] = int8(k4[i])
			n++
			i++
		}
		six[n] = int8(r)
		n++
	}
	for ; i < 4; i++ {
		six[n] = int8(k4[i])
		n++
	}
	return six
}

// hiddenSuits returns the chances that the suits of the opponent's hidden
// cards, drawn from those I have not seen, give their hand nobs, or a
// flush of four or of five (with the starter). Each hidden card's suit is
// taken as independent of the others' but for two of a rank, which cannot
// share a suit.
func hiddenSuits(known []engine.Card, hidden []int8, starter engine.Card, seen *[52]bool) (nobs, flush4, flush5 float32) {
	if len(hidden) == 0 {
		return 0, 0, 0
	}
	var unseenOf [14]int // unseen cards of each rank
	for c := engine.Card(0); c < 52; c++ {
		if !seen[c] {
			unseenOf[c.Rank()]++
		}
	}
	var heldOf [14]int
	for _, r := range hidden {
		heldOf[r]++
	}
	// Nobs: one of their hidden jacks is the jack of the starter's suit.
	hasNobs := false
	for _, c := range known {
		if c.Rank() == 11 && c.Suit() == starter.Suit() {
			hasNobs = true
		}
	}
	if j := engine.NewCard(11, starter.Suit()); !hasNobs && !seen[j] && heldOf[11] > 0 {
		nobs = float32(heldOf[11]) / float32(unseenOf[11])
	}
	// Flush: every card of the hand in one suit.
	for su := engine.Suit(0); su < 4; su++ {
		ok := true
		for _, c := range known {
			if c.Suit() != su {
				ok = false
			}
		}
		if !ok {
			continue
		}
		p := 1.0
		for r := 1; r <= 13 && p > 0; r++ {
			switch {
			case heldOf[r] == 0:
			case heldOf[r] > 1 || seen[engine.NewCard(r, su)]:
				p = 0
			default:
				p /= float64(unseenOf[r])
			}
		}
		if su == starter.Suit() {
			flush5 += float32(p)
		} else {
			flush4 += float32(p)
		}
	}
	return nobs, flush4, flush5
}

// newEnds forgets every cached end value.
func (s *searcher) newEnds() {
	s.ends.gen++
	if s.ends.gen == 0 { // wrapped: clear the stamps
		clear(s.ends.stamp)
		s.ends.gen = 1
	}
}

// cribByWorldFrom is the score from which the search gives each world its
// own crib. Below it a world's crib shifts all its ends alike, which
// barely changes a choice, so the cheaper common crib is used.
const cribByWorldFrom = 90

// worldCribs fills s.cribs with each world's crib distribution: my two
// discards, the two cards they threw and the starter. The pair they threw
// is weighted as keepWeight weighs it (by card counts and the keep model)
// given the four they kept. Fifteens, pairs and runs are exact; nobs from
// their cards is its chance; a crib flush, which needs five cards of a
// suit, is left out for their cards.
func (s *searcher) worldCribs(o *engine.Observation, oppPlayed []engine.Card, seen *[52]bool, worlds []world) {
	handEVTable()
	s.cribs = s.cribs[:0]
	key := keepCacheKey{starter: o.Starter, oppSide: 1}
	if o.Dealer != o.Player {
		key.oppSide = 0
	}
	sr := o.Starter.Rank()
	d1, d2 := o.Discards[0], o.Discards[1]
	myNobs := (d1.Rank() == 11 && d1.Suit() == o.Starter.Suit()) || (d2.Rank() == 11 && d2.Suit() == o.Starter.Suit())
	jStar := engine.NewCard(11, o.Starter.Suit())
	for i := range worlds {
		w := &worlds[i]
		var k4 [4]int
		n := 0
		for _, c := range oppPlayed {
			k4[n] = c.Rank()
			n++
		}
		for _, r := range w.ranks[:w.n] {
			k4[n] = int(r)
			n++
		}
		sort.Ints(k4[:])
		shape := shapeIdx[((k4[0]*14+k4[1])*14+k4[2])*14+k4[3]]
		// Cards they could have thrown: unseen by me, not in this world.
		var avail [14]int
		for c := engine.Card(0); c < 52; c++ {
			if !seen[c] {
				avail[c.Rank()]++
			}
		}
		for _, r := range w.ranks[:w.n] {
			avail[r]--
		}
		jStarFree := !seen[jStar] // the jack of the starter's suit may be theirs
		var probs []float32
		if s.keep != nil {
			probs = s.keep.byKept[key.oppSide][int(shape)*numPairs:][:numPairs]
		}
		var crib [maxShow]float64
		var total float64
		for r1 := 1; r1 <= 13; r1++ {
			for r2 := r1; r2 <= 13; r2++ {
				combos := avail[r1] * avail[r2]
				if r1 == r2 {
					combos = choose(avail[r1], 2)
				}
				if combos <= 0 {
					continue
				}
				wt := float64(combos)
				if probs != nil {
					wt *= float64(probs[pairIdx[r1][r2]])
				}
				if wt == 0 {
					continue
				}
				c4 := [4]int{d1.Rank(), d2.Rank(), r1, r2}
				sort.Ints(c4[:])
				pts := int(handEVByShape[shapeIdx[((c4[0]*14+c4[1])*14+c4[2])*14+c4[3]]][sr-1])
				pNobs := 0.0
				switch {
				case myNobs:
					pts++
				case jStarFree && avail[11] > 0:
					jacks := 0
					if r1 == 11 {
						jacks++
					}
					if r2 == 11 {
						jacks++
					}
					pNobs = float64(jacks) / float64(avail[11])
				}
				crib[min(pts, maxShow-1)] += wt * (1 - pNobs)
				crib[min(pts+1, maxShow-1)] += wt * pNobs
				total += wt
			}
		}
		if total == 0 {
			crib = s.crib // nothing fits: keep the common crib
		} else {
			for c := range crib {
				crib[c] /= total
			}
		}
		s.cribs = append(s.cribs, crib)
	}
}
