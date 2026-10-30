package bots

import (
	"math"
	"sync"

	"cribbage/internal/engine"
)

// Heuristic is a deterministic baseline player:
//   - Discard: keep the four cards maximizing expected hand value over all
//     possible starters, plus (dealer) or minus (pone) the expected value of
//     the two discards in the crib.
//   - Pegging: maximize points scored now minus the expected points the
//     opponent can score in reply with the cards still unseen.
type Heuristic struct{}

func NewHeuristic() *Heuristic { return &Heuristic{} }

func (h *Heuristic) Name() string { return "heuristic" }

func (h *Heuristic) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if len(legal) == 1 {
		return legal[0]
	}
	if o.Phase == engine.PhaseDiscard {
		return chooseDiscard(o, legal)
	}
	return choosePlay(o, legal)
}

func chooseDiscard(o *engine.Observation, legal []engine.Action) engine.Action {
	hand := o.HandCards()
	var seen [52]bool
	for _, c := range hand {
		seen[c] = true
	}
	dealer := o.Dealer == o.Player
	best, bestV := legal[0], math.Inf(-1)
	for _, a := range legal {
		if v := discardEV(keptAfter(hand, a), a, &seen, dealer); v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// discardEV is the expected show of kept plus (dealer) or minus (pone) the
// expected crib from the discards.
func discardEV(kept [4]engine.Card, a engine.Action, seen *[52]bool, dealer bool) float64 {
	v, crib := HandEV(kept, seen), CribEV(a.Cards[0], a.Cards[1])
	if dealer {
		return v + crib
	}
	return v - crib
}

func keptAfter(hand []engine.Card, a engine.Action) [4]engine.Card {
	var kept [4]engine.Card
	n := 0
	for _, c := range hand {
		if c != a.Cards[0] && c != a.Cards[1] {
			kept[n] = c
			n++
		}
	}
	return kept
}

// HandEV is the average show score of kept over every starter not in seen.
// Fifteens, pairs and runs depend only on the starter's rank, so they are
// computed once per rank; flush and nobs are added per starter.
func HandEV(kept [4]engine.Card, seen *[52]bool) float64 {
	var byRank [14]int
	for r := 1; r <= 13; r++ {
		b := engine.ScoreHand(kept, engine.NewCard(r, engine.Clubs), false)
		byRank[r] = b.Fifteens + b.Pairs + b.Runs
	}
	suit := kept[0].Suit()
	flush := kept[1].Suit() == suit && kept[2].Suit() == suit && kept[3].Suit() == suit
	var nobsSuits [4]bool
	for _, c := range kept {
		if c.Rank() == 11 {
			nobsSuits[c.Suit()] = true
		}
	}
	total, n := 0, 0
	for s := engine.Card(0); s < 52; s++ {
		if seen[s] {
			continue
		}
		total += byRank[s.Rank()]
		if flush {
			total += 4
			if s.Suit() == suit {
				total++
			}
		}
		if nobsSuits[s.Suit()] {
			total++
		}
		n++
	}
	return float64(total) / float64(n)
}

var (
	cribOnce  sync.Once
	cribTable [14][14]float64
)

// CribEV estimates the expected crib score contributed by discarding a and
// b, assuming the other two crib cards and the starter are random. Suits are
// ignored, so crib flushes are not counted.
func CribEV(a, b engine.Card) float64 {
	cribOnce.Do(buildCribTable)
	return cribTable[a.Rank()][b.Rank()]
}

// buildCribTable enumerates, for every pair of discard ranks, all opponent
// discard pairs and starters from the remaining 50 cards.
func buildCribTable() {
	var wg sync.WaitGroup
	for r1 := 1; r1 <= 13; r1++ {
		for r2 := r1; r2 <= 13; r2++ {
			wg.Add(1)
			go func(r1, r2 int) {
				defer wg.Done()
				a, b := engine.NewCard(r1, engine.Clubs), engine.NewCard(r2, engine.Diamonds)
				var rest []engine.Card
				for c := engine.Card(0); c < 52; c++ {
					if c != a && c != b {
						rest = append(rest, c)
					}
				}
				total, n := 0, 0
				for i := 0; i < len(rest); i++ {
					for j := i + 1; j < len(rest); j++ {
						crib := [4]engine.Card{a, b, rest[i], rest[j]}
						for _, s := range rest {
							if s == rest[i] || s == rest[j] {
								continue
							}
							total += engine.ScoreHand(crib, s, true).Total()
							n++
						}
					}
				}
				ev := float64(total) / float64(n)
				cribTable[r1][r2], cribTable[r2][r1] = ev, ev
			}(r1, r2)
		}
	}
	wg.Wait()
}

func choosePlay(o *engine.Observation, legal []engine.Action) engine.Action {
	var seq [16]engine.Card
	n := 0
	for _, pc := range o.Sequence() {
		seq[n] = pc.Card
		n++
	}
	var unseen [52]engine.Card
	nu := unseenCards(o, &unseen)
	// Each unseen card is in the opponent's hand with this probability.
	pHeld := 0.0
	if nu > 0 {
		pHeld = float64(o.OppHandN) / float64(nu)
	}

	best, bestV := legal[0], math.Inf(-1)
	for _, a := range legal {
		c := a.Cards[0]
		seq[n] = c
		gain := float64(engine.ScorePeg(seq[:n+1]).Total())
		count := o.Count + c.Value()
		risk := 0.0
		if count < 31 {
			for _, u := range unseen[:nu] {
				if count+u.Value() <= 31 {
					seq[n+1] = u
					risk += float64(engine.ScorePeg(seq[:n+2]).Total())
				}
			}
			risk *= pHeld
		}
		// Slight preference for shedding high cards early, keeping low cards
		// for go and 31 opportunities later.
		v := gain - risk + 0.01*float64(c.Value())
		if v > bestV {
			best, bestV = a, v
		}
	}
	return best
}

// unseenCards fills unseen with the cards the opponent might hold:
// everything the observing player has not seen. It returns their number.
func unseenCards(o *engine.Observation, unseen *[52]engine.Card) int {
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
	nu := 0
	for c := engine.Card(0); c < 52; c++ {
		if !seen[c] {
			unseen[nu] = c
			nu++
		}
	}
	return nu
}

// GoFloor returns a card value v such that every card the opponent still
// holds is known to be worth more than v.
//
// The opponent could not play when I played twice running within one
// count, and neither player could when the count was reset below 31. The
// cards they hold now were in their hand then.
func GoFloor(o *engine.Observation) int {
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
