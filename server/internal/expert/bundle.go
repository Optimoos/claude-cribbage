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
	// KeepExact, when not nil, is the share of each six-rank shape's
	// options that the real discarder keeps, by side, in the keep model's
	// option order (see BuildExactKeep). The opponent model then mixes it
	// with the softmax.
	KeepExact *[2][]float32
	// PegPolicy, when not nil, is the fitted opponent pegging policy
	// (proposal 2) that weights worlds by the plays made in them.
	PegPolicy *PegPolicy
	// PegPair, when not nil, is each pair of kept shapes' mean pegging
	// (proposal 4), for discards conditioned on the opponent's hand.
	PegPair *PegPairTable

	keepMu sync.Mutex
	keeps  map[float64]*keepModel // by mixing weight; -1 for the softmax alone
}

// DefaultKeepEps is the softmax's weight in the opponent model when the
// bundle has an exact keep table.
const DefaultKeepEps = 0.5

// keepModel returns the opponent discard model with the default mix,
// built on first use, or nil.
func (b *Bundle) keepModel() *keepModel { return b.keepModelMix(DefaultKeepEps, false) }

// keepModelMix returns the opponent discard model with the softmax at
// weight eps against the exact table (or alone with softmaxOnly or
// without one), built on first use; nil when the bundle has no model.
func (b *Bundle) keepModelMix(eps float64, softmaxOnly bool) *keepModel {
	if b.Discard == nil || b.KeepTemp[0] <= 0 || b.KeepTemp[1] <= 0 {
		return nil
	}
	exact := b.KeepExact
	if softmaxOnly || exact == nil {
		eps, exact = -1, nil
	}
	b.keepMu.Lock()
	defer b.keepMu.Unlock()
	if m := b.keeps[eps]; m != nil {
		return m
	}
	if b.keeps == nil {
		b.keeps = map[float64]*keepModel{}
	}
	m := newKeepModel(b.Discard, b.KeepTemp, exact, eps)
	b.keeps[eps] = m
	return m
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
	if b.KeepExact != nil {
		ts = append(ts, Table{"keepx", keepxShape()})
		data = append(data, append(append([]float32{}, b.KeepExact[0]...), b.KeepExact[1]...))
	}
	if p := b.PegPolicy; p != nil {
		ts = append(ts, Table{"pegpol", pegPolicyShape})
		v := make([]float32, nPegFeat)
		for i, w := range p {
			v[i] = float32(w)
		}
		data = append(data, v)
	}
	if p := b.PegPair; p != nil {
		ts = append(ts, Table{"pegpair", pegPairShape})
		data = append(data, append(append([]float32{}, p.Dealer...), p.Pone...))
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
	want := map[string][]int{"win": winShape, "crib": cribShape, "peg": pegShape, "show": showShape, "keep": keepShape, "keepx": keepxShape(), "pegpol": pegPolicyShape, "pegpair": pegPairShape}
	d := new(DiscardTables)
	nd := 0
	buf := make([]byte, 4)
	seen := map[string]bool{}
	for _, t := range b.Meta.Tables {
		// Checked before allocating: the shape comes from the file.
		shape, ok := want[t.Name]
		if !ok {
			return nil, fmt.Errorf("expert: unknown table %q", t.Name)
		}
		if !slices.Equal(t.Shape, shape) {
			return nil, fmt.Errorf("expert: %s table shape %v, want %v", t.Name, t.Shape, shape)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("expert: table %s is listed twice", t.Name)
		}
		seen[t.Name] = true
		vals := make([]float32, t.size())
		for i := range vals {
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, fmt.Errorf("expert: table %s: %w", t.Name, err)
			}
			vals[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
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
		case "keepx":
			n := len(vals) / 2
			b.KeepExact = &[2][]float32{vals[:n], vals[n:]}
		case "pegpair":
			n := len(vals) / 2
			b.PegPair = &PegPairTable{Dealer: vals[:n], Pone: vals[n:]}
		case "pegpol":
			b.PegPolicy = new(PegPolicy)
			for i, v := range vals {
				b.PegPolicy[i] = float64(v)
			}
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

// Check reports whether the bundle has what opt asks for: the shape-pair
// table for CondPeg, and the pegging policy for an IS search with λ below
// 1 (without it the search would quietly run with λ 1).
func (b *Bundle) Check(opt Options) error {
	if opt.CondPeg && b.PegPair == nil {
		return fmt.Errorf("condpeg needs a bundle with a shape-pair pegging table")
	}
	if opt.ISLambdaSet && opt.ISLambda < 1 && (b.PegPolicy == nil || opt.NoPegPolicy || opt.PegTemp != 0) {
		return fmt.Errorf("islambda below 1 needs the bundle's pegging policy")
	}
	return nil
}
