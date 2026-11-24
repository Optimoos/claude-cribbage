package expert

import "sync"

// Information-set pegging search (proposal 1). The minimax search values
// each world on its own, as if I would know the opponent's hand on every
// later play. This search keeps the worlds together: a node is the public
// play history and the worlds still consistent with it, and at my nodes I
// choose one card for all of them, the one with the best weighted value
// over the worlds. At the opponent's nodes each world's opponent plays the
// card worst for me in that world (they best-respond to my strategy), so
// only my side's knowledge is corrected; a policy for the opponent's side
// is proposal 2's.
//
// Each world keeps its own pegNode and plays through the same apply as the
// minimax search, so the rules (gos, 31, last card, winning) are shared.
// After a play the worlds are grouped by the public state that results: a
// world where the opponent cannot play goes on differently from one where
// they can.

// isGroup is a set of worlds sharing one public state: nodes[k] is the
// state in world idx[k].
type isGroup struct {
	nodes []pegNode
	idx   []int32
}

// publicKey is a node with the opponent's hidden cards left out.
func publicKey(n *pegNode) pegNode {
	k := *n
	k.hand[1] = [4]int8{}
	return k
}

// isBuf is stack-allocated scratch for the search: every slice taken
// after mark is given back by release, so a decision allocates nothing
// once the buffers have grown. When a buffer runs out a larger one is
// started; slices from the old one stay valid until released.
type isBuf struct {
	nodes []pegNode
	idx   []int32
	vals  []float64
	ints  []int
	tops  [4]int
}

type isMark struct {
	nodes []pegNode
	idx   []int32
	vals  []float64
	ints  []int
	tops  [4]int
}

func (b *isBuf) mark() isMark { return isMark{b.nodes, b.idx, b.vals, b.ints, b.tops} }
func (b *isBuf) release(m isMark) {
	b.nodes, b.idx, b.vals, b.ints, b.tops = m.nodes, m.idx, m.vals, m.ints, m.tops
}
func (b *isBuf) node(n int) []pegNode { return take(&b.nodes, &b.tops[0], n) }
func (b *isBuf) i32(n int) []int32    { return take(&b.idx, &b.tops[1], n) }
func (b *isBuf) f64(n int) []float64  { return take(&b.vals, &b.tops[2], n) }
func (b *isBuf) int(n int) []int      { return take(&b.ints, &b.tops[3], n) }

func take[T any](buf *[]T, top *int, n int) []T {
	if *top+n > len(*buf) {
		*buf = make([]T, max(2*len(*buf), 4*n, 1024))
		*top = 0
	}
	s := (*buf)[*top : *top+n : *top+n]
	*top += n
	return s
}

// isDepthFactor bounds the scratch a search takes, in slices of the
// number of worlds of each kind.
const isDepthFactor = 40

var isBufs = sync.Pool{New: func() any { return new(isBuf) }}

// isRoot values my playing hand[0][i] from root in every world, filling
// vals[k] for world k, by the information-set search.
func (s *searcher) isRoot(root pegNode, i int, worlds []world, worldCribs bool, vals []float64) {
	b := isBufs.Get().(*isBuf)
	defer isBufs.Put(b)
	// Each play deep takes at most a few slices of len(worlds) of each
	// kind, and a hand has at most eight plays: size for that up front,
	// since a buffer grown part way is given back at the next release.
	if need := isDepthFactor * len(worlds); len(b.nodes) < need {
		*b = isBuf{nodes: make([]pegNode, need), idx: make([]int32, need), vals: make([]float64, need), ints: make([]int, need)}
	}
	m := b.mark()
	defer b.release(m)
	s.isb = b
	nodes, idx := b.node(len(worlds)), b.i32(len(worlds))
	for k := range worlds {
		n := root
		n.hand[1], n.n[1] = worlds[k].ranks, worlds[k].n
		n.turn = 0
		nodes[k], idx[k] = n, int32(k)
	}
	s.isPlay(isGroup{nodes, idx}, i, nil, worlds, worldCribs, vals)
}

// isPlay has the player to move in every node of g play the card at
// position i of their hand (or at pos[k] in node k, when pos is not nil;
// it must be legal in each), then values the results into vals (by
// position in g).
func (s *searcher) isPlay(g isGroup, i int, pos []int, worlds []world, worldCribs bool, vals []float64) {
	b := s.isb
	m := b.mark()
	defer b.release(m)
	n := len(g.nodes)
	after := b.node(n)
	gid := b.int(n)
	var keys [8]pegNode
	var codes [8]int
	var sizes [8]int
	ng := 0
	for k := range g.nodes {
		c := g.nodes[k]
		at := i
		if pos != nil {
			at = pos[k]
		}
		code := c.apply(at, s.base)
		after[k] = c
		key := publicKey(&c)
		j := 0
		for j < ng && (codes[j] != code || keys[j] != key) {
			j++
		}
		if j == ng {
			if ng == len(keys) {
				panic("expert: more public outcomes of one play than expected")
			}
			keys[j], codes[j] = key, code
			ng++
		}
		gid[k] = j
		sizes[j]++
	}
	for j := 0; j < ng; j++ {
		switch codes[j] {
		case pegWon:
			v := 0.0
			if keys[j].turn == 0 {
				v = 1
			}
			for k := range gid {
				if gid[k] == j {
					vals[k] = v
				}
			}
		case pegOver:
			for k := range gid {
				if gid[k] != j {
					continue
				}
				wi := g.idx[k]
				s.world = &worlds[wi]
				if worldCribs {
					s.crib = s.cribs[wi]
					s.newEnds()
				}
				vals[k] = s.end(int(after[k].pts[0]), int(after[k].pts[1]))
			}
		default:
			if ng == 1 {
				s.isNode(isGroup{after, g.idx}, worlds, worldCribs, vals)
				continue
			}
			gm := b.mark()
			sub := isGroup{b.node(sizes[j]), b.i32(sizes[j])}
			sv := b.f64(sizes[j])
			at := 0
			for k := range gid {
				if gid[k] == j {
					sub.nodes[at], sub.idx[at] = after[k], g.idx[k]
					at++
				}
			}
			s.isNode(sub, worlds, worldCribs, sv)
			at = 0
			for k := range gid {
				if gid[k] == j {
					vals[k] = sv[at]
					at++
				}
			}
			b.release(gm)
		}
	}
}

// isNode values a group whose public state has a player to move.
func (s *searcher) isNode(g isGroup, worlds []world, worldCribs bool, vals []float64) {
	b := s.isb
	m := b.mark()
	defer b.release(m)
	n0 := &g.nodes[0]
	n := len(g.nodes)
	if n0.turn == 0 {
		// My hand is the same in every world: one card for all of them.
		tmp := b.f64(n)
		bestV := -1.0
		var tried uint16
		for i := 0; i < int(n0.n[0]); i++ {
			r := n0.hand[0][i]
			if tried&(1<<r) != 0 || n0.count+rankValue(r) > 31 {
				continue
			}
			tried |= 1 << r
			s.isPlay(g, i, nil, worlds, worldCribs, tmp)
			var v float64
			for k := range tmp {
				v += worlds[g.idx[k]].weight * tmp[k]
			}
			if v > bestV {
				bestV = v
				copy(vals, tmp)
			}
		}
		return
	}
	// The opponent: in each world, the card worst for me.
	for k := range vals {
		vals[k] = 2
	}
	sub := isGroup{b.node(n), b.i32(n)}
	at, pos := b.int(n), b.int(n)
	sv := b.f64(n)
	for r := int8(1); r <= 13; r++ {
		if n0.count+rankValue(r) > 31 {
			continue
		}
		m := 0
		for k := range g.nodes {
			nd := &g.nodes[k]
			for i := 0; i < int(nd.n[1]); i++ {
				if nd.hand[1][i] == r {
					sub.nodes[m], sub.idx[m], at[m], pos[m] = *nd, g.idx[k], k, i
					m++
					break
				}
			}
		}
		if m == 0 {
			continue
		}
		s.isPlay(isGroup{sub.nodes[:m], sub.idx[:m]}, 0, pos[:m], worlds, worldCribs, sv[:m])
		for j := 0; j < m; j++ {
			vals[at[j]] = min(vals[at[j]], sv[j])
		}
	}
}
