package expert

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
	"slices"
	"sync"
)

const (
	bundleMagic = "CRIBEX01"
	// BundleVersion changes whenever a table's meaning or layout changes;
	// only bundles of this version are read.
	BundleVersion = 2
)

// Table describes one table stored in a bundle: little-endian float32
// values in row-major order.
type Table struct {
	Name  string `json:"name"`
	Shape []int  `json:"shape"`
}

func (t Table) size() int {
	n := 1
	for _, d := range t.Shape {
		n *= d
	}
	return n
}

// Meta is a bundle's JSON header.
type Meta struct {
	Version int                `json:"version"`
	Saved   string             `json:"saved"`  // RFC 3339 time
	Source  string             `json:"source"` // the bot whose self-play built the tables
	Games   int                `json:"games"`
	Hands   int                `json:"hands"`
	Seed    uint64             `json:"seed"`
	Tables  []Table            `json:"tables"`
	Sources map[string]string  `json:"sources,omitempty"` // table name -> the bot whose self-play built it
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Notes   string             `json:"notes,omitempty"`
}

// Bundle is the expert's model: every learned table plus its metadata.
// Discard is nil in a bundle built with only a win table (a first step
// when building one); the bot then discards as the heuristic does.
type Bundle struct {
	Meta    Meta
	Win     *WinTable
	Discard *DiscardTables
	// KeepTemp is the fitted temperature of the opponent discard model by
	// side; zero when the bundle has none (the search then weights
	// opponent hands only by card counts).
	KeepTemp [2]float64

	keepOnce sync.Once
	keep     *keepModel
}

// keepModel returns the opponent discard model, built on first use, or nil.
func (b *Bundle) keepModel() *keepModel {
	b.keepOnce.Do(func() {
		if b.Discard != nil && b.KeepTemp[0] > 0 && b.KeepTemp[1] > 0 {
			b.keep = newKeepModel(b.Discard, b.KeepTemp)
		}
	})
	return b.keep
}

// tables lists the bundle's tables in file order with their data.
func (b *Bundle) tables() ([]Table, [][]float32) {
	win := make([]float32, 0, Scores*Scores)
	for x := range b.Win.V {
		win = append(win, b.Win.V[x][:]...)
	}
	ts := []Table{{Name: "win", Shape: winShape}}
	data := [][]float32{win}
	if d := b.Discard; d != nil {
		ts = append(ts, Table{"crib", cribShape}, Table{"peg", pegShape}, Table{"show", showShape})
		data = append(data, d.Crib, d.Peg, d.Show)
	}
	if b.KeepTemp[0] > 0 {
		ts = append(ts, Table{"keep", keepShape})
		data = append(data, []float32{float32(b.KeepTemp[0]), float32(b.KeepTemp[1])})
	}
	return ts, data
}

var (
	winShape  = []int{Scores, Scores}
	keepShape = []int{2}
)

// Write encodes the bundle: magic, a little-endian uint32 header length,
// the JSON header, then each table listed in the header.
func (b *Bundle) Write(w io.Writer) error {
	meta := b.Meta
	meta.Version = BundleVersion
	var data [][]float32
	meta.Tables, data = b.tables()
	hdr, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	bw.WriteString(bundleMagic)
	binary.Write(bw, binary.LittleEndian, uint32(len(hdr)))
	bw.Write(hdr)
	buf := make([]byte, 4)
	for _, vals := range data {
		for _, v := range vals {
			binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
			bw.Write(buf)
		}
	}
	return bw.Flush()
}

// ReadBundle decodes a bundle written by Write.
func ReadBundle(r io.Reader) (*Bundle, error) {
	br := bufio.NewReader(r)
	m := make([]byte, len(bundleMagic))
	if _, err := io.ReadFull(br, m); err != nil {
		return nil, err
	}
	if string(m) != bundleMagic {
		return nil, errors.New("expert: not a bundle file")
	}
	var hlen uint32
	if err := binary.Read(br, binary.LittleEndian, &hlen); err != nil {
		return nil, err
	}
	if hlen > 1<<20 {
		return nil, errors.New("expert: header too large")
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, err
	}
	b := &Bundle{}
	if err := json.Unmarshal(hdr, &b.Meta); err != nil {
		return nil, fmt.Errorf("expert: header: %w", err)
	}
	if b.Meta.Version != BundleVersion {
		return nil, fmt.Errorf("expert: bundle version %d; this build reads version %d", b.Meta.Version, BundleVersion)
	}
	want := map[string][]int{"win": winShape, "crib": cribShape, "peg": pegShape, "show": showShape, "keep": keepShape}
	d := new(DiscardTables)
	nd := 0
	buf := make([]byte, 4)
	for _, t := range b.Meta.Tables {
		vals := make([]float32, t.size())
		for i := range vals {
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, fmt.Errorf("expert: table %s: %w", t.Name, err)
			}
			vals[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
		}
		shape, ok := want[t.Name]
		if !ok {
			return nil, fmt.Errorf("expert: unknown table %q", t.Name)
		}
		if !slices.Equal(t.Shape, shape) {
			return nil, fmt.Errorf("expert: %s table shape %v, want %v", t.Name, t.Shape, shape)
		}
		switch t.Name {
		case "win":
			b.Win = new(WinTable)
			for x := range b.Win.V {
				copy(b.Win.V[x][:], vals[x*Scores:])
			}
		case "crib":
			d.Crib, nd = vals, nd+1
		case "peg":
			d.Peg, nd = vals, nd+1
		case "show":
			d.Show, nd = vals, nd+1
		case "keep":
			b.KeepTemp = [2]float64{float64(vals[0]), float64(vals[1])}
		}
	}
	if b.Win == nil {
		return nil, errors.New("expert: bundle has no win table")
	}
	switch nd {
	case 3:
		b.Discard = d
	case 0:
	default:
		return nil, errors.New("expert: bundle has only some of the discard tables")
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, errors.New("expert: trailing data after tables")
	}
	return b, nil
}

// Save writes the bundle to path atomically (via a temporary file).
func (b *Bundle) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := b.Write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadBundle reads a bundle file.
func LoadBundle(path string) (*Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadBundle(f)
}
