package nn

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

const magic = "CRIBNN01"

// Meta describes a saved model.
type Meta struct {
	Sizes           []int              `json:"sizes"`
	EncodingVersion int                `json:"encoding_version"`
	Games           int64              `json:"games"` // self-play games trained on
	Steps           int64              `json:"steps"` // optimizer steps
	Saved           string             `json:"saved"` // RFC 3339 time
	Metrics         map[string]float64 `json:"metrics,omitempty"`
	Notes           string             `json:"notes,omitempty"`
}

// Write encodes the model: magic, a little-endian uint32 header length, the
// JSON header, then every layer's W and B as little-endian float32.
func Write(w io.Writer, n *Net, meta Meta) error {
	meta.Sizes = n.Sizes()
	hdr, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	bw.WriteString(magic)
	binary.Write(bw, binary.LittleEndian, uint32(len(hdr)))
	bw.Write(hdr)
	buf := make([]byte, 4)
	for _, l := range n.Layers {
		for _, vs := range [][]float32{l.W, l.B} {
			for _, v := range vs {
				binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
				bw.Write(buf)
			}
		}
	}
	return bw.Flush()
}

// Read decodes a model written by Write.
func Read(r io.Reader) (*Net, Meta, error) {
	var meta Meta
	br := bufio.NewReader(r)
	m := make([]byte, len(magic))
	if _, err := io.ReadFull(br, m); err != nil {
		return nil, meta, err
	}
	if string(m) != magic {
		return nil, meta, errors.New("nn: not a model file")
	}
	var hlen uint32
	if err := binary.Read(br, binary.LittleEndian, &hlen); err != nil {
		return nil, meta, err
	}
	if hlen > 1<<20 {
		return nil, meta, errors.New("nn: header too large")
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, meta, err
	}
	if err := json.Unmarshal(hdr, &meta); err != nil {
		return nil, meta, fmt.Errorf("nn: header: %w", err)
	}
	s := meta.Sizes
	if len(s) < 2 || s[len(s)-1] != 1 {
		return nil, meta, fmt.Errorf("nn: bad sizes %v", s)
	}
	n := &Net{}
	buf := make([]byte, 4)
	for l := 0; l+1 < len(s); l++ {
		layer := Layer{In: s[l], Out: s[l+1], W: make([]float32, s[l]*s[l+1]), B: make([]float32, s[l+1])}
		for _, vs := range [][]float32{layer.W, layer.B} {
			for i := range vs {
				if _, err := io.ReadFull(br, buf); err != nil {
					return nil, meta, fmt.Errorf("nn: weights: %w", err)
				}
				vs[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
			}
		}
		n.Layers = append(n.Layers, layer)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, meta, errors.New("nn: trailing data after weights")
	}
	return n, meta, nil
}

// Save writes the model to path atomically (via a temporary file).
func Save(path string, n *Net, meta Meta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := Write(f, n, meta); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a model file.
func Load(path string) (*Net, Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Meta{}, err
	}
	defer f.Close()
	return Read(f)
}
