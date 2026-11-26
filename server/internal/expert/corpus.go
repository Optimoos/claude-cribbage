package expert

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"

	"cribbage/internal/ai"
	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// HandRecord is one hand in full, by game seat (0 and 1). A corpus of them
// keeps everything later tables and models may need: both deals, the
// discards, the starter, the plays in order, every scoring step and the
// game's result.
type HandRecord struct {
	Game     uint32 // index of the game in its corpus file
	Hand     uint8  // 1 for the game's first hand (or first simulated hand)
	Dealer   uint8  // seat
	Flags    uint8  // FlagExplore0, FlagExplore1, FlagSynthetic
	Winner   int8   // seat that won the game
	Start    [2]uint8
	End      [2]uint8 // scores at the end of the hand (or when the game ended in it)
	Six      [2][6]engine.Card
	Discards [2][2]engine.Card
	Starter  engine.Card
	PlayN    uint8
	Plays    [8]uint8 // card | seat<<7, in order
	StepN    uint8
	Steps    [20]int8 // scoring steps in order: points for the dealer positive, for the pone negative
	Show     [2]uint8 // hands' shows by seat
	Crib     uint8
	Peg      [2]uint8 // pegging points by seat
}

// Hand record flags.
const (
	FlagExplore0  = 1 << iota // seat 0 discarded at random this hand
	FlagExplore1              // seat 1 discarded at random this hand
	FlagSynthetic             // the game started from chosen scores, not 0-0
)

// Complete reports whether the hand ran to its crib (no win part way).
func (r *HandRecord) Complete() bool { return r.Crib != noCrib }

const noCrib = 255

// Kept returns the four cards seat p kept.
func (r *HandRecord) Kept(p int) [4]engine.Card {
	var k [4]engine.Card
	n := 0
	for _, c := range r.Six[p] {
		if c != r.Discards[p][0] && c != r.Discards[p][1] {
			k[n] = c
			n++
		}
	}
	return k
}

// CorpusMeta describes how a corpus file was made.
type CorpusMeta struct {
	Bots     [2]string `json:"bots"` // seat 0 and seat 1 (seats swap per game in the deal, not here)
	Seed     uint64    `json:"seed"`
	Games    int       `json:"games"`
	Explore  float64   `json:"explore"`             // chance per hand and seat of a random discard
	StartMin int       `json:"start_min,omitempty"` // synthetic starts: each score uniform in [StartMin, StartMax]
	StartMax int       `json:"start_max,omitempty"`
	Saved    string    `json:"saved"`
}

const corpusMagic = "CRIBCORP1\n"

// SaveCorpus writes meta and the records.
func SaveCorpus(path string, meta CorpusMeta, recs []HandRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	head, err := json.Marshal(meta)
	if err != nil {
		f.Close()
		return err
	}
	w.WriteString(corpusMagic)
	w.Write(head)
	w.WriteByte('\n')
	if err := binary.Write(w, binary.LittleEndian, recs); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadCorpus reads a file written by SaveCorpus.
func LoadCorpus(path string) (CorpusMeta, []HandRecord, error) {
	var meta CorpusMeta
	f, err := os.Open(path)
	if err != nil {
		return meta, nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	magic := make([]byte, len(corpusMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != corpusMagic {
		return meta, nil, fmt.Errorf("%s: not a corpus file", path)
	}
	head, err := r.ReadBytes('\n')
	if err != nil {
		return meta, nil, err
	}
	if err := json.Unmarshal(head, &meta); err != nil {
		return meta, nil, err
	}
	var recs []HandRecord
	for {
		var h HandRecord
		err := binary.Read(r, binary.LittleEndian, &h)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return meta, nil, err
		}
		if !h.valid() {
			return meta, nil, fmt.Errorf("%s: corrupt record %d", path, len(recs))
		}
		recs = append(recs, h)
	}
	return meta, recs, nil
}

// valid reports whether the record's values are in range, so a corrupt
// file is refused rather than indexing out of bounds later. A show or crib
// of noCrib was never scored (the game ended first).
func (h *HandRecord) valid() bool {
	ok := h.Dealer < 2 && (h.Winner == 0 || h.Winner == 1 || h.Winner == -1) && h.PlayN <= 8 && h.StepN <= 20 && h.Starter < 52 && (h.Crib < maxShow || h.Crib == noCrib)
	for _, p := range h.Plays[:min(h.PlayN, 8)] {
		ok = ok && p&0x7f < 52
	}
	for s := range 2 {
		ok = ok && h.Start[s] <= engine.WinScore && h.End[s] <= engine.WinScore && (h.Show[s] < maxShow || h.Show[s] == noCrib) && h.Peg[s] <= engine.WinScore
		for _, c := range append(h.Six[s][:], h.Discards[s][:]...) {
			ok = ok && c < 52
		}
	}
	return ok
}

// CollectCorpus plays meta.Games games between bots made by newA (seat 0)
// and newB (seat 1) and records every hand. Game i deals from
// ai.DealSeed(meta.Seed, i), so the result does not depend on workers.
// With meta.StartMax > 0 every game starts at random scores in
// [StartMin, StartMax] (and a random dealer), to cover the endgame.
func CollectCorpus(ctx context.Context, newA, newB func(worker int) bots.Bot, meta CorpusMeta, workers int) ([]HandRecord, error) {
	workers = max(workers, 1)
	perGame := make([][]HandRecord, meta.Games)
	var next atomic.Int64
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			players := [2]bots.Bot{newA(w), newB(w)}
			for ctx.Err() == nil {
				i := next.Add(1) - 1
				if i >= int64(meta.Games) {
					return
				}
				recs, err := playCorpusGame(uint32(i), ai.DealSeed(meta.Seed, i), players, meta)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				perGame[i] = recs
			}
		}(w)
	}
	wg.Wait()
	var all []HandRecord
	for _, g := range perGame {
		all = append(all, g...)
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return all, firstErr
}

// playCorpusGame plays one game and returns its hands.
func playCorpusGame(id uint32, seed uint64, players [2]bots.Bot, meta CorpusMeta) ([]HandRecord, error) {
	rng := rand.New(rand.NewPCG(seed, 0xc0c0))
	var g *engine.Game
	flags := uint8(0)
	if meta.StartMax > 0 {
		var deck [52]engine.Card
		for i := range deck {
			deck[i] = engine.Card(i)
		}
		rng.Shuffle(52, func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
		var hands [2][6]engine.Card
		copy(hands[0][:], deck[0:6])
		copy(hands[1][:], deck[6:12])
		span := meta.StartMax - meta.StartMin + 1
		scores := [2]int{meta.StartMin + rng.IntN(span), meta.StartMin + rng.IntN(span)}
		g = engine.NewHand(seed, rng.IntN(2), scores, hands, deck[12])
		g.SetQuiet(false)
		flags = FlagSynthetic
	} else {
		g = engine.NewGame(seed)
	}

	var recs []HandRecord
	var cur *HandRecord
	prev := g.Scores()
	begin := func() {
		recs = append(recs, HandRecord{Game: id, Hand: uint8(len(recs) + 1), Dealer: uint8(g.Dealer()), Flags: flags})
		cur = &recs[len(recs)-1]
		cur.Crib = noCrib
		sc := g.Scores()
		cur.Start = [2]uint8{uint8(sc[0]), uint8(sc[1])}
		for p := 0; p < 2; p++ {
			copy(cur.Six[p][:], g.Hand(p))
			if meta.Explore > 0 && rng.Float64() < meta.Explore {
				cur.Flags |= FlagExplore0 << p
			}
		}
	}
	end := func() {
		if cur != nil {
			cur.End = [2]uint8{uint8(prev[0]), uint8(prev[1])}
		}
	}
	handle := func(evs []engine.Event) {
		for _, e := range evs {
			if e.Kind == engine.EvDeal {
				end()
				prev = e.Scores
				begin()
				continue
			}
			if cur == nil {
				continue
			}
			switch e.Kind {
			case engine.EvStarter:
				cur.Starter = e.Card
			case engine.EvPlay:
				if cur.PlayN < 8 {
					cur.Plays[cur.PlayN] = uint8(e.Card) | uint8(e.Player)<<7
					cur.PlayN++
				}
				cur.Peg[e.Player] += uint8(e.Points)
			case engine.EvGoPoint, engine.EvLastCard:
				cur.Peg[e.Player] += uint8(e.Points)
			case engine.EvShowHand:
				cur.Show[e.Player] = uint8(e.Points)
			case engine.EvShowCrib:
				cur.Crib = uint8(e.Points)
			}
			for p := 0; p < 2; p++ {
				if d := e.Scores[p] - prev[p]; d > 0 && cur.StepN < uint8(len(cur.Steps)) {
					if p == int(cur.Dealer) {
						cur.Steps[cur.StepN] = int8(d)
					} else {
						cur.Steps[cur.StepN] = int8(-d)
					}
					cur.StepN++
				}
			}
			prev = e.Scores
			if e.Kind == engine.EvGameOver {
				end()
			}
		}
	}
	if flags&FlagSynthetic != 0 {
		begin() // NewHand deals without an event
	}
	handle(g.Events())
	buf := make([]engine.Action, 0, 16)
	o := new(engine.Observation)
	for !g.Over() {
		for p := 0; p < 2 && !g.Over(); p++ {
			if !g.ToAct(p) {
				continue
			}
			g.ObserveInto(p, o)
			buf = g.AppendLegal(p, buf[:0])
			var a engine.Action
			if o.Phase == engine.PhaseDiscard && cur != nil && cur.Flags&(FlagExplore0<<p) != 0 {
				a = buf[rng.IntN(len(buf))]
			} else {
				a = players[p].Choose(o, buf)
			}
			if err := g.Apply(p, a); err != nil {
				return nil, fmt.Errorf("seed %d: %s (P%d) chose %v: %w", seed, players[p].Name(), p, a, err)
			}
			if a.Kind == engine.ActDiscard && cur != nil {
				cur.Discards[p] = a.Cards
			}
			handle(g.Events())
		}
	}
	for i := range recs {
		recs[i].Winner = int8(g.Winner())
	}
	return recs, nil
}

// CorpusFilter chooses which hands of a corpus go into a SelfPlay.
type CorpusFilter struct {
	NoExplore bool // drop hands where either seat discarded at random
}

// SelfPlay turns corpus hands into the data the table builders take:
// every hand's steps and start (for the win table), the outcome of every
// hand start, and the complete hands that started below DetailMaxStart.
func CorpusSelfPlay(recs []HandRecord, f CorpusFilter) *SelfPlay {
	sp := &SelfPlay{}
	games := map[uint32]bool{}
	for i := range recs {
		r := &recs[i]
		if f.NoExplore && r.Flags&(FlagExplore0|FlagExplore1) != 0 {
			continue
		}
		games[r.Game] = true
		d, p := int(r.Dealer), 1-int(r.Dealer)
		x, y := int(r.Start[d]), int(r.Start[p])
		sp.Hands.add(x, y, r.Steps[:r.StepN])
		sp.Outcomes.N[x][y]++
		if int(r.Winner) == d {
			sp.Outcomes.DealerWins[x][y]++
		}
		if r.Complete() && x < DetailMaxStart && y < DetailMaxStart {
			hs := HandStat{Starter: r.Starter, Crib: int8(r.Crib)}
			for side, seat := range [2]int{d, p} {
				hs.Kept[side] = r.Kept(seat)
				hs.Discards[side] = r.Discards[seat]
				hs.Show[side] = int8(r.Show[seat])
				hs.Peg[side] = int8(r.Peg[seat])
			}
			sp.Details = append(sp.Details, hs)
		}
	}
	sp.Games = len(games)
	return sp
}
