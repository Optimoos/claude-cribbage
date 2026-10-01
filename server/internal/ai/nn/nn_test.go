package nn

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"
)

func randSparse(r *rand.Rand, in int) Sparse {
	var x Sparse
	for i := 0; i < in; i++ {
		if r.Float64() < 0.3 {
			x.Set(i, float32(r.NormFloat64()))
		}
	}
	return x
}

func loss(n *Net, x Sparse, y float32) float64 {
	p := float64(n.Predict(x, n.NewScratch()))
	return -(float64(y)*math.Log(p) + (1-float64(y))*math.Log(1-p))
}

// TestGradientCheck compares Backward with central finite differences on
// every parameter of a small network.
func TestGradientCheck(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	n, err := New([]int{7, 6, 5, 1}, r)
	if err != nil {
		t.Fatal(err)
	}
	for l := range n.Layers {
		for i := range n.Layers[l].B {
			n.Layers[l].B[i] = float32(r.NormFloat64() * 0.1) // avoid dead ReLUs at exactly 0
		}
	}
	x := randSparse(r, 7)
	x.Set(0, 0.7) // make sure input 0 is active
	const y = 0.3
	g := n.NewGrad()
	n.Backward(x, y, n.NewScratch(), g)

	const h = 1e-2
	check := func(name string, params []float32, grad []float32) {
		for i := range params {
			orig := params[i]
			params[i] = orig + h
			up := loss(n, x, y)
			params[i] = orig - h
			down := loss(n, x, y)
			params[i] = orig
			num := (up - down) / (2 * h)
			if diff := math.Abs(num - float64(grad[i])); diff > 2e-3+0.02*math.Abs(num) {
				t.Errorf("%s[%d]: analytic %.5f, numeric %.5f", name, i, grad[i], num)
			}
		}
	}
	for l := range n.Layers {
		check("W", n.Layers[l].W, g.W[l])
		check("B", n.Layers[l].B, g.B[l])
	}
}

// TestLearnsXOR checks that training actually reduces loss on a nonlinear
// problem.
func TestLearnsXOR(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	n, _ := New([]int{2, 16, 1}, r)
	opt := NewAdam(n, 0.02)
	data := []struct {
		x Sparse
		y float32
	}{
		{Sparse{}, 0},
		{Sparse{Idx: []int32{0}, Val: []float32{1}}, 1},
		{Sparse{Idx: []int32{1}, Val: []float32{1}}, 1},
		{Sparse{Idx: []int32{0, 1}, Val: []float32{1, 1}}, 0},
	}
	s, g := n.NewScratch(), n.NewGrad()
	for step := 0; step < 2000; step++ {
		g.Zero()
		for _, d := range data {
			n.Backward(d.x, d.y, s, g)
		}
		opt.Step(n, g)
	}
	for _, d := range data {
		if p := n.Predict(d.x, s); math.Abs(float64(p-d.y)) > 0.1 {
			t.Errorf("XOR %v: predicted %.3f, want %.0f", d.x.Idx, p, d.y)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	n, _ := New([]int{20, 8, 4, 1}, r)
	var buf bytes.Buffer
	if err := Write(&buf, n, Meta{EncodingVersion: 3, Games: 42, Metrics: map[string]float64{"x": 0.5}}); err != nil {
		t.Fatal(err)
	}
	m, meta, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if meta.EncodingVersion != 3 || meta.Games != 42 || meta.Metrics["x"] != 0.5 || len(meta.Sizes) != 4 {
		t.Fatalf("meta %+v", meta)
	}
	s := n.NewScratch()
	for i := 0; i < 100; i++ {
		x := randSparse(r, 20)
		if a, b := n.Predict(x, s), m.Predict(x, s); a != b {
			t.Fatalf("prediction differs after reload: %v vs %v", a, b)
		}
	}
	if _, _, err := Read(bytes.NewReader(buf.Bytes()[:buf.Len()-1])); err == nil {
		t.Fatal("truncated file accepted")
	}
	if _, _, err := Read(bytes.NewReader(append(buf.Bytes(), 0))); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func BenchmarkPredict(b *testing.B) {
	r := rand.New(rand.NewPCG(7, 8))
	n, _ := New([]int{457, 128, 64, 1}, r)
	var x Sparse
	for i := 0; i < 45; i++ {
		x.Set(r.IntN(457), 1)
	}
	s := n.NewScratch()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n.Predict(x, s)
	}
}

func BenchmarkBackward(b *testing.B) {
	r := rand.New(rand.NewPCG(7, 8))
	n, _ := New([]int{457, 128, 64, 1}, r)
	var x Sparse
	for i := 0; i < 45; i++ {
		x.Set(r.IntN(457), 1)
	}
	s, g := n.NewScratch(), n.NewGrad()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n.Backward(x, 0.5, s, g)
	}
}
