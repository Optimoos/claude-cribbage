package expert

import (
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// BotName is the name the expert bot plays under.
const BotName = "expert"

// DefaultEffort is the default number of opponent hands searched per
// pegging decision.
const DefaultEffort = 400

// DefaultPegTemp is the default temperature, in points, of the model of the
// opponent's pegging used to weight their possible hands.
const DefaultPegTemp = 1.0

// DefaultPegMix is the softmax on points' weight in the opponent pegging
// model when the bundle has a fitted policy: it keeps the policy from
// ruling out the real hand of an opponent unlike the bots it was fitted to
// (PLAN M38).
const DefaultPegMix = 0.2

// Options configure a Bot. The zero value plus an Effort uses every
// component the bundle has.
type Options struct {
	Effort           int     // opponent hands searched per pegging decision; 0 for all
	PegTemp          float64 // temperature of the opponent pegging model; 0 for DefaultPegTemp
	NoKeepModel      bool    // do not weight opponent hands by how likely they were kept
	NoPegModel       bool    // do not weight opponent hands by the plays made with them
	HeuristicDiscard bool    // discard as the heuristic does, ignoring the discard tables
	Oracle           Oracle  // test only: let pegging see some of the opponent's hidden cards
	MixFirst         float64 // proposal 9: mix the first play of a hand among plays within this much win probability of the best
	NoExactKeep      bool    // ignore the bundle's exact keep table: the softmax model alone
	KeepEps          float64 // the softmax's weight against the exact keep table, when KeepEpsSet
	KeepEpsSet       bool    // otherwise DefaultKeepEps
	NoSmallFixes     bool    // play as M14 did: no heels order, even odds when both peg out, no hidden suits, one crib
	ISSearch         bool    // peg by information-set search rather than minimax per world (proposal 1)
	NoPegPolicy      bool    // ignore the bundle's fitted pegging policy: the softmax on points at PegTemp
	PegMix           float64 // mix the softmax on points into the policy's chance of each play at this weight, when PegMixSet
	PegMixSet        bool    // otherwise DefaultPegMix
	ISLambda         float64 // with ISSearch and a pegging policy: the weight of the opponent's best reply against the policy, when ISLambdaSet
	ISLambdaSet      bool    // otherwise 1, best reply alone
	CondDiscard      bool    // condition the opponent's show and the crib on their likely hands (proposal 4)
	CondPeg          bool    // with CondDiscard and a shape-pair table: condition the pegging too
}

// Oracle is how much of the opponent's hand a test oracle lets the pegging
// search see. Oracles bound what better inference could be worth; they
// cheat, so they only play in tests and experiments.
type Oracle int

const (
	NoOracle   Oracle = iota
	OracleSix         // the six cards they were dealt, but not which four they kept
	OracleHand        // the cards they hold (clairvoyant)
)

// Bot is the expert player. It pegs by searching the possible opponent
// hands, and discards by valuing each option through the bundle's tables
// (or as the heuristic does, if the bundle has none). It is deterministic
// and not safe for concurrent use; many Bots may share one Bundle.
type Bot struct {
	heuristic bots.Heuristic
	search    *searcher
	discard   *discarder // nil without discard tables
}

// NewBot creates an expert bot searching at most effort opponent hands per
// pegging decision (0 for all of them), using every component the bundle
// has.
func NewBot(b *Bundle, effort int) *Bot { return NewBotWith(b, Options{Effort: effort}) }

// NewBotWith creates an expert bot with the given options.
func NewBotWith(b *Bundle, opt Options) *Bot {
	bot := &Bot{search: newSearcher(b.Win, opt.Effort)}
	bot.search.oracle = opt.Oracle
	bot.search.mixFirst = opt.MixFirst
	bot.search.noFixes = opt.NoSmallFixes
	bot.search.isSearch = opt.ISSearch
	bot.search.isLambda = 1
	if opt.ISLambdaSet {
		bot.search.isLambda = opt.ISLambda
	}
	if b.Discard != nil && !opt.HeuristicDiscard {
		bot.discard = newDiscarder(b.Win, b.Discard)
		bot.discard.noFixes = opt.NoSmallFixes
		if opt.CondDiscard {
			eps := DefaultKeepEps
			if opt.KeepEpsSet {
				eps = opt.KeepEps
			}
			bot.discard.keep = b.keepModelMix(eps, opt.NoExactKeep)
			if opt.CondPeg && b.PegPair != nil && bot.discard.keep != nil {
				bot.discard.pair = b.PegPair
				bot.discard.base = pegBaseFor(b.PegPair, bot.discard.keep)
			}
		}
	}
	if !opt.NoKeepModel {
		eps := DefaultKeepEps
		if opt.KeepEpsSet {
			eps = opt.KeepEps
		}
		bot.search.keep = b.keepModelMix(eps, opt.NoExactKeep)
	}
	if !opt.NoPegModel {
		bot.search.pegTemp = opt.PegTemp
		if bot.search.pegTemp == 0 {
			bot.search.pegTemp = DefaultPegTemp
		}
		if b.PegPolicy != nil && !opt.NoPegPolicy && opt.PegTemp == 0 {
			bot.search.pegPol = b.PegPolicy
			bot.search.pegMix = DefaultPegMix
			if opt.PegMixSet {
				bot.search.pegMix = opt.PegMix
			}
		}
	}
	return bot
}

func (b *Bot) Name() string { return BotName }

// SetSeed sets the seed that draws mixed plays (see Options.MixFirst).
func (b *Bot) SetSeed(seed uint64) { b.search.seed = seed }

// PlayDist returns the pegging plays the bot may make in o and their
// chances; without mixing, one play with chance 1.
func (b *Bot) PlayDist(o *engine.Observation, legal []engine.Action) ([]engine.Action, []float64) {
	if len(legal) == 1 {
		return legal, []float64{1}
	}
	return b.search.dist(o, legal)
}

// Watch lets an oracle see the game; other bots ignore it.
func (b *Bot) Watch(g *engine.Game, seat int) {
	if b.search.oracle != NoOracle {
		b.search.game, b.search.seat = g, seat
	}
}

func (b *Bot) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if len(legal) == 1 {
		return legal[0]
	}
	if o.Phase == engine.PhaseDiscard {
		if b.discard == nil {
			return b.heuristic.Choose(o, legal)
		}
		return b.discard.choose(o, legal)
	}
	return b.search.choose(o, legal)
}
