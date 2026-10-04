package expert

import (
	"errors"
	"runtime"
	"sync"
)

// WinTable holds V(x, y): the probability that the dealer wins the game
// when a hand starts with the dealer on x points and the pone on y.
type WinTable struct {
	V [Scores][Scores]float32
}

// Win returns the probability that I win from the start of a hand with
// the given scores. Scores of 121 or more mean that player has won.
func (t *WinTable) Win(me, them int, iDeal bool) float64 {
	switch {
	case me >= Scores:
		return 1
	case them >= Scores:
		return 0
	case iDeal:
		return float64(t.V[me][them])
	}
	return 1 - float64(t.V[them][me])
}

// DefaultWindowHands is the default for BuildWinTable's minHands.
const DefaultWindowHands = 5000

// BuildWinTable computes V by dynamic programming over recorded hands.
//
// Play changes with the score, so each start (x, y) is valued with the hands
// that started nearby: those from the smallest window [x-w, x] × [y-w, y]
// holding at least minHands of them (or all hands from below and left of
// (x, y), if fewer). Each hand is replayed in order from (x, y), so the
// table is exact about who reaches 121 first when both players would. The
// window lies below and left of (x, y) so that a hand cut short by the end
// of its game still has a known result from (x, y), and hands are chosen by
// where they started, never by how they went, so the selection is unbiased.
func BuildWinTable(h *Hands, minHands int) (*WinTable, error) {
	n := h.Len()
	if n == 0 {
		return nil, errors.New("expert: no hands to build from")
	}
	minHands = max(minHands, 1)
	// Index hands by start, and count them with 2D prefix sums.
	const S = Scores
	var off [S*S + 1]int32
	for i := 0; i < n; i++ {
		x, y := h.From(i)
		off[x*S+y+1]++
	}
	for k := 1; k <= S*S; k++ {
		off[k] += off[k-1]
	}
	byStart := make([]int32, n)
	fill := off
	for i := 0; i < n; i++ {
		x, y := h.From(i)
		byStart[fill[x*S+y]] = int32(i)
		fill[x*S+y]++
	}
	var pre [S + 1][S + 1]int64 // pre[i][j] = hands from x < i, y < j
	for x := 0; x < S; x++ {
		for y := 0; y < S; y++ {
			pre[x+1][y+1] = pre[x][y+1] + pre[x+1][y] - pre[x][y] + int64(off[x*S+y+1]-off[x*S+y])
		}
	}
	count := func(x0, y0, x, y int) int64 { // hands from [x0, x] × [y0, y]
		return pre[x+1][y+1] - pre[x0][y+1] - pre[x+1][y0] + pre[x0][y0]
	}

	t := new(WinTable)
	// V(x, y) depends on V(y', x') with x'+y' > x+y (every hand scores at
	// least the last card), so fill one diagonal x+y at a time, from the
	// end; the states on a diagonal are independent.
	workers := runtime.GOMAXPROCS(0)
	for sum := 2 * (S - 1); sum >= 0; sum-- {
		lo, hi := max(0, sum-(S-1)), min(sum, S-1)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for x := lo + w; x <= hi; x += workers {
					y := sum - x
					r := 0
					for r < max(x, y) && count(max(x-r, 0), max(y-r, 0), x, y) < int64(minHands) {
						r++
					}
					var v float64
					var k int
					for x0 := max(x-r, 0); x0 <= x; x0++ {
						cell := x0*S + max(y-r, 0)
						for _, i := range byStart[off[cell]:off[x0*S+y+1]] {
							k++
							switch res, x2, y2 := Replay(h.Hand(int(i)), x, y); res {
							case 1:
								v++
							case 0:
								v += 1 - float64(t.V[y2][x2])
							}
						}
					}
					// Sampling noise can break monotonicity slightly, so
					// clamp between the neighbours on the diagonal above,
					// which are already monotone with each other.
					lo, hi := 0.0, 1.0
					if y+1 < S {
						lo = float64(t.V[x][y+1])
					}
					if x+1 < S {
						hi = float64(t.V[x+1][y])
					}
					t.V[x][y] = float32(min(max(v/float64(max(k, 1)), lo), hi))
				}
			}(w)
		}
		wg.Wait()
	}
	return t, nil
}
