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
// hands and discards as the heuristic does (until M12). It is
// deterministic and not safe for concurrent use; many Bots may share one
// Bundle.
type Bot struct {
	heuristic bots.Heuristic
	search    *searcher
}

// NewBot creates an expert bot searching at most effort opponent hands per
// pegging decision (0 for all of them).
func NewBot(b *Bundle, effort int) *Bot {
	return &Bot{search: newSearcher(b.Win, effort)}
}

func (b *Bot) Name() string { return BotName }

func (b *Bot) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	if len(legal) == 1 {
		return legal[0]
	}
	if o.Phase == engine.PhaseDiscard {
		return b.heuristic.Choose(o, legal)
	}
	return b.search.choose(o, legal)
}
