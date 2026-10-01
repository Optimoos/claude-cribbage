// Package bots contains computer players and a runner that plays them
// against each other using the rules engine directly.
package bots

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"cribbage/internal/engine"
)

// Bot chooses actions from a player's observation. A Bot instance is not
// safe for concurrent use; create one per goroutine.
type Bot interface {
	Name() string
	// Choose returns one of legal, which is never empty.
	Choose(o *engine.Observation, legal []engine.Action) engine.Action
}

var factories = map[string]func(seed uint64) Bot{
	"random":    func(seed uint64) Bot { return NewRandom(seed) },
	"heuristic": func(uint64) Bot { return NewHeuristic() },
}

// Register adds a bot factory under name, replacing any existing one. Call
// it during startup, before bots are created concurrently.
func Register(name string, f func(seed uint64) Bot) { factories[name] = f }

// New creates a bot by name. The seed drives any randomness in the bot.
func New(name string, seed uint64) (Bot, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown bot %q (available: %v)", name, Names())
	}
	return f(seed), nil
}

// Names lists the registered bot names.
func Names() []string {
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Random picks uniformly among legal actions.
type Random struct{ rng *rand.Rand }

func NewRandom(seed uint64) *Random {
	return &Random{rng: rand.New(rand.NewPCG(seed, 0x5eed))}
}

func (r *Random) Name() string { return "random" }

func (r *Random) Choose(_ *engine.Observation, legal []engine.Action) engine.Action {
	return legal[r.rng.IntN(len(legal))]
}
