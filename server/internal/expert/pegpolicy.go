package expert

import (
	"fmt"
	"math"
	"slices"

	"cribbage/internal/engine"
)

// The opponent pegging policy (proposal 2) is a conditional logit: the
// chance a player plays rank h from the ranks they hold is proportional to
// exp(W·f(h)), over the distinct ranks they may play. Features come in two
// kinds. Public features depend only on what both players have seen (the
// cards played this hand and the starter), so the search computes them once
// per play for every rank; private ones depend on the rest of the hand, so
// they are worked out per world. Reply risk takes its belief from the
// public cards alone, both when fitting and in use, so the two agree.

// Feature indices.
const (
	pfPoints      = iota // points the play scores at once
	pfRisk               // expected points of the reply, over unseen ranks
	pfCount5             // the play leaves the count at 5
	pfCount10            // ... at 10
	pfCount21            // ... at 21
	pfHigh               // ... at 22 to 30
	pfValue              // the card's value / 10
	pfLead               // the play leads (the count was 0)
	pfLeadValue          // lead × value / 10
	pfLeadFive           // lead of a five
	pfPairHeld           // another card of this rank is still held (private)
	pfLeadPair           // lead × pair held (private)
	pfFifteenHeld        // a held card makes 15 with this one (private)
	pfPlayable           // share of the rest still playable on the new count (private)
	pfLast               // the rest holds nothing playable on the new count (private)
	nPegFeat
)

// pegFeatNames name the features, for reports.
var pegFeatNames = [nPegFeat]string{
	"points", "risk", "count5", "count10", "count21", "high", "value",
	"lead", "lead·value", "lead five", "pair held", "lead·pair", "fifteen held", "playable", "last",
}

// pegPolicyShape is the bundle table's shape.
var pegPolicyShape = []int{nPegFeat}

// pegPublic sets the public features of playing rank h onto seq (the ranks
// played since the count reset) at count, with avail the unseen count of
// each rank from the public view.
func pegPublic(seq []int8, count int8, avail *[14]int8, h int8, f *[nPegFeat]float64) {
	*f = [nPegFeat]float64{}
	var s [9]int8
	n := copy(s[:], seq)
	s[n] = h
	c2 := count + rankValue(h)
	f[pfPoints] = float64(pegPoints(s[:n+1], c2))
	if c2 < 31 {
		var tot, risk float64
		for r := int8(1); r <= 13; r++ {
			a := float64(avail[r])
			tot += a
			if a == 0 || c2+rankValue(r) > 31 {
				continue
			}
			s[n+1] = r
			risk += a * float64(pegPoints(s[:n+2], c2+rankValue(r)))
		}
		if tot > 0 {
			f[pfRisk] = risk / tot
		}
	}
	switch {
	case c2 == 5:
		f[pfCount5] = 1
	case c2 == 10:
		f[pfCount10] = 1
	case c2 == 21:
		f[pfCount21] = 1
	case c2 >= 22 && c2 <= 30:
		f[pfHigh] = 1
	}
	v := float64(rankValue(h)) / 10
	f[pfValue] = v
	if count == 0 {
		f[pfLead] = 1
		f[pfLeadValue] = v
		if h == 5 {
			f[pfLeadFive] = 1
		}
	}
}

// pegPrivate sets the private features of playing rank h, leaving rest
// held, at count (before the play).
func pegPrivate(rest []int8, count int8, h int8, f *[nPegFeat]float64) {
	c2 := count + rankValue(h)
	if c2 == 31 {
		c2 = 0
	}
	pair, fifteen := 0.0, 0.0
	playable := 0
	for _, r := range rest {
		if r == h {
			pair = 1
		}
		if rankValue(r)+rankValue(h) == 15 {
			fifteen = 1
		}
		if c2+rankValue(r) <= 31 {
			playable++
		}
	}
	f[pfPairHeld] = pair
	f[pfLeadPair] = pair * f[pfLead]
	f[pfFifteenHeld] = fifteen
	f[pfPlayable], f[pfLast] = 0, 0
	if len(rest) > 0 {
		f[pfPlayable] = float64(playable) / float64(len(rest))
		if playable == 0 {
			f[pfLast] = 1
		}
	}
}

// PegPolicy is the fitted weight of each feature.
type PegPolicy [nPegFeat]float64

// pointsPolicy is the old model as a policy: a softmax on immediate points
// at temperature t.
func pointsPolicy(t float64) PegPolicy {
	var p PegPolicy
	p[pfPoints] = 1 / t
	return p
}

func (p *PegPolicy) dot(f *[nPegFeat]float64) float64 {
	var s float64
	for i, w := range p {
		s += w * f[i]
	}
	return s
}

// dotPrivate is the private features' part of the score.
func (p *PegPolicy) dotPrivate(f *[nPegFeat]float64) float64 {
	return p[pfPairHeld]*f[pfPairHeld] + p[pfLeadPair]*f[pfLeadPair] + p[pfFifteenHeld]*f[pfFifteenHeld] +
		p[pfPlayable]*f[pfPlayable] + p[pfLast]*f[pfLast]
}

// String lists the weights by feature name.
func (p PegPolicy) String() string {
	s := ""
	for i, w := range p {
		s += fmt.Sprintf("  %-13s %+.3f\n", pegFeatNames[i], w)
	}
	return s
}

// oppPlay is one opponent play of the hand so far, with each rank's public
// score at that point (when it could be played).
type oppPlay struct {
	rank  int8
	count int8
	lead  bool
	pub   [14]float64
	pts   [14]float64 // the points each rank scores at once
}

// publicAvail is each rank's count not yet seen from the public view: not
// the starter and not played this hand.
func publicAvail(o *engine.Observation) [14]int8 {
	var avail [14]int8
	for r := 1; r <= 13; r++ {
		avail[r] = 4
	}
	avail[o.Starter.Rank()]--
	for _, pc := range o.Played[:o.PlayedN] {
		avail[pc.Card.Rank()]--
	}
	return avail
}

// prepPegPolicy works out the public part of every opponent play so far
// in the observed hand, for pegPolicyLik.
func (s *searcher) prepPegPolicy(o *engine.Observation) {
	s.oppPlays = s.oppPlays[:0]
	var avail [14]int8
	for r := 1; r <= 13; r++ {
		avail[r] = 4
	}
	avail[o.Starter.Rank()]--
	var seq [8]int8
	ns, count := 0, int8(0)
	var f [nPegFeat]float64
	for _, pc := range o.Played[:o.PlayedN] {
		r := int8(pc.Card.Rank())
		if count+rankValue(r) > 31 {
			count, ns = 0, 0
		}
		if pc.Player != o.Player {
			op := oppPlay{rank: r, count: count, lead: count == 0}
			for h := int8(1); h <= 13; h++ {
				if count+rankValue(h) > 31 {
					continue
				}
				pegPublic(seq[:ns], count, &avail, h, &f)
				op.pub[h] = s.pegPol.dot(&f)
				op.pts[h] = f[pfPoints]
			}
			s.oppPlays = append(s.oppPlays, op)
		}
		avail[r]--
		seq[ns] = r
		ns++
		count += rankValue(r)
		if count == 31 {
			count, ns = 0, 0
		}
	}
}

// pegPolicyLik is the chance under the fitted policy that the opponent,
// holding this world's ranks, played the cards they did this hand.
// prepPegPolicy must have run for this observation.
func (s *searcher) pegPolicyLik(w world) float64 {
	var hand [8]int8
	nh := copy(hand[:], w.ranks[:w.n])
	for _, op := range s.oppPlays {
		hand[nh] = op.rank
		nh++
	}
	lik := 1.0
	var f [nPegFeat]float64
	var rest [4]int8
	for _, op := range s.oppPlays {
		if op.lead {
			f[pfLead] = 1
		} else {
			f[pfLead] = 0
		}
		var z, chosen, zs, chosenS float64
		var tried uint16
		for i, h := range hand[:nh] {
			if tried&(1<<h) != 0 || op.count+rankValue(h) > 31 {
				continue
			}
			tried |= 1 << h
			nr := copy(rest[:], hand[:i])
			nr += copy(rest[nr:], hand[i+1:nh])
			pegPrivate(rest[:nr], op.count, h, &f)
			e := math.Exp(op.pub[h] + s.pegPol.dotPrivate(&f))
			z += e
			if h == op.rank {
				chosen = e
			}
			if s.pegMix > 0 {
				es := math.Exp(op.pts[h] / s.pegTemp)
				zs += es
				if h == op.rank {
					chosenS = es
				}
			}
		}
		if s.pegMix > 0 {
			lik *= (1-s.pegMix)*chosen/z + s.pegMix*chosenS/zs
		} else {
			lik *= chosen / z
		}
		for i := range hand[:nh] {
			if hand[i] == op.rank {
				hand[i] = hand[nh-1]
				nh--
				break
			}
		}
	}
	return lik
}

// PegDecision is one pegging choice from a corpus: the features of each
// distinct rank the player could play and which they played.
type PegDecision struct {
	F      [][nPegFeat]float64
	Chosen int
}

// PegDecisions extracts the pegging choices of the given seats (a bit per
// seat) from corpus hands; choices with one playable rank are skipped.
func PegDecisions(recs []HandRecord, seats uint8) []PegDecision {
	var out []PegDecision
	for i := range recs {
		r := &recs[i]
		var held [2][]int8
		for p := 0; p < 2; p++ {
			for _, c := range r.Kept(p) {
				held[p] = append(held[p], int8(c.Rank()))
			}
		}
		var avail [14]int8
		for k := 1; k <= 13; k++ {
			avail[k] = 4
		}
		avail[r.Starter.Rank()]--
		var seq [8]int8
		ns, count := 0, int8(0)
		for _, pl := range r.Plays[:r.PlayN] {
			c, p := engine.Card(pl&0x7f), int(pl>>7)
			rk := int8(c.Rank())
			if count+rankValue(rk) > 31 {
				count, ns = 0, 0
			}
			if seats&(1<<p) != 0 {
				var d PegDecision
				var tried uint16
				var rest [4]int8
				for j, h := range held[p] {
					if tried&(1<<h) != 0 || count+rankValue(h) > 31 {
						continue
					}
					tried |= 1 << h
					var f [nPegFeat]float64
					pegPublic(seq[:ns], count, &avail, h, &f)
					nr := copy(rest[:], held[p][:j])
					nr += copy(rest[nr:], held[p][j+1:])
					pegPrivate(rest[:nr], count, h, &f)
					if h == rk {
						d.Chosen = len(d.F)
					}
					d.F = append(d.F, f)
				}
				if len(d.F) > 1 {
					out = append(out, d)
				}
			}
			if j := slices.Index(held[p], rk); j >= 0 {
				held[p] = slices.Delete(held[p], j, j+1)
			}
			avail[rk]--
			seq[ns] = rk
			ns++
			count += rankValue(rk)
			if count == 31 {
				count, ns = 0, 0
			}
		}
	}
	return out
}

// LogLik is the policy's mean log-likelihood per decision.
func (p *PegPolicy) LogLik(ds []PegDecision) float64 {
	var ll float64
	for _, d := range ds {
		ll += p.logLik1(d)
	}
	return ll / float64(len(ds))
}

func (p *PegPolicy) logLik1(d PegDecision) float64 {
	m := math.Inf(-1)
	var sc [13]float64
	for j := range d.F {
		sc[j] = p.dot(&d.F[j])
		m = max(m, sc[j])
	}
	var z float64
	for j := range d.F {
		z += math.Exp(sc[j] - m)
	}
	return sc[d.Chosen] - m - math.Log(z)
}

// FitPegPolicy fits the policy to the decisions by maximum likelihood
// (Newton's method, a light ridge penalty l2 per decision), starting from
// start.
func FitPegPolicy(ds []PegDecision, start PegPolicy, l2 float64, iters int) PegPolicy {
	p := start
	n := float64(len(ds))
	for it := 0; it < iters; it++ {
		var g [nPegFeat]float64
		var h [nPegFeat][nPegFeat]float64
		for _, d := range ds {
			var sc [13]float64
			m := math.Inf(-1)
			for j := range d.F {
				sc[j] = p.dot(&d.F[j])
				m = max(m, sc[j])
			}
			var z float64
			for j := range d.F {
				sc[j] = math.Exp(sc[j] - m)
				z += sc[j]
			}
			var mean [nPegFeat]float64
			for j := range d.F {
				q := sc[j] / z
				for a := range mean {
					mean[a] += q * d.F[j][a]
				}
			}
			for a := range g {
				g[a] += d.F[d.Chosen][a] - mean[a]
			}
			for j := range d.F {
				q := sc[j] / z
				for a := 0; a < nPegFeat; a++ {
					da := d.F[j][a] - mean[a]
					if da == 0 {
						continue
					}
					for b := 0; b <= a; b++ {
						h[a][b] += q * da * (d.F[j][b] - mean[b])
					}
				}
			}
		}
		// Solve (H + l2·n·I) Δ = g - l2·n·p.
		var A [nPegFeat][nPegFeat + 1]float64
		for a := 0; a < nPegFeat; a++ {
			for b := 0; b <= a; b++ {
				A[a][b], A[b][a] = h[a][b], h[a][b]
			}
			A[a][a] += l2 * n
			A[a][nPegFeat] = g[a] - l2*n*p[a]
		}
		delta := solve(&A)
		var step float64
		for a := range p {
			p[a] += delta[a]
			step = max(step, math.Abs(delta[a]))
		}
		if step < 1e-6 {
			break
		}
	}
	return p
}

// solve does Gaussian elimination with partial pivoting on an augmented
// matrix.
func solve(A *[nPegFeat][nPegFeat + 1]float64) [nPegFeat]float64 {
	const n = nPegFeat
	for c := 0; c < n; c++ {
		piv := c
		for r := c + 1; r < n; r++ {
			if math.Abs(A[r][c]) > math.Abs(A[piv][c]) {
				piv = r
			}
		}
		A[c], A[piv] = A[piv], A[c]
		if A[c][c] == 0 {
			continue
		}
		for r := c + 1; r < n; r++ {
			k := A[r][c] / A[c][c]
			for j := c; j <= n; j++ {
				A[r][j] -= k * A[c][j]
			}
		}
	}
	var x [n]float64
	for r := n - 1; r >= 0; r-- {
		if A[r][r] == 0 {
			continue
		}
		s := A[r][n]
		for j := r + 1; j < n; j++ {
			s -= A[r][j] * x[j]
		}
		x[r] = s / A[r][r]
	}
	return x
}
