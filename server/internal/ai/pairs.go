package ai

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// PairFile is the per-deal record of one match, saved so later matches on
// the same deals can be compared with it deal by deal (Couple).
type PairFile struct {
	Bot, Opponent string
	Seed          uint64
	Pairs         []Pair // in deal order
}

const pairHeader = "deal,wins,margin,peg,hands,points_a,points_b,peg_a,peg_b,trace"

// Save writes the file as CSV, with the match on comment lines first.
func (f *PairFile) Save(path string) error {
	sort.Slice(f.Pairs, func(i, j int) bool { return f.Pairs[i].Deal < f.Pairs[j].Deal })
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	fmt.Fprintf(w, "# bot %s\n# opponent %s\n# seed %d\n%s\n", f.Bot, f.Opponent, f.Seed, pairHeader)
	for _, p := range f.Pairs {
		fmt.Fprintf(w, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%016x\n", p.Deal, p.WinsA, p.Margin, p.Peg, p.Hands,
			p.PointsA, p.PointsB, p.PegA, p.PegB, p.Trace)
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// LoadPairFile reads a file written by Save.
func LoadPairFile(path string) (*PairFile, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	return readPairFile(in)
}

func readPairFile(r io.Reader) (*PairFile, error) {
	f := &PairFile{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		t := sc.Text()
		switch {
		case strings.HasPrefix(t, "# bot "):
			f.Bot = t[len("# bot "):]
		case strings.HasPrefix(t, "# opponent "):
			f.Opponent = t[len("# opponent "):]
		case strings.HasPrefix(t, "# seed "):
			s, err := strconv.ParseUint(t[len("# seed "):], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", line, err)
			}
			f.Seed = s
		case t == pairHeader || t == "" || strings.HasPrefix(t, "#"):
		default:
			var p Pair
			var trace string
			n, err := fmt.Sscanf(strings.ReplaceAll(t, ",", " "), "%d %d %d %d %d %d %d %d %d %s",
				&p.Deal, &p.WinsA, &p.Margin, &p.Peg, &p.Hands, &p.PointsA, &p.PointsB, &p.PegA, &p.PegB, &trace)
			if err != nil || n != 10 {
				return nil, fmt.Errorf("line %d: %q: %v", line, t, err)
			}
			if p.Trace, err = strconv.ParseUint(trace, 16, 64); err != nil {
				return nil, fmt.Errorf("line %d: %v", line, err)
			}
			f.Pairs = append(f.Pairs, p)
		}
	}
	return f, sc.Err()
}

// Coupled compares a candidate with a baseline that played the same deals
// against the same opponent, deal by deal. A deal both played the same way
// differs by exactly zero, so only deals where the candidate deviated add
// variance, and small changes resolve with far fewer games than comparing
// two independent win rates.
type Coupled struct {
	Pairs, Deviated int // deals compared, and deals whose play differed
	// Per-pair differences (candidate less baseline) and their squares:
	// w in games won (out of 2), m in final margin, p in pegging margin.
	sumW, sumW2, sumM, sumM2, sumP, sumP2 float64
	hands                                 int // hands in both matches
}

// Add records one deal of each match.
func (c *Coupled) Add(cand, base Pair) {
	c.Pairs++
	if cand.Trace != base.Trace {
		c.Deviated++
	}
	w := float64(cand.WinsA - base.WinsA)
	m := float64(cand.Margin - base.Margin)
	p := float64(cand.Peg - base.Peg)
	c.sumW += w
	c.sumW2 += w * w
	c.sumM += m
	c.sumM2 += m * m
	c.sumP += p
	c.sumP2 += p * p
	c.hands += cand.Hands + base.Hands
}

// WinRate returns the change in win rate (a fraction) and its 95%
// half-width.
func (c *Coupled) WinRate() (mean, ci float64) {
	return pairMean(c.sumW/2, c.sumW2/4, c.Pairs)
}

// Margin returns the change in final margin per game and its half-width.
func (c *Coupled) Margin() (mean, ci float64) {
	return pairMean(c.sumM/2, c.sumM2/4, c.Pairs)
}

// PegMargin returns the change in pegging margin per hand and its
// half-width.
func (c *Coupled) PegMargin() (mean, ci float64) {
	if c.Pairs == 0 || c.hands == 0 {
		return 0, 0
	}
	hpp := float64(c.hands) / 2 / float64(c.Pairs) // hands per pair, one match
	m, ci := pairMean(c.sumP, c.sumP2, c.Pairs)
	return m / hpp, ci / hpp
}

// pairMean is the mean of n per-pair values and its 95% half-width.
func pairMean(sum, sumSq float64, n int) (mean, ci float64) {
	if n == 0 {
		return 0, 0
	}
	mean = sum / float64(n)
	return mean, ci95(sumSq, mean, float64(n))
}

// SPRT is a sequential probability ratio test on the mean of per-pair
// values (a win fraction, or a coupled difference): H0 says the mean is
// Mu0, H1 says Mu1. It uses the normal approximation with the variance
// estimated from the data (the GSPRT used by chess engine testing), so it
// suits values that are bounded and many. Alpha and Beta are the error
// rates of wrongly accepting H1 and H0; divide Alpha by the number of
// candidates tried when testing several.
type SPRT struct {
	Mu0, Mu1    float64
	Alpha, Beta float64
	MinPairs    int // no decision before this many pairs, so the variance is settled

	n          int
	sum, sumSq float64
}

// Add records one pair's value.
func (s *SPRT) Add(x float64) {
	s.n++
	s.sum += x
	s.sumSq += x * x
}

// LLR returns the log-likelihood ratio of H1 over H0 so far.
func (s *SPRT) LLR() float64 {
	if s.n < 2 {
		return 0
	}
	n := float64(s.n)
	mean := s.sum / n
	v := (s.sumSq - n*mean*mean) / (n - 1)
	if v <= 0 {
		// Every pair the same: all the evidence is in where the mean lies.
		switch {
		case math.Abs(mean-s.Mu1) < math.Abs(mean-s.Mu0):
			return math.Inf(1)
		case math.Abs(mean-s.Mu1) > math.Abs(mean-s.Mu0):
			return math.Inf(-1)
		}
		return 0
	}
	return (s.Mu1 - s.Mu0) / v * (s.sum - n*(s.Mu0+s.Mu1)/2)
}

// Bounds returns the LLR at which H0 and H1 are accepted.
func (s *SPRT) Bounds() (lower, upper float64) {
	return math.Log(s.Beta / (1 - s.Alpha)), math.Log((1 - s.Beta) / s.Alpha)
}

// Decision returns +1 once H1 is accepted, -1 once H0 is, and 0 while the
// test continues.
func (s *SPRT) Decision() int {
	if s.n < s.MinPairs {
		return 0
	}
	lo, hi := s.Bounds()
	switch llr := s.LLR(); {
	case llr >= hi:
		return 1
	case llr <= lo:
		return -1
	}
	return 0
}

// N returns the number of pairs recorded.
func (s *SPRT) N() int { return s.n }
