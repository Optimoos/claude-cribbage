package engine

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
)

// pegGame builds a game in the pegging phase with the given four-card hands.
// Player 1 - dealer leads.
func pegGame(dealer int, hand0, hand1, starter string) *Game {
	g := &Game{winner: -1, last: -1, dealer: dealer, phase: PhasePegging, turn: 1 - dealer, handNum: 1}
	for p, s := range []string{hand0, hand1} {
		h := hand4(s)
		copy(g.hands[p][:], h[:])
		g.hands[p][4], g.hands[p][5] = NoCard, NoCard
		g.handN[p] = 4
		g.kept[p] = h
		g.discarded[p] = true
	}
	g.crib = hand4("2D 3D 9D QC")
	g.cribN = 4
	g.starter = card(starter)
	return g
}

func mustApply(t *testing.T, g *Game, p int, a Action) []Event {
	t.Helper()
	if err := g.Apply(p, a); err != nil {
		t.Fatalf("P%d %v: %v", p, a, err)
	}
	return append([]Event(nil), g.Events()...)
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestPeggingGoAndLastCard(t *testing.T) {
	g := pegGame(0, "QH 5H 9H 8H", "KS JS TS 2S", "3C")
	steps := []struct {
		p      int
		c      string
		scores [2]int
		next   int
	}{
		{1, "KS", [2]int{0, 0}, 0},
		{0, "QH", [2]int{0, 0}, 1},
		{1, "JS", [2]int{0, 4}, 0}, // run K-Q-J for 3, then go point (nobody can play)
		{0, "5H", [2]int{0, 4}, 1},
		{1, "TS", [2]int{0, 6}, 0}, // fifteen
		{0, "9H", [2]int{0, 6}, 1},
		{1, "2S", [2]int{0, 7}, 0}, // P0 cannot play 8H on 26, P1 is out: go point
	}
	for _, s := range steps {
		mustApply(t, g, s.p, Play(card(s.c)))
		if g.Scores() != s.scores || g.Turn() != s.next {
			t.Fatalf("after P%d %s: scores %v turn %d, want %v turn %d", s.p, s.c, g.Scores(), g.Turn(), s.scores, s.next)
		}
	}
	evs := mustApply(t, g, 0, Play(card("8H")))
	want := []EventKind{EvPlay, EvLastCard, EvShowHand, EvShowHand, EvShowCrib, EvDeal}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("events %v, want %v", kinds(evs), want)
	}
	if evs[1].Player != 0 || evs[1].Points != 1 {
		t.Errorf("last card event %+v", evs[1])
	}
	if g.Phase() != PhaseDiscard || g.Dealer() != 1 || g.HandNumber() != 2 {
		t.Errorf("expected new hand dealt by P1, got phase %v dealer %d hand %d", g.Phase(), g.Dealer(), g.HandNumber())
	}
}

func TestPeggingGoEvents(t *testing.T) {
	g := pegGame(0, "KH 5H 6H 7H", "KS QS AS 2S", "3C")
	mustApply(t, g, 1, Play(card("KS")))
	evs := mustApply(t, g, 0, Play(card("KH")))
	if evs[0].Peg.Pairs != 2 {
		t.Fatalf("expected pair, got %+v", evs[0])
	}
	// P1 plays Q for 30; P0 holds 5,6,7 and must say go; P1 continues.
	evs = mustApply(t, g, 1, Play(card("QS")))
	if !reflect.DeepEqual(kinds(evs), []EventKind{EvPlay, EvGo}) || evs[1].Player != 0 {
		t.Fatalf("events %v", evs)
	}
	if g.Turn() != 1 {
		t.Fatalf("P1 should continue after go, turn=%d", g.Turn())
	}
	if err := g.Apply(1, Play(card("2S"))); !errors.Is(err, ErrOver31) {
		t.Fatalf("expected ErrOver31, got %v", err)
	}
	// Ace makes 31: 2 for the 31 and no separate go point.
	evs = mustApply(t, g, 1, Play(card("AS")))
	if !reflect.DeepEqual(kinds(evs), []EventKind{EvPlay, EvCountReset}) || evs[0].Peg.ThirtyOne != 2 {
		t.Fatalf("events %v", evs)
	}
	if g.Scores() != [2]int{2, 2} || g.Turn() != 0 || g.Count() != 0 {
		t.Fatalf("scores %v turn %d count %d", g.Scores(), g.Turn(), g.Count())
	}
}

func TestIllegalActions(t *testing.T) {
	g := pegGame(0, "KH 5H 6H 7H", "KS QS AS 2S", "3C")
	cases := []struct {
		p   int
		a   Action
		err error
	}{
		{0, Play(card("KH")), ErrNotYourTurn},
		{1, Play(card("KH")), ErrCardNotInHand},
		{1, Discard(card("KS"), card("QS")), ErrWrongPhase},
		{2, Play(card("KS")), ErrBadPlayer},
	}
	for _, c := range cases {
		if err := g.Apply(c.p, c.a); !errors.Is(err, c.err) {
			t.Errorf("P%d %v: got %v, want %v", c.p, c.a, err, c.err)
		}
	}

	g = NewGame(7)
	h := g.Hand(0)
	if err := g.Apply(0, Discard(h[0], h[0])); !errors.Is(err, ErrCardNotInHand) {
		t.Errorf("duplicate discard: %v", err)
	}
	if err := g.Apply(0, Play(h[0])); !errors.Is(err, ErrWrongPhase) {
		t.Errorf("play during discard: %v", err)
	}
	mustApply(t, g, 0, Discard(h[0], h[1]))
	if err := g.Apply(0, Discard(h[0], h[1])); !errors.Is(err, ErrAlreadyActed) {
		t.Errorf("second discard: %v", err)
	}
}

func TestHisHeels(t *testing.T) {
	g := NewGame(3)
	g.pending = card("JD")
	d := g.Dealer()
	for p := 0; p < 2; p++ {
		h := g.Hand(p)
		if h[0] == g.pending || h[1] == g.pending {
			t.Skip("seed dealt the jack")
		}
		mustApply(t, g, p, Discard(h[0], h[1]))
	}
	if g.Scores()[d] != 2 || g.Starter() != card("JD") {
		t.Fatalf("dealer should score 2 for his heels, scores %v", g.Scores())
	}
}

func TestWinDuringPegging(t *testing.T) {
	g := pegGame(0, "KH 5H 6H 7H", "TS QS AS 2S", "3C")
	g.scores = [2]int{119, 100}
	mustApply(t, g, 1, Play(card("TS")))
	evs := mustApply(t, g, 0, Play(card("5H")))
	if !g.Over() || g.Winner() != 0 || g.Scores()[0] != WinScore {
		t.Fatalf("expected P0 to win, over=%v winner=%d scores=%v", g.Over(), g.Winner(), g.Scores())
	}
	if last := evs[len(evs)-1]; last.Kind != EvGameOver || last.Skunk != 0 {
		t.Fatalf("last event %v", last)
	}
	if err := g.Apply(1, Play(card("QS"))); !errors.Is(err, ErrGameOver) {
		t.Fatalf("expected ErrGameOver, got %v", err)
	}
	if len(g.Legal(0))+len(g.Legal(1)) != 0 {
		t.Fatal("no legal actions after game over")
	}
}

func TestNonDealerCountsFirst(t *testing.T) {
	g := pegGame(0, "5H 5C 5S JD", "AC 2D 3H 4S", "5D") // dealer P0 holds 29
	g.scores = [2]int{115, 115}
	g.handN = [2]int{0, 0}
	g.show()
	if g.Winner() != 1 || g.Scores() != [2]int{115, WinScore} {
		t.Fatalf("non-dealer should win first: winner %d scores %v", g.Winner(), g.Scores())
	}
}

func TestSkunk(t *testing.T) {
	g := pegGame(0, "KH 5H 6H 7H", "TS QS AS 2S", "3C")
	g.scores = [2]int{119, 60}
	mustApply(t, g, 1, Play(card("TS")))
	evs := mustApply(t, g, 0, Play(card("5H")))
	if last := evs[len(evs)-1]; last.Skunk != 2 {
		t.Fatalf("expected double skunk, got %+v", last)
	}
}

func TestObservationHidesOpponent(t *testing.T) {
	g := NewGame(11)
	o := g.Observation(0)
	if o.HandN != 6 || o.OppHandN != 6 || o.Starter != NoCard || !o.MustAct {
		t.Fatalf("bad initial observation %+v", o)
	}
	opp := map[Card]bool{}
	for _, c := range g.Hand(1) {
		opp[c] = true
	}
	for _, c := range o.HandCards() {
		if opp[c] {
			t.Fatal("observation leaked opponent card")
		}
	}
	h := g.Hand(1)
	mustApply(t, g, 1, Discard(h[0], h[1]))
	o = g.Observation(0)
	if !o.OppDiscarded || o.Discarded || o.Discards != [2]Card{NoCard, NoCard} {
		t.Fatalf("discard flags wrong %+v", o)
	}
}

// randomGame plays a full game with uniformly random legal moves and checks
// invariants after every action.
func randomGame(t *testing.T, seed uint64) *Game {
	r := rand.New(rand.NewPCG(seed, 99))
	g := NewGame(seed)
	var buf []Action
	gameOvers := 0
	for steps := 0; !g.Over(); steps++ {
		if steps > 10000 {
			t.Fatalf("seed %d: game did not end", seed)
		}
		p := r.IntN(2)
		if !g.ToAct(p) {
			p = 1 - p
		}
		buf = g.AppendLegal(p, buf[:0])
		if len(buf) == 0 {
			t.Fatalf("seed %d: P%d to act with no legal moves", seed, p)
		}
		before := g.Scores()
		a := buf[r.IntN(len(buf))]
		if err := g.Apply(p, a); err != nil {
			t.Fatalf("seed %d: legal action %v rejected: %v", seed, a, err)
		}
		var gained [2]int
		for _, e := range g.Events() {
			switch e.Kind {
			case EvStarter, EvPlay, EvGoPoint, EvLastCard, EvShowHand, EvShowCrib:
				gained[e.Player] += e.Points
			case EvGameOver:
				gameOvers++
			}
		}
		after := g.Scores()
		for q := 0; q < 2; q++ {
			if after[q] < before[q] || after[q] > WinScore {
				t.Fatalf("seed %d: score went %v -> %v", seed, before, after)
			}
			d := after[q] - before[q]
			if d != gained[q] && !(g.Over() && after[q] == WinScore && d < gained[q]) {
				t.Fatalf("seed %d: P%d gained %d but events say %d", seed, q, d, gained[q])
			}
		}
		checkInvariants(t, g, seed)
	}
	if gameOvers != 1 || g.Winner() < 0 || g.Scores()[g.Winner()] != WinScore {
		t.Fatalf("seed %d: gameOvers=%d winner=%d scores=%v", seed, gameOvers, g.Winner(), g.Scores())
	}
	return g
}

func checkInvariants(t *testing.T, g *Game, seed uint64) {
	t.Helper()
	var seen [52]bool
	mark := func(c Card) {
		if !c.Valid() || seen[c] {
			t.Fatalf("seed %d: duplicate or invalid card %v", seed, c)
		}
		seen[c] = true
	}
	for p := 0; p < 2; p++ {
		for _, c := range g.Hand(p) {
			mark(c)
		}
	}
	for _, c := range g.Crib() {
		mark(c)
	}
	for _, pc := range g.played[:g.playedN] {
		mark(pc.Card)
	}
	if g.starter.Valid() {
		mark(g.starter)
	} else {
		mark(g.pending)
	}
	if g.Over() {
		return
	}
	sum := 0
	for _, pc := range g.played[g.seqStart:g.playedN] {
		sum += pc.Card.Value()
	}
	if sum != g.count || g.count > 31 {
		t.Fatalf("seed %d: count %d, sequence sums to %d", seed, g.count, sum)
	}
	if g.phase == PhasePegging {
		if g.playedN+g.handN[0]+g.handN[1] != 8 || len(g.Legal(g.turn)) == 0 {
			t.Fatalf("seed %d: bad pegging state", seed)
		}
	}
}

func TestRandomGames(t *testing.T) {
	n := 100000
	if testing.Short() {
		n = 2000
	}
	wins := [2]int{}
	for i := 0; i < n; i++ {
		g := randomGame(t, uint64(i))
		wins[g.Winner()]++
	}
	t.Logf("%d games, wins %v", n, wins)
}

func TestDeterministicReplay(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 5))
		g := NewGame(seed)
		type move struct {
			p int
			a Action
		}
		var log []move
		for !g.Over() {
			p := 0
			if !g.ToAct(0) {
				p = 1
			}
			legal := g.Legal(p)
			a := legal[r.IntN(len(legal))]
			log = append(log, move{p, a})
			mustApply(t, g, p, a)
		}
		h := NewGame(seed)
		for _, m := range log {
			mustApply(t, h, m.p, m.a)
		}
		if !reflect.DeepEqual(g.Clone(), h.Clone()) {
			t.Fatalf("seed %d: replay diverged", seed)
		}
	}
}

func TestCloneIsIndependent(t *testing.T) {
	g := NewGame(1)
	c := g.Clone()
	h := c.Hand(0)
	mustApply(t, c, 0, Discard(h[0], h[1]))
	if !g.ToAct(0) || len(g.Hand(0)) != 6 {
		t.Fatal("mutating clone changed original")
	}
}

func BenchmarkRandomGame(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	buf := make([]Action, 0, 16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		g := NewGame(uint64(i))
		g.SetQuiet(true)
		for !g.Over() {
			p := 0
			if !g.ToAct(0) {
				p = 1
			}
			buf = g.AppendLegal(p, buf[:0])
			_ = g.Apply(p, buf[r.IntN(len(buf))])
		}
	}
}

func BenchmarkScoreHand(b *testing.B) {
	h := hand4("5H 5C 5S JD")
	s := card("5D")
	for i := 0; i < b.N; i++ {
		_ = ScoreHand(h, s, false)
	}
}

func TestNewHand(t *testing.T) {
	hands := [2][6]Card{
		{NewCard(5, Clubs), NewCard(5, 1), NewCard(10, 2), NewCard(11, Clubs), NewCard(1, 3), NewCard(2, 3)},
		{NewCard(3, 0), NewCard(4, 0), NewCard(6, 1), NewCard(7, 2), NewCard(8, 3), NewCard(9, 0)},
	}
	starter := NewCard(13, Clubs)
	g := NewHand(1, 1, [2]int{100, 110}, hands, starter)
	if g == nil || g.Phase() != PhaseDiscard || g.Dealer() != 1 || g.Scores() != [2]int{100, 110} {
		t.Fatalf("bad hand %+v", g)
	}
	for p := 0; p < 2; p++ {
		h := g.Hand(p)
		if err := g.Apply(p, Discard(h[0], h[1])); err != nil {
			t.Fatal(err)
		}
	}
	if g.Starter() != starter {
		t.Errorf("starter %v, want %v", g.Starter(), starter)
	}
	if NewHand(1, 0, [2]int{}, hands, hands[0][0]) != nil {
		t.Error("a repeated card was accepted")
	}
}

// The game ends the moment a player reaches 121, whatever scored the point.

func TestWinOnHeels(t *testing.T) {
	g := NewGame(3)
	g.pending = card("JD")
	d := g.Dealer()
	g.scores[d] = WinScore - 2
	for p := 0; p < 2; p++ {
		h := g.Hand(p)
		if h[0] == g.pending || h[1] == g.pending {
			t.Skip("seed dealt the jack")
		}
		mustApply(t, g, p, Discard(h[0], h[1]))
	}
	if !g.Over() || g.Winner() != d || len(g.Legal(0))+len(g.Legal(1)) != 0 {
		t.Fatalf("dealer should win on his heels: over=%v winner=%d scores=%v", g.Over(), g.Winner(), g.Scores())
	}
}

func TestWinOnGoPoint(t *testing.T) {
	g := pegGame(0, "QH 5H 9H 8H", "KS JS TS 2S", "3C")
	g.scores = [2]int{0, WinScore - 4} // the run K-Q-J makes 120, the go point 121
	mustApply(t, g, 1, Play(card("KS")))
	mustApply(t, g, 0, Play(card("QH")))
	evs := mustApply(t, g, 1, Play(card("JS")))
	if want := []EventKind{EvPlay, EvGo, EvGoPoint, EvGameOver}; !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("events %v, want %v", kinds(evs), want)
	}
	if g.Winner() != 1 || g.Scores()[1] != WinScore {
		t.Fatalf("winner %d scores %v", g.Winner(), g.Scores())
	}
}

func TestWinOnLastCard(t *testing.T) {
	g := pegGame(0, "QH 5H 9H 8H", "KS JS TS 2S", "3C")
	g.scores = [2]int{WinScore - 1, 0}
	for _, s := range []struct {
		p int
		c string
	}{{1, "KS"}, {0, "QH"}, {1, "JS"}, {0, "5H"}, {1, "TS"}, {0, "9H"}, {1, "2S"}} {
		mustApply(t, g, s.p, Play(card(s.c)))
	}
	evs := mustApply(t, g, 0, Play(card("8H")))
	if want := []EventKind{EvPlay, EvLastCard, EvGameOver}; !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("events %v, want %v: no hand is shown after the winning point", kinds(evs), want)
	}
	if g.Winner() != 0 {
		t.Fatalf("winner %d scores %v", g.Winner(), g.Scores())
	}
}

func TestWinAtShow(t *testing.T) {
	// Dealer P0 holds 29; the crib (2D 3D 9D QC with the 5D) is worth 4.
	show := func(dealerScore int) *Game {
		g := pegGame(0, "5H 5C 5S JD", "AC 2D 3H 4S", "5D")
		g.scores = [2]int{dealerScore, 0}
		g.handN = [2]int{0, 0}
		g.show()
		return g
	}
	g := show(WinScore - 29)
	if k := kinds(g.Events()); g.Winner() != 0 || k[len(k)-2] != EvShowHand {
		t.Fatalf("dealer's hand should win before the crib is counted: winner %d events %v", g.Winner(), k)
	}
	g = show(WinScore - 29 - 4)
	if k := kinds(g.Events()); g.Winner() != 0 || k[len(k)-2] != EvShowCrib {
		t.Fatalf("the crib should win: winner %d scores %v events %v", g.Winner(), g.Scores(), k)
	}
	if g = show(WinScore - 29 - 5); g.Over() {
		t.Fatalf("one short should play on: scores %v", g.Scores())
	}
}

func TestDiscardCardNotInHand(t *testing.T) {
	g := NewGame(1)
	h := g.Hand(0)
	other := g.Hand(1)[0]
	if err := g.Apply(0, Discard(h[0], other)); err == nil {
		t.Fatal("discarding a card from the other hand was accepted")
	}
}
