package engine

import (
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
)

// WinScore is the number of points needed to win.
const WinScore = 121

// Phase of the game. Counting the show and dealing the next hand happen
// automatically, so only phases that wait for player input are represented.
type Phase uint8

const (
	PhaseDiscard Phase = iota // both players choose two cards for the crib
	PhasePegging              // players alternate playing cards
	PhaseGameOver
)

func (p Phase) String() string {
	switch p {
	case PhaseDiscard:
		return "discard"
	case PhasePegging:
		return "pegging"
	case PhaseGameOver:
		return "game-over"
	}
	return fmt.Sprintf("phase(%d)", p)
}

// ActionKind identifies an Action.
type ActionKind uint8

const (
	ActDiscard ActionKind = iota + 1
	ActPlay
)

// Action is a player decision. Discard uses both Cards; Play uses Cards[0].
type Action struct {
	Kind  ActionKind
	Cards [2]Card
}

// Discard returns an action putting a and b into the crib.
func Discard(a, b Card) Action { return Action{Kind: ActDiscard, Cards: [2]Card{a, b}} }

// Play returns an action playing c during pegging.
func Play(c Card) Action { return Action{Kind: ActPlay, Cards: [2]Card{c, NoCard}} }

func (a Action) String() string {
	switch a.Kind {
	case ActDiscard:
		return fmt.Sprintf("discard %v %v", a.Cards[0], a.Cards[1])
	case ActPlay:
		return fmt.Sprintf("play %v", a.Cards[0])
	}
	return "invalid"
}

var (
	ErrGameOver      = errors.New("game is over")
	ErrBadPlayer     = errors.New("invalid player")
	ErrWrongPhase    = errors.New("action not allowed in this phase")
	ErrAlreadyActed  = errors.New("already discarded this hand")
	ErrNotYourTurn   = errors.New("not your turn")
	ErrCardNotInHand = errors.New("card not in hand")
	ErrOver31        = errors.New("card would exceed 31")
)

// PlayedCard is a card played during pegging and who played it.
type PlayedCard struct {
	Card   Card
	Player int
}

// Game is the full (perfect-information) state of a game. It contains no
// pointers apart from the event buffer, so Clone is a cheap value copy.
type Game struct {
	rng       pcg
	phase     Phase
	dealer    int
	turn      int // pegging only; -1 otherwise
	scores    [2]int
	winner    int
	handNum   int
	hands     [2][6]Card // cards still held
	handN     [2]int
	kept      [2][4]Card // the four cards kept, for the show
	discards  [2][2]Card
	discarded [2]bool
	crib      [4]Card
	cribN     int
	starter   Card // NoCard until revealed
	pending   Card // next starter, hidden until both have discarded
	count     int
	played    [8]PlayedCard
	playedN   int
	seqStart  int // index in played where the current count began
	last      int // last player to play a card in the current count
	goSaid    [2]bool

	quiet  bool
	events []Event
}

// NewGame starts a game from a seed. The same seed and the same sequence of
// actions always produce the same game.
func NewGame(seed uint64) *Game {
	g := &Game{rng: newPCG(seed), winner: -1, turn: -1, starter: NoCard}
	g.cutForDeal()
	g.deal()
	return g
}

// Clone returns an independent copy of the game. Events are not copied.
func (g *Game) Clone() *Game {
	c := *g
	c.events = nil
	return &c
}

// SetQuiet disables event recording, which speeds up simulations.
func (g *Game) SetQuiet(q bool) { g.quiet = q }

// Events returns the events produced by NewGame or the most recent Apply.
// The slice is reused by the next Apply.
func (g *Game) Events() []Event { return g.events }

func (g *Game) Phase() Phase       { return g.phase }
func (g *Game) Dealer() int        { return g.dealer }
func (g *Game) Turn() int          { return g.turn }
func (g *Game) Scores() [2]int     { return g.scores }
func (g *Game) Winner() int        { return g.winner }
func (g *Game) HandNumber() int    { return g.handNum }
func (g *Game) Count() int         { return g.count }
func (g *Game) Starter() Card      { return g.starter }
func (g *Game) Hand(p int) []Card  { return g.hands[p][:g.handN[p]] }
func (g *Game) Crib() []Card       { return g.crib[:g.cribN] }
func (g *Game) Over() bool         { return g.phase == PhaseGameOver }
func (g *Game) Kept(p int) [4]Card { return g.kept[p] }

// ToAct reports whether player p is expected to act now.
func (g *Game) ToAct(p int) bool {
	switch g.phase {
	case PhaseDiscard:
		return !g.discarded[p]
	case PhasePegging:
		return g.turn == p
	}
	return false
}

// Legal returns the legal actions for player p.
func (g *Game) Legal(p int) []Action { return g.AppendLegal(p, nil) }

// AppendLegal appends player p's legal actions to buf.
func (g *Game) AppendLegal(p int, buf []Action) []Action {
	if !g.ToAct(p) {
		return buf
	}
	h := g.hands[p][:g.handN[p]]
	switch g.phase {
	case PhaseDiscard:
		for i := 0; i < len(h); i++ {
			for j := i + 1; j < len(h); j++ {
				buf = append(buf, Discard(h[i], h[j]))
			}
		}
	case PhasePegging:
		for _, c := range h {
			if g.count+c.Value() <= 31 {
				buf = append(buf, Play(c))
			}
		}
	}
	return buf
}

// Apply performs action a for player p. On error the game is unchanged.
func (g *Game) Apply(p int, a Action) error {
	g.events = g.events[:0]
	if p != 0 && p != 1 {
		return ErrBadPlayer
	}
	switch g.phase {
	case PhaseGameOver:
		return ErrGameOver
	case PhaseDiscard:
		if a.Kind != ActDiscard {
			return ErrWrongPhase
		}
		if g.discarded[p] {
			return ErrAlreadyActed
		}
		i, j := g.find(p, a.Cards[0]), g.find(p, a.Cards[1])
		if i < 0 || j < 0 || i == j {
			return ErrCardNotInHand
		}
		g.discard(p, i, j)
	case PhasePegging:
		if a.Kind != ActPlay {
			return ErrWrongPhase
		}
		if p != g.turn {
			return ErrNotYourTurn
		}
		i := g.find(p, a.Cards[0])
		if i < 0 {
			return ErrCardNotInHand
		}
		if g.count+a.Cards[0].Value() > 31 {
			return ErrOver31
		}
		g.play(p, i)
	}
	return nil
}

func (g *Game) find(p int, c Card) int {
	for i := 0; i < g.handN[p]; i++ {
		if g.hands[p][i] == c {
			return i
		}
	}
	return -1
}

func (g *Game) emit(e Event) {
	if g.quiet {
		return
	}
	e.Scores = g.scores
	g.events = append(g.events, e)
}

// addScore awards points and reports whether p has reached WinScore.
func (g *Game) addScore(p, pts int) bool {
	g.scores[p] += pts
	if g.scores[p] >= WinScore {
		g.scores[p] = WinScore
		return true
	}
	return false
}

func (g *Game) finish(winner int) {
	g.phase = PhaseGameOver
	g.winner = winner
	g.turn = -1
	skunk := 0
	switch loser := g.scores[1-winner]; {
	case loser < 61:
		skunk = 2
	case loser < 91:
		skunk = 1
	}
	g.emit(Event{Kind: EvGameOver, Player: winner, Card: NoCard, Skunk: skunk})
}

func (g *Game) cutForDeal() {
	for {
		a := Card(g.rng.intn(52))
		b := Card(g.rng.intn(51))
		if b >= a {
			b++
		}
		g.emit(Event{Kind: EvCutForDeal, Player: -1, Card: NoCard, Cards: [4]Card{a, b, NoCard, NoCard}})
		if a.Rank() != b.Rank() {
			if a.Rank() < b.Rank() {
				g.dealer = 0
			} else {
				g.dealer = 1
			}
			return
		}
	}
}

func (g *Game) deal() {
	var d [52]Card
	for i := range d {
		d[i] = Card(i)
	}
	// Partial Fisher-Yates: only the 13 cards we need are shuffled.
	for i := 0; i < 13; i++ {
		j := i + g.rng.intn(52-i)
		d[i], d[j] = d[j], d[i]
	}
	g.handNum++
	nd := 1 - g.dealer
	for i := 0; i < 6; i++ {
		g.hands[nd][i] = d[2*i]
		g.hands[g.dealer][i] = d[2*i+1]
	}
	sortCards(g.hands[0][:])
	sortCards(g.hands[1][:])
	g.handN = [2]int{6, 6}
	g.pending = d[12]
	g.starter = NoCard
	g.discarded = [2]bool{}
	g.cribN = 0
	g.count = 0
	g.playedN = 0
	g.seqStart = 0
	g.goSaid = [2]bool{}
	g.last = -1
	g.turn = -1
	g.phase = PhaseDiscard
	g.emit(Event{Kind: EvDeal, Player: g.dealer, Card: NoCard})
}

func (g *Game) removeAt(p, i int) Card {
	c := g.hands[p][i]
	copy(g.hands[p][i:], g.hands[p][i+1:g.handN[p]])
	g.handN[p]--
	g.hands[p][g.handN[p]] = NoCard
	return c
}

func (g *Game) discard(p, i, j int) {
	if i < j {
		i, j = j, i // remove the higher index first
	}
	a := g.removeAt(p, i)
	b := g.removeAt(p, j)
	g.discards[p] = [2]Card{b, a}
	g.crib[g.cribN], g.crib[g.cribN+1] = b, a
	g.cribN += 2
	copy(g.kept[p][:], g.hands[p][:4])
	g.discarded[p] = true
	g.emit(Event{Kind: EvDiscard, Player: p, Card: NoCard})
	if g.discarded[0] && g.discarded[1] {
		g.startPegging()
	}
}

func (g *Game) startPegging() {
	g.starter = g.pending
	heels := 0
	if g.starter.Rank() == 11 {
		heels = 2
	}
	won := heels > 0 && g.addScore(g.dealer, heels)
	g.emit(Event{Kind: EvStarter, Player: g.dealer, Card: g.starter, Points: heels})
	if won {
		g.finish(g.dealer)
		return
	}
	g.phase = PhasePegging
	g.turn = 1 - g.dealer // non-dealer leads, and always holds a playable card
}

func (g *Game) canPlay(p int) bool {
	for i := 0; i < g.handN[p]; i++ {
		if g.count+g.hands[p][i].Value() <= 31 {
			return true
		}
	}
	return false
}

func (g *Game) play(p, i int) {
	c := g.removeAt(p, i)
	g.played[g.playedN] = PlayedCard{Card: c, Player: p}
	g.playedN++
	g.count += c.Value()
	g.last = p

	var seq [8]Card
	n := 0
	for _, pc := range g.played[g.seqStart:g.playedN] {
		seq[n] = pc.Card
		n++
	}
	ps := ScorePeg(seq[:n])
	won := g.addScore(p, ps.Total())
	g.emit(Event{Kind: EvPlay, Player: p, Card: c, Count: g.count, Points: ps.Total(), Peg: ps})
	if won {
		g.finish(p)
		return
	}
	if g.count == 31 {
		g.resetCount()
	}
	g.advance(1 - p)
}

func (g *Game) resetCount() {
	g.count = 0
	g.seqStart = g.playedN
	g.goSaid = [2]bool{}
	g.emit(Event{Kind: EvCountReset, Player: -1, Card: NoCard})
}

func (g *Game) sayGo(p int) {
	if g.handN[p] > 0 && !g.goSaid[p] {
		g.goSaid[p] = true
		g.emit(Event{Kind: EvGo, Player: p, Card: NoCard, Count: g.count})
	}
}

// advance hands the turn to next if possible, resolving gos, the last card
// and the end of pegging.
func (g *Game) advance(next int) {
	for {
		if g.handN[0] == 0 && g.handN[1] == 0 {
			if g.count > 0 {
				won := g.addScore(g.last, 1)
				g.emit(Event{Kind: EvLastCard, Player: g.last, Card: NoCard, Points: 1, Count: g.count})
				if won {
					g.finish(g.last)
					return
				}
			}
			g.show()
			return
		}
		if g.canPlay(next) {
			g.turn = next
			return
		}
		g.sayGo(next)
		if g.canPlay(1 - next) {
			g.turn = 1 - next
			return
		}
		// Neither player can play: the last player to play scores the go.
		won := g.addScore(g.last, 1)
		g.emit(Event{Kind: EvGoPoint, Player: g.last, Card: NoCard, Points: 1, Count: g.count})
		if won {
			g.finish(g.last)
			return
		}
		g.resetCount()
		next = 1 - g.last
	}
}

func (g *Game) show() {
	g.turn = -1
	nd := 1 - g.dealer
	for _, p := range [2]int{nd, g.dealer} {
		b := ScoreHand(g.kept[p], g.starter, false)
		won := g.addScore(p, b.Total())
		g.emit(Event{Kind: EvShowHand, Player: p, Card: g.starter, Cards: g.kept[p], Points: b.Total(), Show: b})
		if won {
			g.finish(p)
			return
		}
	}
	b := ScoreHand(g.crib, g.starter, true)
	won := g.addScore(g.dealer, b.Total())
	g.emit(Event{Kind: EvShowCrib, Player: g.dealer, Card: g.starter, Cards: g.crib, Points: b.Total(), Show: b})
	if won {
		g.finish(g.dealer)
		return
	}
	g.dealer = 1 - g.dealer
	g.deal()
}

// pcg wraps math/rand/v2's PCG by value so that Game stays copyable.
type pcg struct{ src rand.PCG }

func newPCG(seed uint64) pcg {
	return pcg{src: *rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)}
}

// intn returns a value in [0, n) using Lemire's multiply-shift reduction.
func (r *pcg) intn(n int) int {
	hi, _ := bits.Mul64(r.src.Uint64(), uint64(n))
	return int(hi)
}
