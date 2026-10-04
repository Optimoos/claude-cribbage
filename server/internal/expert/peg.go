package expert

import "cribbage/internal/engine"

// Pegging depends only on ranks, so the search works on ranks (1-13).

func rankValue(r int8) int8 { return min(r, 10) }

// pegPoints mirrors engine.ScorePeg for the last card of seq, the ranks
// played since the count was reset, which bring the count to count.
func pegPoints(seq []int8, count int8) int16 {
	n := len(seq)
	pts := int16(0)
	if count == 15 || count == 31 {
		pts = 2
	}
	last := seq[n-1]
	k := 1
	for i := n - 2; i >= 0 && seq[i] == last; i-- {
		k++
	}
	pts += int16(k * (k - 1))
	for l := n; l >= 3; l-- {
		var mask uint16
		lo, hi := int8(14), int8(0)
		dup := false
		for _, r := range seq[n-l:] {
			if mask&(1<<r) != 0 {
				dup = true
				break
			}
			mask |= 1 << r
			lo, hi = min(lo, r), max(hi, r)
		}
		if !dup && int(hi-lo) == l-1 {
			pts += int16(l)
			break
		}
	}
	return pts
}

// pegNode is the pegging state of one world, from the searcher's point of
// view: player 0 is the searcher, 1 the opponent.
type pegNode struct {
	hand  [2][4]int8 // ranks held; only the first n[p] are valid
	n     [2]int8
	seq   [8]int8 // ranks played since the count was reset
	seqN  int8
	count int8
	last  int8     // last player to play a card, -1 if none
	turn  int8     // player to play
	pts   [2]int16 // pegging points scored since the root
}

func (n *pegNode) canPlay(p int8) bool {
	for _, r := range n.hand[p][:n.n[p]] {
		if n.count+rankValue(r) <= 31 {
			return true
		}
	}
	return false
}

func (n *pegNode) reset() {
	n.count = 0
	n.seqN = 0
}

// Outcome of applying a play.
const (
	pegContinue = iota // n.turn is the next player to play
	pegOver            // both hands are empty: the show comes next
	pegWon             // a player reached 121; n.turn is the winner
)

// apply plays n.hand[n.turn][i] and advances to the next player able to
// play, scoring gos and the last card exactly as the engine does. base is
// each player's score at the root, for detecting a win.
func (n *pegNode) apply(i int, base [2]int) int {
	p := n.turn
	r := n.hand[p][i]
	n.n[p]--
	n.hand[p][i] = n.hand[p][n.n[p]]
	n.seq[n.seqN] = r
	n.seqN++
	n.count += rankValue(r)
	n.last = p
	if pts := pegPoints(n.seq[:n.seqN], n.count); pts > 0 {
		if n.score(p, pts, base) {
			return pegWon
		}
	}
	if n.count == 31 {
		n.reset()
	}
	next := 1 - p
	for {
		if n.n[0] == 0 && n.n[1] == 0 {
			if n.count > 0 && n.score(n.last, 1, base) {
				return pegWon
			}
			return pegOver
		}
		if n.canPlay(next) {
			n.turn = next
			return pegContinue
		}
		if n.canPlay(1 - next) {
			n.turn = 1 - next
			return pegContinue
		}
		if n.score(n.last, 1, base) {
			return pegWon
		}
		n.reset()
		next = 1 - n.last
	}
}

// score awards pts to p and reports whether p has won, leaving n.turn as
// the winner if so.
func (n *pegNode) score(p int8, pts int16, base [2]int) bool {
	n.pts[p] += pts
	if base[p]+int(n.pts[p]) >= engine.WinScore {
		n.turn = p
		return true
	}
	return false
}
