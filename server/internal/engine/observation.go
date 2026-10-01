package engine

// Observation is everything one player is allowed to see. It is a plain
// value with fixed-size arrays so it can be produced without allocation.
type Observation struct {
	Player  int
	Phase   Phase
	Dealer  int
	Turn    int // -1 outside pegging
	MustAct bool
	HandNum int
	Scores  [2]int
	Winner  int

	Hand  [6]Card // cards still held
	HandN int

	Discarded    bool    // this player has discarded this hand
	OppDiscarded bool    // opponent has discarded this hand
	Kept         [4]Card // this player's four kept cards (valid once Discarded)
	Discards     [2]Card // this player's contribution to the crib

	Starter  Card // NoCard until revealed
	Count    int
	Played   [8]PlayedCard // every card played this hand, in order
	PlayedN  int
	SeqStart int // Played[SeqStart:PlayedN] is the current count's sequence
	OppHandN int // cards the opponent still holds
}

// Observation returns player p's view of the game.
func (g *Game) Observation(p int) Observation {
	var o Observation
	g.ObserveInto(p, &o)
	return o
}

// ObserveInto fills o with player p's view of the game, avoiding a copy.
func (g *Game) ObserveInto(p int, o *Observation) {
	*o = Observation{
		Player:       p,
		Phase:        g.phase,
		Dealer:       g.dealer,
		Turn:         g.turn,
		MustAct:      g.ToAct(p),
		HandNum:      g.handNum,
		Scores:       g.scores,
		Winner:       g.winner,
		Hand:         g.hands[p],
		HandN:        g.handN[p],
		Discarded:    g.discarded[p],
		OppDiscarded: g.discarded[1-p],
		Starter:      g.starter,
		Count:        g.count,
		Played:       g.played,
		PlayedN:      g.playedN,
		SeqStart:     g.seqStart,
		OppHandN:     g.handN[1-p],
		Kept:         [4]Card{NoCard, NoCard, NoCard, NoCard},
		Discards:     [2]Card{NoCard, NoCard},
	}
	if g.discarded[p] {
		o.Kept = g.kept[p]
		o.Discards = g.discards[p]
	}
	for i := o.PlayedN; i < len(o.Played); i++ {
		o.Played[i] = PlayedCard{Card: NoCard, Player: -1}
	}
}

// HandCards returns the cards the observing player still holds.
func (o *Observation) HandCards() []Card { return o.Hand[:o.HandN] }

// Sequence returns the cards in the current count.
func (o *Observation) Sequence() []PlayedCard { return o.Played[o.SeqStart:o.PlayedN] }
