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
// expert bundle file, optionally followed by @ and comma-separated options:
// a number (opponent hands searched per pegging decision; 0 for all),
// nokeep, nopeg, heurdiscard, pegtemp=X, noexact (ignore the exact keep
// table), keepeps=X (its mix with the softmax), and the test oracles oracle=six
// and oracle=hand (see expert.Oracle).
//
//	models-expert/expert.bin@100,nokeep
func Resolve(spec string) (func(worker int) bots.Bot, error) {
	if _, err := bots.New(spec, 0); err == nil {
		return func(w int) bots.Bot { b, _ := bots.New(spec, uint64(w)); return b }, nil
	}
	path, opt := spec, expert.Options{Effort: expert.DefaultEffort}
	if i := strings.LastIndexByte(spec, '@'); i >= 0 {
		path = spec[:i]
		for _, o := range strings.Split(spec[i+1:], ",") {
			var err error
			switch {
			case o == "nokeep":
				opt.NoKeepModel = true
			case o == "nopeg":
				opt.NoPegModel = true
			case o == "heurdiscard":
				opt.HeuristicDiscard = true
			case o == "noexact":
				opt.NoExactKeep = true
			case strings.HasPrefix(o, "keepeps="):
				opt.KeepEps, err = strconv.ParseFloat(o[len("keepeps="):], 64)
				opt.KeepEpsSet = true
			case o == "oracle=six":
				opt.Oracle = expert.OracleSix
			case o == "oracle=hand":
				opt.Oracle = expert.OracleHand
			case strings.HasPrefix(o, "pegtemp="):
				opt.PegTemp, err = strconv.ParseFloat(o[len("pegtemp="):], 64)
			default:
				opt.Effort, err = strconv.Atoi(o)
				if opt.Effort < 0 {
					err = fmt.Errorf("negative effort")
				}
			}
			if err != nil {
				return nil, fmt.Errorf("%q: bad option %q: %v", spec, o, err)
			}
		}
	}
	if b, err := expert.LoadBundle(path); err == nil {
		return func(int) bots.Bot { return expert.NewBotWith(b, opt) }, nil
	} else if path != spec {
		return nil, err
	}
	net, _, err := ai.Load(spec)
	if err != nil {
		return nil, fmt.Errorf("%q is neither a bot (%v), a bundle nor a loadable model: %w", spec, bots.Names(), err)
	}
	return func(w int) bots.Bot { return ai.NewAgent(net, 0, uint64(w)) }, nil
}
