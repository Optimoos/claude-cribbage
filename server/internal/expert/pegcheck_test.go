package expert

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// TestPegPolicyShare measures proposal 2's own test: the share of world
// weight on the opponent's real hand, with the bundle's fitted pegging
// policy against the softmax on points, in games of the bot named by
// CRIB_PEGCHECK_BOT (default heuristic2). Set CRIB_PEGCHECK to the
// bundle's path to run it.
func TestPegPolicyShare(t *testing.T) {
	path := os.Getenv("CRIB_PEGCHECK")
	if path == "" {
		t.Skip("set CRIB_PEGCHECK to a bundle with a pegging policy")
	}
	b, err := LoadBundle(path)
	if err != nil || b.PegPolicy == nil {
		t.Fatalf("%s: %v (policy %v)", path, err, b != nil && b.PegPolicy != nil)
	}
	name := os.Getenv("CRIB_PEGCHECK_BOT")
	if name == "" {
		name = "heuristic2"
	}
	var bot bots.Bot
	if strings.Contains(name, "/") {
		net, _, err := ai.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		bot = ai.NewAgent(net, 0, 1)
	} else if bot, err = bots.New(name, 1); err != nil {
		t.Fatal(err)
	}
	mk := func(pol bool) *searcher {
		s := newSearcher(b.Win, 0)
		s.keep = b.keepModel()
		s.pegTemp = DefaultPegTemp
		if pol {
			s.pegPol = b.PegPolicy
			if x := os.Getenv("CRIB_PEGCHECK_MIX"); x != "" {
				s.pegMix, _ = strconv.ParseFloat(x, 64)
			}
		}
		return s
	}
	old, pol := mk(false), mk(true)
	var lo, lp, gain float64
	var bad1, bad2 int
	n := 0
	botGames(t, bot, 400, func(g *engine.Game, o *engine.Observation, _ engine.Action) {
		if o.Phase != engine.PhasePegging || o.OppHandN < 1 {
			return
		}
		var real [14]int
		for _, c := range g.Hand(1 - o.Player) {
			real[c.Rank()]++
		}
		var seen [52]bool
		for _, c := range o.Kept {
			seen[c] = true
		}
		for _, c := range o.Discards {
			seen[c] = true
		}
		seen[o.Starter] = true
		var oppPlayed []engine.Card
		for _, pc := range o.Played[:o.PlayedN] {
			seen[pc.Card] = true
			if pc.Player != o.Player {
				oppPlayed = append(oppPlayed, pc.Card)
			}
		}
		if len(oppPlayed) == 0 {
			return
		}
		share := func(s *searcher) float64 {
			s.enumerate(o, &seen, oppPlayed)
			var total, mine float64
			for _, w := range s.reweight(o, oppPlayed, s.worlds) {
				total += w.weight
				var got [14]int
				for _, r := range w.ranks[:w.n] {
					got[r]++
				}
				if got == real {
					mine += w.weight
				}
			}
			return mine / total
		}
		a, c := share(old), share(pol)
		if r := math.Log(c / a); r < -2 {
			bad2++
		} else if r < -1 {
			bad1++
		}
		lo += math.Log(a)
		lp += math.Log(c)
		gain += math.Log(c / a)
		n++
	})
	t.Logf("%s, %d positions after an opponent play: mean log share of the real hand: points %.4f, policy %.4f, gain %.4f",
		name, n, lo/float64(n), lp/float64(n), gain/float64(n))
	t.Logf("positions where the policy's share is under 1/e of the old one's: %.2f%%; under 1/e^2: %.2f%%",
		100*float64(bad1+bad2)/float64(n), 100*float64(bad2)/float64(n))
}
