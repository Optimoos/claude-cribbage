// Package players turns a player spec from the command line into bots.
package players

import (
	"fmt"
	"strconv"
	"strings"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/expert"
)

// Resolve turns a spec into a per-worker constructor. A spec is a
// registered bot name, a network model file (played greedily), or an
// expert bundle file, optionally followed by @effort (opponent hands
// searched per pegging decision; 0 for all).
func Resolve(spec string) (func(worker int) bots.Bot, error) {
	if _, err := bots.New(spec, 0); err == nil {
		return func(w int) bots.Bot { b, _ := bots.New(spec, uint64(w)); return b }, nil
	}
	path, effort := spec, expert.DefaultEffort
	if i := strings.LastIndexByte(spec, '@'); i >= 0 {
		e, err := strconv.Atoi(spec[i+1:])
		if err != nil || e < 0 {
			return nil, fmt.Errorf("%q: bad effort after @", spec)
		}
		path, effort = spec[:i], e
	}
	if b, err := expert.LoadBundle(path); err == nil {
		return func(int) bots.Bot { return expert.NewBot(b, effort) }, nil
	} else if path != spec {
		return nil, err
	}
	net, _, err := ai.Load(spec)
	if err != nil {
		return nil, fmt.Errorf("%q is neither a bot (%v), a bundle nor a loadable model: %w", spec, bots.Names(), err)
	}
	return func(w int) bots.Bot { return ai.NewAgent(net, 0, uint64(w)) }, nil
}
