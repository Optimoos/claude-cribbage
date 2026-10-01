package engine

// Breakdown itemizes the score of a hand or crib during the show.
type Breakdown struct {
	Fifteens int
	Pairs    int
	Runs     int
	Flush    int
	Nobs     int
}

// Total returns the sum of all components.
func (b Breakdown) Total() int { return b.Fifteens + b.Pairs + b.Runs + b.Flush + b.Nobs }

// ScoreHand scores a four-card hand (or crib) together with the starter.
// In the crib, a flush only counts when the starter matches as well.
func ScoreHand(hand [4]Card, starter Card, isCrib bool) Breakdown {
	var b Breakdown
	cards := [5]Card{hand[0], hand[1], hand[2], hand[3], starter}

	// Fifteens: count subsets summing to 15 with a subset-sum DP.
	var ways [16]int
	ways[0] = 1
	var cnt [14]int
	for _, c := range cards {
		v := c.Value()
		for s := 15; s >= v; s-- {
			ways[s] += ways[s-v]
		}
		cnt[c.Rank()]++
	}
	b.Fifteens = 2 * ways[15]

	// Pairs: n cards of a rank form n*(n-1)/2 pairs worth 2 each.
	// Runs: each maximal stretch of 3+ consecutive ranks scores
	// length * (product of multiplicities).
	for r := 1; r <= 13; {
		n := cnt[r]
		if n == 0 {
			r++
			continue
		}
		start, mult := r, 1
		for r <= 13 && cnt[r] > 0 {
			b.Pairs += cnt[r] * (cnt[r] - 1)
			mult *= cnt[r]
			r++
		}
		if length := r - start; length >= 3 {
			b.Runs += length * mult
		}
	}

	s := hand[0].Suit()
	if hand[1].Suit() == s && hand[2].Suit() == s && hand[3].Suit() == s {
		if starter.Suit() == s {
			b.Flush = 5
		} else if !isCrib {
			b.Flush = 4
		}
	}

	for _, c := range hand {
		if c.Rank() == 11 && c.Suit() == starter.Suit() {
			b.Nobs = 1
			break
		}
	}
	return b
}

// PegScore itemizes points earned by a single play during pegging.
type PegScore struct {
	Fifteen   int
	ThirtyOne int
	Pairs     int
	Run       int
}

// Total returns the sum of all components.
func (p PegScore) Total() int { return p.Fifteen + p.ThirtyOne + p.Pairs + p.Run }

// ScorePeg scores the most recent card in seq, the cards played since the
// count was last reset (the last element is the card just played).
func ScorePeg(seq []Card) PegScore {
	var ps PegScore
	n := len(seq)
	if n == 0 {
		return ps
	}
	count := 0
	for _, c := range seq {
		count += c.Value()
	}
	switch count {
	case 15:
		ps.Fifteen = 2
	case 31:
		ps.ThirtyOne = 2
	}

	last := seq[n-1].Rank()
	k := 1
	for i := n - 2; i >= 0 && seq[i].Rank() == last; i-- {
		k++
	}
	ps.Pairs = k * (k - 1)

	// Longest run formed by the trailing L cards, in any order.
	for l := n; l >= 3; l-- {
		var mask uint16
		lo, hi := 14, 0
		dup := false
		for _, c := range seq[n-l:] {
			r := c.Rank()
			if mask&(1<<r) != 0 {
				dup = true
				break
			}
			mask |= 1 << r
			lo = min(lo, r)
			hi = max(hi, r)
		}
		if !dup && hi-lo == l-1 {
			ps.Run = l
			break
		}
	}
	return ps
}
