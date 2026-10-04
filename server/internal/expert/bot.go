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
// pegging decision (0 for all of them).
func NewBot(b *Bundle, effort int) *Bot {
	bot := &Bot{search: newSearcher(b.Win, effort)}
	if b.Discard != nil {
		bot.discard = newDiscarder(b.Win, b.Discard)
	}
	return bot
}

func (b *Bot) Name() string { return BotName }

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
