package expert

import "cribbage/internal/bots"

// Level is a named difficulty: a set of bot options. Weaker levels search
// less and drop components, the opponent model first, so the spread is
// wider than effort alone would give. Every level is deterministic.
type Level struct {
	Name    string // bot name in the lobby and API
	Info    bots.Info
	Options Options
}

// Levels lists the difficulties from weakest to strongest; the last is the
// full-strength bot, registered under BotName. Measured with the M14
// bundle, paired against the heuristic: easy 51.0%, medium 54.8%, expert
// 57.2%.
var Levels = []Level{
	{"expert-easy", bots.Info{Label: "Expert (easy)", Blurb: "A quick look ahead", Icon: "♝"}, Options{Effort: 10, NoKeepModel: true, NoPegModel: true, HeuristicDiscard: true}},
	{"expert-medium", bots.Info{Label: "Expert (medium)", Blurb: "A lighter search", Icon: "♜"}, Options{Effort: 50, NoKeepModel: true, NoPegModel: true}},
	{BotName, bots.Info{Label: "Expert", Blurb: "Searches and plays to win", Icon: "♛"}, Options{Effort: DefaultEffort}},
}

// Register makes every level of the bundle available by name. Bots created
// from it share the bundle's tables. Call it during startup, before bots
// are created concurrently.
func Register(b *Bundle) {
	b.keepModel() // build shared state now rather than in the first game
	for _, l := range Levels {
		opt := l.Options
		bots.Register(l.Name, func(uint64) bots.Bot { return NewBotWith(b, opt) })
		bots.Describe(l.Name, l.Info)
	}
}
