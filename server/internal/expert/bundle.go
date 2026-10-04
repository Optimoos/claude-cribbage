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
)

const (
	bundleMagic = "CRIBEX01"
	// BundleVersion changes whenever a table's meaning or layout changes;
	// older bundles are rejected.
	BundleVersion = 1
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
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Notes   string             `json:"notes,omitempty"`
}

// Bundle is the expert's model: every learned table plus its metadata.
type Bundle struct {
	Meta Meta
	Win  *WinTable
}

var winShape = []int{Scores, Scores}

// Write encodes the bundle: magic, a little-endian uint32 header length,
// the JSON header, then each table listed in the header.
func (b *Bundle) Write(w io.Writer) error {
	meta := b.Meta
	meta.Version = BundleVersion
	meta.Tables = []Table{{Name: "win", Shape: winShape}}
	hdr, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	bw.WriteString(bundleMagic)
	binary.Write(bw, binary.LittleEndian, uint32(len(hdr)))
	bw.Write(hdr)
	buf := make([]byte, 4)
	for x := range b.Win.V {
		for _, v := range b.Win.V[x] {
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
	buf := make([]byte, 4)
	for _, t := range b.Meta.Tables {
		vals := make([]float32, t.size())
		for i := range vals {
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, fmt.Errorf("expert: table %s: %w", t.Name, err)
			}
			vals[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
		}
		switch t.Name {
		case "win":
			if !slices.Equal(t.Shape, winShape) {
				return nil, fmt.Errorf("expert: win table shape %v, want %v", t.Shape, winShape)
			}
			b.Win = new(WinTable)
			for x := range b.Win.V {
				copy(b.Win.V[x][:], vals[x*Scores:])
			}
		default:
			return nil, fmt.Errorf("expert: unknown table %q", t.Name)
		}
	}
	if b.Win == nil {
		return nil, errors.New("expert: bundle has no win table")
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
