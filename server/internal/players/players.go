// Package players turns a player spec from the command line into bots.
package players

import (
	"fmt"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
)

// Resolve turns a spec into a per-worker constructor: a registered bot
// name, or else a network model file played greedily.
func Resolve(spec string) (func(worker int) bots.Bot, error) {
	if _, err := bots.New(spec, 0); err == nil {
		return func(w int) bots.Bot { b, _ := bots.New(spec, uint64(w)); return b }, nil
	}
	net, _, err := ai.Load(spec)
	if err != nil {
		return nil, fmt.Errorf("%q is neither a bot (%v) nor a loadable model: %w", spec, bots.Names(), err)
	}
	return func(w int) bots.Bot { return ai.NewAgent(net, 0, uint64(w)) }, nil
}
