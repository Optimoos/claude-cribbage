package expert

import "cribbage/internal/bots"

// Level is a named difficulty: a set of bot options. Weaker levels search
// less and drop components, the opponent model first, so the spread is
// wider than effort alone would give. Every level is deterministic.
type Level struct {
	Name    string // bot name in the lobby and API
	Label   string
	Options Options
}

// Levels lists the difficulties from weakest to strongest; the last is the
// full-strength bot, registered under BotName. Measured with the bundle of
// PLAN M41 (M14's tables, the exact keep table and the pegging policy),
// paired against the heuristic: medium 50.5%, hard 54.4%, very hard 58.6%
// (random, the easy level, wins 0.6%). The very hard level needs the
// pegging policy: check a bundle with Bundle.Check before registering it.
var Levels = []Level{
	{"expert-easy", "Medium", Options{Effort: 10, NoKeepModel: true, NoPegModel: true, HeuristicDiscard: true}},
	{"expert-medium", "Hard", Options{Effort: 50, NoKeepModel: true, NoPegModel: true}},
	{BotName, "Very hard", Options{Effort: DefaultEffort, ISSearch: true, ISLambda: 0.5, ISLambdaSet: true, CondDiscard: true}},
}

// Register makes every level of the bundle available by name. Bots created
// from it share the bundle's tables. Call it during startup, before bots
// are created concurrently.
func Register(b *Bundle) {
	b.keepModel() // build shared state now rather than in the first game
	for _, l := range Levels {
		opt := l.Options
		bots.Register(l.Name, func(seed uint64) bots.Bot {
			nb := NewBotWith(b, opt)
			nb.SetSeed(seed)
			return nb
		})
	}
}
