package expert

import (
	"testing"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// pegGames plays random games and calls f before every pegging play with
// the game, the observation of the player to move, and the chosen action.
func pegGames(t *testing.T, games int, f func(g *engine.Game, o *engine.Observation, a engine.Action)) {
	t.Helper()
	botGames(t, bots.NewRandom(9), games, f)
}

// botGames is pegGames with both seats played by bot.
func botGames(t *testing.T, r bots.Bot, games int, f func(g *engine.Game, o *engine.Observation, a engine.Action)) {
	t.Helper()
	for seed := uint64(0); seed < uint64(games); seed++ {
		g := engine.NewGame(seed)
		g.SetQuiet(true)
		for !g.Over() {
			for p := 0; p < 2 && !g.Over(); p++ {
				if !g.ToAct(p) {
					continue
				}
				o := g.Observation(p)
				legal := g.Legal(p)
				a := r.Choose(&o, legal)
				if a.Kind == engine.ActPlay {
					f(g, &o, a)
				}
				if err := g.Apply(p, a); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// nodeFor builds the rank-level state of a game in which engine player p
// is the searcher (node player 0).
func nodeFor(g *engine.Game, o *engine.Observation) pegNode {
	var n pegNode
	for side, ep := range [2]int{o.Player, 1 - o.Player} {
		for _, c := range g.Hand(ep) {
			n.hand[side][n.n[side]] = int8(c.Rank())
			n.n[side]++
		}
	}
	for _, pc := range o.Sequence() {
		n.seq[n.seqN] = int8(pc.Card.Rank())
		n.seqN++
	}
	n.count = int8(o.Count)
	n.last = -1
	if o.PlayedN > 0 {
		n.last = 0
		if o.Played[o.PlayedN-1].Player != o.Player {
			n.last = 1
		}
	}
	return n
}

// The rank model scores, passes the turn and ends pegging exactly as the
// engine does.
func TestPegModelMatchesEngine(t *testing.T) {
	steps := 0
	pegGames(t, 300, func(g *engine.Game, o *engine.Observation, a engine.Action) {
		p := o.Player
		n := nodeFor(g, o)
		base := [2]int{o.Scores[p], o.Scores[1-p]}
		idx := 0
		for n.hand[0][idx] != int8(a.Cards[0].Rank()) {
			idx++
		}
		peg0 := g.Pegged()
		after := g.Clone()
		after.SetQuiet(true)
		if err := after.Apply(p, a); err != nil {
			t.Fatal(err)
		}
		res := n.apply(idx, base)
		peg := after.Pegged()
		if got, want := [2]int{int(n.pts[0]), int(n.pts[1])}, [2]int{peg[p] - peg0[p], peg[1-p] - peg0[1-p]}; got != want {
			t.Fatalf("pegging points %v, engine %v", got, want)
		}
		switch res {
		case pegWon:
			if !after.Over() || after.Winner() != map[int8]int{0: p, 1: 1 - p}[n.turn] {
				t.Fatalf("model says player %d won; engine over %v winner %d", n.turn, after.Over(), after.Winner())
			}
		case pegOver:
			if after.Phase() == engine.PhasePegging {
				t.Fatal("model ended pegging; engine did not")
			}
		case pegContinue:
			if after.Phase() != engine.PhasePegging {
				t.Fatalf("engine left pegging (phase %v); model did not", after.Phase())
			}
			want := int8(0)
			if after.Turn() != p {
				want = 1
			}
			if n.turn != want || int(n.count) != after.Count() {
				t.Fatalf("model turn %d count %d; engine turn %d count %d", n.turn, n.count, want, after.Count())
			}
		}
		steps++
	})
	if steps < 5000 {
		t.Fatalf("only %d pegging steps checked", steps)
	}
}

// Every card the opponent really holds is consistent with the gos the
// searcher infers, and so the real hand is among the enumerated worlds.
func TestGoFloorIsSound(t *testing.T) {
	s := newSearcher(&WinTable{}, 0)
	constrained := 0
	pegGames(t, 300, func(g *engine.Game, o *engine.Observation, _ engine.Action) {
		floor := bots.GoFloor(o)
		if floor > 0 {
			constrained++
		}
		for _, c := range g.Hand(1 - o.Player) {
			if c.Value() <= floor {
				t.Fatalf("floor %d excludes the opponent's %v; played %v", floor, c, o.Played[:o.PlayedN])
			}
		}
		var seen [52]bool
		for _, c := range o.Kept {
			seen[c] = true
		}
		for _, c := range o.Discards {
			seen[c] = true
		}
		seen[o.Starter] = true
		for _, pc := range o.Played[:o.PlayedN] {
			seen[pc.Card] = true
		}
		s.enumerate(o, &seen, nil)
		var real [14]int
		for _, c := range g.Hand(1 - o.Player) {
			real[c.Rank()]++
		}
		found := false
		for _, w := range s.worlds {
			var got [14]int
			for _, r := range w.ranks[:w.n] {
				got[r]++
			}
			if got == real {
				found = true
			}
		}
		if !found {
			t.Fatalf("the opponent's real hand %v is not among %d worlds", g.Hand(1-o.Player), len(s.worlds))
		}
	})
	if constrained < 100 {
		t.Fatalf("gos constrained only %d decisions", constrained)
	}
}

func TestPegPointsMatchesEngine(t *testing.T) {
	for _, seq := range [][]int{{5, 10}, {7, 8, 9}, {3, 3, 3}, {4, 2, 3}, {10, 11, 12, 13}, {6, 6}, {9, 8, 7, 6, 1}, {1, 2, 4, 3}} {
		var cards []engine.Card
		var ranks []int8
		count := 0
		for i, r := range seq {
			c := engine.NewCard(r, engine.Suit(i%4))
			cards = append(cards, c)
			ranks = append(ranks, int8(r))
			count += c.Value()
		}
		if got, want := pegPoints(ranks, int8(count)), engine.ScorePeg(cards).Total(); int(got) != want {
			t.Errorf("%v: %d, engine %d", seq, got, want)
		}
	}
}

func TestSampleIsSystematic(t *testing.T) {
	s := newSearcher(&WinTable{}, 2)
	// Total weight 8.5 in two draws: at 2.125 and 6.375, both in the
	// heavy second world.
	s.worlds = []world{{weight: 1}, {weight: 6, show: 7}, {weight: 1}, {weight: 0.5}}
	got := s.sample()
	if len(got) != 1 || got[0].show != 7 || got[0].weight != 2 {
		t.Fatalf("sample %+v, want the second world twice", got)
	}
}
