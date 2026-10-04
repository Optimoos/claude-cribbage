package expert

import (
	"math"
	"sort"
)

// Monotonicity counts violations of "more points for me never hurts":
// V must rise with the dealer's score and fall with the pone's. Worst is
// the largest violation found.
func (t *WinTable) Monotonicity() (violations int, worst float64) {
	for x := 0; x < Scores; x++ {
		for y := 0; y < Scores; y++ {
			v := float64(t.V[x][y])
			if x+1 < Scores {
				if d := v - float64(t.V[x+1][y]); d > 0 {
					violations++
					worst = max(worst, d)
				}
			}
			if y+1 < Scores {
				if d := float64(t.V[x][y+1]) - v; d > 0 {
					violations++
					worst = max(worst, d)
				}
			}
		}
	}
	return violations, worst
}

// Bin compares the table with observed results over a square of hand-start
// scores.
type Bin struct {
	X, Y      int // lowest dealer and pone scores in the bin
	N         int64
	Predicted float64 // the table's average over the bin's hand starts
	Observed  float64 // the dealer's observed win fraction
	Z         float64 // (observed - predicted) in standard errors
}

// Calibration groups hand starts into size×size squares of scores and
// compares predicted and observed dealer win rates in every square holding
// at least minN starts. It also returns the mean log loss over every start
// and that of always predicting the overall win rate, for scale.
func (t *WinTable) Calibration(o *Outcomes, size int, minN int64) (bins []Bin, logLoss, baseLoss float64) {
	nb := (Scores + size - 1) / size
	var total, wins int64
	for bx := 0; bx < nb; bx++ {
		for by := 0; by < nb; by++ {
			var b Bin
			b.X, b.Y = bx*size, by*size
			var pred float64
			var w int64
			for x := b.X; x < min(b.X+size, Scores); x++ {
				for y := b.Y; y < min(b.Y+size, Scores); y++ {
					n := o.N[x][y]
					if n == 0 {
						continue
					}
					v := clampP(float64(t.V[x][y]))
					k := o.DealerWins[x][y]
					b.N += n
					w += k
					pred += float64(n) * v
					logLoss -= float64(k)*math.Log(v) + float64(n-k)*math.Log(1-v)
				}
			}
			total += b.N
			wins += w
			if b.N < minN || b.N == 0 {
				continue
			}
			b.Predicted = pred / float64(b.N)
			b.Observed = float64(w) / float64(b.N)
			b.Z = (b.Observed - b.Predicted) / math.Sqrt(clampP(b.Predicted)*(1-clampP(b.Predicted))/float64(b.N))
			bins = append(bins, b)
		}
	}
	if total == 0 {
		return bins, 0, 0
	}
	p := clampP(float64(wins) / float64(total))
	baseLoss = -(float64(wins)*math.Log(p) + float64(total-wins)*math.Log(1-p)) / float64(total)
	return bins, logLoss / float64(total), baseLoss
}

// WorstBins returns the k bins with the largest |Z|.
func WorstBins(bins []Bin, k int) []Bin {
	s := append([]Bin(nil), bins...)
	sort.Slice(s, func(i, j int) bool { return math.Abs(s[i].Z) > math.Abs(s[j].Z) })
	return s[:min(k, len(s))]
}

func clampP(p float64) float64 { return min(max(p, 1e-6), 1-1e-6) }
