package expert

import (
	"bytes"
	"context"
	"math"
	"slices"
	"testing"

	"cribbage/internal/bots"
)

func handsOf(hs ...[]int8) *Hands {
	h := new(Hands)
	for _, s := range hs {
		h.add(0, 0, s)
	}
	return h
}

// If every hand gives the dealer exactly one point, the players take turns
// scoring one point, starting with the dealer, so the dealer wins exactly
// when they need no more points than the pone.
func TestWinTableRace(t *testing.T) {
	w, err := BuildWinTable(handsOf([]int8{1}), 1)
	if err != nil {
		t.Fatal(err)
	}
	for x := 0; x < Scores; x++ {
		for y := 0; y < Scores; y++ {
			want := float32(0)
			if Scores-x <= Scores-y {
				want = 1
			}
			if w.V[x][y] != want {
				t.Fatalf("V(%d,%d) = %v, want %v", x, y, w.V[x][y], want)
			}
		}
	}
}

// When both players would pass 121 in the same hand, the order of the
// scoring decides: here the pone pegs 5 before the dealer pegs 10.
func TestWinTableOrder(t *testing.T) {
	w, err := BuildWinTable(handsOf([]int8{-5, 10}), 1)
	if err != nil {
		t.Fatal(err)
	}
	if v := w.V[115][116]; v != 0 {
		t.Errorf("V(115,116) = %v, want 0 (pone reaches 121 first)", v)
	}
	if v := w.V[115][115]; v != 1 {
		t.Errorf("V(115,115) = %v, want 1 (pone falls one short)", v)
	}
	if got := w.Win(116, 115, false); got != 1 {
		t.Errorf("Win(116,115, pone) = %v, want 1", got)
	}
}

func TestWinTableAverages(t *testing.T) {
	// Half the hands win outright for the dealer from 100-100, half for the
	// pone.
	w, err := BuildWinTable(handsOf([]int8{30, -1}, []int8{-30, 1}), 2)
	if err != nil {
		t.Fatal(err)
	}
	if v := w.V[100][100]; v != 0.5 {
		t.Errorf("V(100,100) = %v, want 0.5", v)
	}
	if n, worst := w.Monotonicity(); n != 0 {
		t.Errorf("%d monotonicity violations (worst %v)", n, worst)
	}
}

func TestBundleRoundTrip(t *testing.T) {
	w, _ := BuildWinTable(handsOf([]int8{3, -2, 7}, []int8{-12, 4}), 2)
	b := &Bundle{Meta: Meta{Source: "test", Games: 3, Metrics: map[string]float64{"x": 1}}, Win: w}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Source != "test" || got.Meta.Version != BundleVersion || got.Win.V != w.V {
		t.Errorf("round trip changed the bundle: %+v", got.Meta)
	}
	if _, err := ReadBundle(bytes.NewReader(buf.Bytes()[:buf.Len()-1])); err == nil {
		t.Error("truncated bundle read without error")
	}
}

func TestCollect(t *testing.T) {
	heur := func(int) bots.Bot { return bots.NewHeuristic() }
	a, err := Collect(context.Background(), heur, 50, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Collect(context.Background(), heur, 50, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Hands.steps, b.Hands.steps) || a.Outcomes != b.Outcomes {
		t.Fatal("results depend on the number of workers")
	}
	var starts, dealerWins int64
	for x := range a.Outcomes.N {
		for y := range a.Outcomes.N[x] {
			starts += a.Outcomes.N[x][y]
			dealerWins += a.Outcomes.DealerWins[x][y]
		}
	}
	if a.Outcomes.N[0][0] != 50 {
		t.Errorf("%d hand starts at 0-0, want one per game", a.Outcomes.N[0][0])
	}
	if int64(a.Hands.Len()) != starts {
		t.Errorf("%d hands recorded from %d hand starts", a.Hands.Len(), starts)
	}
	ended := 0
	for i := 0; i < a.Hands.Len(); i++ {
		x, y := a.Hands.From(i)
		res, x2, y2 := Replay(a.Hands.Hand(i), x, y)
		if res != 0 {
			ended++
		} else if x2 == x && y2 == y || x2-x > 70 || y2-y > 70 {
			t.Fatalf("hand %d goes from %d-%d to %d-%d", i, x, y, x2, y2)
		}
	}
	if ended != 50 {
		t.Errorf("%d hands end a game, want one per game", ended)
	}
	if dealerWins == 0 || dealerWins == starts {
		t.Errorf("dealer won %d of %d hand starts", dealerWins, starts)
	}
}

// Each start is valued with the hands that began nearest it. Hands from
// 0-0 give the pone 1 point and hands from 100-100 give the dealer 30.
// With windows of 10 hands, 110-110 sees only the hands from 100-100;
// with windows of 20 it also sees those from 0-0.
func TestWinTableWindows(t *testing.T) {
	h := new(Hands)
	for i := 0; i < 10; i++ {
		h.add(0, 0, []int8{-1})
		h.add(100, 100, []int8{30})
	}
	w, err := BuildWinTable(h, 10)
	if err != nil {
		t.Fatal(err)
	}
	if v := w.V[110][110]; v != 1 {
		t.Errorf("windows of 10: V(110,110) = %v, want 1", v)
	}
	if w, _ = BuildWinTable(h, 20); w.V[110][110] >= 1 {
		t.Errorf("windows of 20: V(110,110) = %v, want below 1", w.V[110][110])
	}
}

// Small windows over noisy hands still give a monotone table.
func TestWinTableMonotoneUnderNoise(t *testing.T) {
	h := new(Hands)
	for x := 0; x < Scores; x += 3 {
		for y := 0; y < Scores; y += 3 {
			for i := 0; i < 4; i++ {
				s := (x*7 + y*13 + i*29) % 23
				h.add(x, y, []int8{int8(1 + s%9), int8(-(1 + s/3))})
			}
		}
	}
	w, err := BuildWinTable(h, 4)
	if err != nil {
		t.Fatal(err)
	}
	if n, worst := w.Monotonicity(); n != 0 {
		t.Errorf("%d monotonicity violations (worst %v)", n, worst)
	}
}

// My chance from one seat and the opponent's from the other add up to 1.
func TestWinComplementary(t *testing.T) {
	w, _ := BuildWinTable(handsOf([]int8{3, -2, 7}, []int8{-12, 4}, []int8{-1, 9, -5}), 1)
	for _, s := range [][2]int{{0, 0}, {37, 90}, {119, 120}, {121, 50}, {60, 125}} {
		for _, deal := range []bool{true, false} {
			if sum := w.Win(s[0], s[1], deal) + w.Win(s[1], s[0], !deal); math.Abs(sum-1) > 1e-6 {
				t.Errorf("Win(%d,%d,%v) + Win(%d,%d,%v) = %v", s[0], s[1], deal, s[1], s[0], !deal, sum)
			}
		}
	}
}
