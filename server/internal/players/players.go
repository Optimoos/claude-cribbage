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
// nokeep, nopeg, heurdiscard, pegtemp=X, is (information-set pegging
// search), nofixes (M14's play, without
// proposal 6's small fixes), noexact (ignore the exact keep
// table), keepeps=X (its mix with the softmax), nopegpol (ignore the fitted
// pegging policy), pegmix=X (mix the softmax on points into it at weight
// X), cond (discards conditioned on the opponent's likely
// hands), condpeg (and their pegging, with a shape-pair table), islambda=X (information-set search, the opponent's best
// reply at weight X against the policy), and the test oracles oracle=six
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
		var err error
		if opt, err = ParseOptions(spec[i+1:]); err != nil {
			return nil, fmt.Errorf("%q: %v", spec, err)
		}
	}
	if b, err := expert.LoadBundle(path); err == nil {
		if err := b.Check(opt); err != nil {
			return nil, fmt.Errorf("%q: %v", spec, err)
		}
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

// ParseOptions reads the comma-separated options after a bundle's @ (see
// Resolve) on top of the default effort.
func ParseOptions(s string) (expert.Options, error) {
	opt := expert.Options{Effort: expert.DefaultEffort}
	for _, o := range strings.Split(s, ",") {
		var err error
		switch {
		case o == "nokeep":
			opt.NoKeepModel = true
		case o == "nopeg":
			opt.NoPegModel = true
		case o == "heurdiscard":
			opt.HeuristicDiscard = true
		case o == "is":
			opt.ISSearch = true
		case o == "nofixes":
			opt.NoSmallFixes = true
		case o == "noexact":
			opt.NoExactKeep = true
		case o == "cond":
			opt.CondDiscard = true
		case o == "condpeg":
			opt.CondDiscard, opt.CondPeg = true, true
		case o == "nopegpol":
			opt.NoPegPolicy = true
		case strings.HasPrefix(o, "pegmix="):
			opt.PegMix, err = strconv.ParseFloat(o[len("pegmix="):], 64)
			opt.PegMixSet = true
		case strings.HasPrefix(o, "islambda="):
			opt.ISLambda, err = strconv.ParseFloat(o[len("islambda="):], 64)
			opt.ISLambdaSet, opt.ISSearch = true, true
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
			return opt, fmt.Errorf("bad option %q: %v", o, err)
		}
	}
	return opt, nil
}
