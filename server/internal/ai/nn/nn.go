// Package nn is a small multilayer perceptron for estimating win
// probability: ReLU hidden layers and a single sigmoid output, trained with
// binary cross-entropy and Adam. Inputs are sparse; layers skip zero inputs,
// which makes both one-hot features and post-ReLU activations cheap.
package nn

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// Sparse is a feature vector given as its non-zero entries.
type Sparse struct {
	Idx []int32
	Val []float32
}

// Reset empties the vector, keeping capacity.
func (s *Sparse) Reset() { s.Idx, s.Val = s.Idx[:0], s.Val[:0] }

// Set appends feature i with value v; zero values are skipped.
func (s *Sparse) Set(i int, v float32) {
	if v != 0 {
		s.Idx = append(s.Idx, int32(i))
		s.Val = append(s.Val, v)
	}
}

// Clone returns an independent copy.
func (s Sparse) Clone() Sparse {
	return Sparse{Idx: append([]int32(nil), s.Idx...), Val: append([]float32(nil), s.Val...)}
}

// Layer is a dense layer. W is stored input-major: W[i*Out+j] connects
// input i to output j, so adding one input's contribution is a contiguous
// slice operation.
type Layer struct {
	In, Out int
	W       []float32
	B       []float32
}

// Net is an MLP: every layer but the last uses ReLU; the last has one
// output passed through a sigmoid.
type Net struct {
	Layers []Layer
}

// New creates a network with the given layer sizes, e.g. [457, 128, 64, 1],
// using He initialization.
func New(sizes []int, rng *rand.Rand) (*Net, error) {
	if len(sizes) < 2 || sizes[len(sizes)-1] != 1 {
		return nil, fmt.Errorf("nn: sizes must have at least 2 entries and end in 1, got %v", sizes)
	}
	n := &Net{}
	for l := 0; l+1 < len(sizes); l++ {
		in, out := sizes[l], sizes[l+1]
		layer := Layer{In: in, Out: out, W: make([]float32, in*out), B: make([]float32, out)}
		std := math.Sqrt(2 / float64(in))
		if l+2 == len(sizes) {
			std = math.Sqrt(1 / float64(in)) // output layer: keep initial logits small
		}
		for i := range layer.W {
			layer.W[i] = float32(rng.NormFloat64() * std)
		}
		n.Layers = append(n.Layers, layer)
	}
	return n, nil
}

// Sizes returns the layer sizes, inputs first.
func (n *Net) Sizes() []int {
	s := []int{n.Layers[0].In}
	for _, l := range n.Layers {
		s = append(s, l.Out)
	}
	return s
}

// Clone returns a deep copy.
func (n *Net) Clone() *Net {
	c := &Net{Layers: make([]Layer, len(n.Layers))}
	for i, l := range n.Layers {
		c.Layers[i] = Layer{In: l.In, Out: l.Out, W: append([]float32(nil), l.W...), B: append([]float32(nil), l.B...)}
	}
	return c
}

// Scratch holds per-goroutine buffers for Predict and Backward. A Net may
// be shared by many goroutines as long as each uses its own Scratch.
type Scratch struct {
	acts   [][]float32 // output of each layer (post-ReLU; the last is the logit)
	deltas [][]float32
}

// NewScratch allocates buffers sized for n.
func (n *Net) NewScratch() *Scratch {
	s := &Scratch{}
	for _, l := range n.Layers {
		s.acts = append(s.acts, make([]float32, l.Out))
		s.deltas = append(s.deltas, make([]float32, l.Out))
	}
	return s
}

func axpy(a float32, x, y []float32) {
	y = y[:len(x)]
	for j, v := range x {
		y[j] += a * v
	}
}

func sigmoid(z float32) float32 {
	return float32(1 / (1 + math.Exp(-float64(z))))
}

// forward computes all activations and returns the output logit.
func (n *Net) forward(x Sparse, s *Scratch) float32 {
	for l, layer := range n.Layers {
		out := s.acts[l]
		copy(out, layer.B)
		if l == 0 {
			for k, i := range x.Idx {
				axpy(x.Val[k], layer.W[int(i)*layer.Out:(int(i)+1)*layer.Out], out)
			}
		} else {
			for i, a := range s.acts[l-1] {
				if a != 0 {
					axpy(a, layer.W[i*layer.Out:(i+1)*layer.Out], out)
				}
			}
		}
		if l < len(n.Layers)-1 {
			for j, v := range out {
				if v < 0 {
					out[j] = 0
				}
			}
		}
	}
	return s.acts[len(n.Layers)-1][0]
}

// Predict returns the network's output probability for x.
func (n *Net) Predict(x Sparse, s *Scratch) float32 {
	return sigmoid(n.forward(x, s))
}

// Grad accumulates gradients with the same shape as a Net.
type Grad struct {
	W, B [][]float32
	N    int // samples accumulated
}

// NewGrad allocates a zero gradient shaped like n.
func (n *Net) NewGrad() *Grad {
	g := &Grad{}
	for _, l := range n.Layers {
		g.W = append(g.W, make([]float32, len(l.W)))
		g.B = append(g.B, make([]float32, len(l.B)))
	}
	return g
}

// Zero clears the gradient.
func (g *Grad) Zero() {
	for l := range g.W {
		clear(g.W[l])
		clear(g.B[l])
	}
	g.N = 0
}

// Add accumulates o into g.
func (g *Grad) Add(o *Grad) {
	for l := range g.W {
		axpy(1, o.W[l], g.W[l])
		axpy(1, o.B[l], g.B[l])
	}
	g.N += o.N
}

// Backward runs a forward pass on x, adds the gradient of the binary
// cross-entropy loss against target (in [0,1]) to g, and returns the loss.
func (n *Net) Backward(x Sparse, target float32, s *Scratch, g *Grad) float32 {
	p := sigmoid(n.forward(x, s))
	last := len(n.Layers) - 1
	s.deltas[last][0] = p - target // d(BCE)/d(logit) for a sigmoid output
	for l := last; l >= 0; l-- {
		layer := n.Layers[l]
		delta := s.deltas[l]
		axpy(1, delta, g.B[l])
		gw := g.W[l]
		if l == 0 {
			for k, i := range x.Idx {
				axpy(x.Val[k], delta, gw[int(i)*layer.Out:(int(i)+1)*layer.Out])
			}
			break
		}
		prev := s.acts[l-1]
		pd := s.deltas[l-1]
		for i, a := range prev {
			if a == 0 { // ReLU was inactive: no gradient flows to or through it
				pd[i] = 0
				continue
			}
			row := layer.W[i*layer.Out : (i+1)*layer.Out]
			axpy(a, delta, gw[i*layer.Out:(i+1)*layer.Out])
			var sum float32
			for j, d := range delta {
				sum += row[j] * d
			}
			pd[i] = sum
		}
	}
	g.N++
	const eps = 1e-7
	pf, tf := float64(p), float64(target)
	return float32(-(tf*math.Log(pf+eps) + (1-tf)*math.Log(1-pf+eps)))
}

// Adam is the Adam optimizer.
type Adam struct {
	LR, Beta1, Beta2, Eps float64
	m, v                  *Grad
	t                     int
}

// NewAdam creates an optimizer for n with learning rate lr.
func NewAdam(n *Net, lr float64) *Adam {
	return &Adam{LR: lr, Beta1: 0.9, Beta2: 0.999, Eps: 1e-8, m: n.NewGrad(), v: n.NewGrad()}
}

// Steps returns how many updates have been applied.
func (a *Adam) Steps() int { return a.t }

// Step applies the mean of the accumulated gradient g to n.
func (a *Adam) Step(n *Net, g *Grad) {
	if g.N == 0 {
		return
	}
	a.t++
	scale := 1 / float64(g.N)
	c1 := 1 - math.Pow(a.Beta1, float64(a.t))
	c2 := 1 - math.Pow(a.Beta2, float64(a.t))
	lr := a.LR * math.Sqrt(c2) / c1
	b1, b2 := float32(a.Beta1), float32(a.Beta2)
	update := func(w, gr, m, v []float32) {
		for i := range w {
			gi := gr[i] * float32(scale)
			m[i] = b1*m[i] + (1-b1)*gi
			v[i] = b2*v[i] + (1-b2)*gi*gi
			w[i] -= float32(lr * float64(m[i]) / (math.Sqrt(float64(v[i])) + a.Eps))
		}
	}
	for l := range n.Layers {
		update(n.Layers[l].W, g.W[l], a.m.W[l], a.v.W[l])
		update(n.Layers[l].B, g.B[l], a.m.B[l], a.v.B[l])
	}
}
