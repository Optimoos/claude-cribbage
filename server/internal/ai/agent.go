// Package ai contains the neural network player and tools to measure it.
package ai

import (
	"fmt"
	"math/rand/v2"

	"cribbage/internal/ai/encode"
	"cribbage/internal/ai/nn"
	"cribbage/internal/engine"
)

// Agent plays by evaluating the afterstate of every legal action with a
// value network and choosing the one with the highest win probability.
// It implements bots.Bot. An Agent is not safe for concurrent use, but many
// Agents may share one Net.
type Agent struct {
	net     *nn.Net
	scratch *nn.Scratch
	Epsilon float64 // probability of a uniformly random move (exploration)
	// Lookahead values each discard by averaging over every possible
	// starter card instead of judging the hand before the cut. It is
	// slower (about 46 evaluations per option) and stronger.
	Lookahead bool
	rng     *rand.Rand
	x, best nn.Sparse
}

// NewAgent creates an agent. Epsilon 0 plays greedily.
func NewAgent(net *nn.Net, epsilon float64, seed uint64) *Agent {
	a := &Agent{Epsilon: epsilon, rng: rand.New(rand.NewPCG(seed, 0xa6e7))}
	a.SetNet(net)
	return a
}

// CheckNet reports whether net can be used with the current encoding.
func CheckNet(net *nn.Net, meta nn.Meta) error {
	if meta.EncodingVersion != 0 && meta.EncodingVersion != encode.Version {
		return fmt.Errorf("model uses feature encoding v%d; this build uses v%d", meta.EncodingVersion, encode.Version)
	}
	if in := net.Layers[0].In; in != encode.Size {
		return fmt.Errorf("model expects %d inputs; encoding has %d", in, encode.Size)
	}
	return nil
}

// SetNet switches to another network (e.g. fresher weights).
func (a *Agent) SetNet(net *nn.Net) {
	if a.net == nil || !sameShape(a.net, net) {
		a.scratch = net.NewScratch()
	}
	a.net = net
}

func sameShape(a, b *nn.Net) bool {
	if len(a.Layers) != len(b.Layers) {
		return false
	}
	for i := range a.Layers {
		if a.Layers[i].In != b.Layers[i].In || a.Layers[i].Out != b.Layers[i].Out {
			return false
		}
	}
	return true
}

func (a *Agent) Name() string { return "nn" }

// Choose implements bots.Bot.
func (a *Agent) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	act, _, _ := a.Decide(o, legal)
	return act
}

// Decide picks an action and returns it with the features of its
// afterstate and the network's value for it. The features are only valid
// until the next call.
func (a *Agent) Decide(o *engine.Observation, legal []engine.Action) (engine.Action, nn.Sparse, float32) {
	bestI, bestV := 0, float32(-1)
	var values [16]float32 // at most 15 legal actions (discard choices)
	for i, act := range legal {
		v := a.Value(o, act)
		values[i] = v
		if v > bestV {
			bestI, bestV = i, v
		}
	}
	if a.Epsilon > 0 && len(legal) > 1 && a.rng.Float64() < a.Epsilon {
		bestI = a.rng.IntN(len(legal))
	}
	after, _ := encode.Afterstate(o, legal[bestI])
	encode.Encode(&after, &a.best)
	return legal[bestI], a.best, values[bestI]
}

// Value estimates the observer's chance of winning after taking act.
func (a *Agent) Value(o *engine.Observation, act engine.Action) float32 {
	after, won := encode.Afterstate(o, act)
	if won {
		return 1
	}
	if a.Lookahead && act.Kind == engine.ActDiscard {
		return a.starterAverage(&after)
	}
	encode.Encode(&after, &a.x)
	return a.net.Predict(a.x, a.scratch)
}

// starterAverage averages the value of after over every starter the
// observer cannot rule out, as the state will look once both players have
// discarded and the starter is cut.
func (a *Agent) starterAverage(after *engine.Observation) float32 {
	var seen [52]bool
	for _, c := range after.Kept {
		seen[c] = true
	}
	for _, c := range after.Discards {
		seen[c] = true
	}
	cut := *after
	cut.OppDiscarded = true
	cut.Phase = engine.PhasePegging
	cut.Turn = 1 - cut.Dealer
	me, dealer := after.Player, after.Dealer
	var sum float32
	n := 0
	for s := engine.Card(0); s < 52; s++ {
		if seen[s] {
			continue
		}
		n++
		cut.Starter = s
		cut.Scores = after.Scores
		if s.Rank() == 11 { // his heels: two for the dealer
			cut.Scores[dealer] = min(cut.Scores[dealer]+2, engine.WinScore)
			if cut.Scores[dealer] == engine.WinScore {
				if dealer == me {
					sum++
				}
				continue
			}
		}
		encode.Encode(&cut, &a.x)
		sum += a.net.Predict(a.x, a.scratch)
	}
	return sum / float32(n)
}
