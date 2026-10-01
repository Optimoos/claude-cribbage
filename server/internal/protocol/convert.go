// Package protocol converts between engine types and wire messages.
package protocol

import (
	"fmt"

	"cribbage/internal/engine"
	"cribbage/internal/protocol/pb"
)

func cardsToPB(cs []engine.Card) []uint32 {
	out := make([]uint32, 0, len(cs))
	for _, c := range cs {
		if c.Valid() {
			out = append(out, uint32(c))
		}
	}
	return out
}

func cardFromPB(v uint32) (engine.Card, error) {
	if v >= 52 {
		return engine.NoCard, fmt.Errorf("invalid card %d", v)
	}
	return engine.Card(v), nil
}

func optCard(c engine.Card) *uint32 {
	if !c.Valid() {
		return nil
	}
	v := uint32(c)
	return &v
}

func scores(s [2]int) []int32 { return []int32{int32(s[0]), int32(s[1])} }

// ActionToPB converts an engine action.
func ActionToPB(a engine.Action) *pb.Action {
	switch a.Kind {
	case engine.ActDiscard:
		return &pb.Action{Kind: pb.ActionKind_ACTION_KIND_DISCARD, Cards: cardsToPB(a.Cards[:])}
	case engine.ActPlay:
		return &pb.Action{Kind: pb.ActionKind_ACTION_KIND_PLAY, Cards: cardsToPB(a.Cards[:1])}
	}
	return &pb.Action{}
}

// ActionsToPB converts a list of engine actions.
func ActionsToPB(as []engine.Action) []*pb.Action {
	out := make([]*pb.Action, len(as))
	for i, a := range as {
		out[i] = ActionToPB(a)
	}
	return out
}

// ActionFromPB validates and converts a wire action. Legality is checked by
// the engine, not here.
func ActionFromPB(a *pb.Action) (engine.Action, error) {
	if a == nil {
		return engine.Action{}, fmt.Errorf("missing action")
	}
	switch a.Kind {
	case pb.ActionKind_ACTION_KIND_DISCARD:
		if len(a.Cards) != 2 {
			return engine.Action{}, fmt.Errorf("discard needs 2 cards, got %d", len(a.Cards))
		}
		c0, err := cardFromPB(a.Cards[0])
		if err != nil {
			return engine.Action{}, err
		}
		c1, err := cardFromPB(a.Cards[1])
		if err != nil {
			return engine.Action{}, err
		}
		return engine.Discard(c0, c1), nil
	case pb.ActionKind_ACTION_KIND_PLAY:
		if len(a.Cards) != 1 {
			return engine.Action{}, fmt.Errorf("play needs 1 card, got %d", len(a.Cards))
		}
		c, err := cardFromPB(a.Cards[0])
		if err != nil {
			return engine.Action{}, err
		}
		return engine.Play(c), nil
	}
	return engine.Action{}, fmt.Errorf("unknown action kind %v", a.Kind)
}

func phaseToPB(p engine.Phase) pb.Phase { return pb.Phase(p + 1) }

// ObservationToPB converts a player's observation.
func ObservationToPB(o *engine.Observation) *pb.Observation {
	played := make([]*pb.PlayedCard, o.PlayedN)
	for i, pc := range o.Played[:o.PlayedN] {
		played[i] = &pb.PlayedCard{Card: uint32(pc.Card), Seat: int32(pc.Player)}
	}
	return &pb.Observation{
		Seat:         int32(o.Player),
		Phase:        phaseToPB(o.Phase),
		Dealer:       int32(o.Dealer),
		Turn:         int32(o.Turn),
		MustAct:      o.MustAct,
		HandNumber:   int32(o.HandNum),
		Scores:       scores(o.Scores),
		Winner:       int32(o.Winner),
		Hand:         cardsToPB(o.HandCards()),
		Discarded:    o.Discarded,
		OppDiscarded: o.OppDiscarded,
		Kept:         cardsToPB(o.Kept[:]),
		Discards:     cardsToPB(o.Discards[:]),
		Starter:      optCard(o.Starter),
		Count:        int32(o.Count),
		Played:       played,
		SeqStart:     int32(o.SeqStart),
		OppHandSize:  int32(o.OppHandN),
	}
}

func fillCards(dst []engine.Card, src []uint32) (int, error) {
	if len(src) > len(dst) {
		return 0, fmt.Errorf("too many cards: %d", len(src))
	}
	for i := range dst {
		dst[i] = engine.NoCard
	}
	for i, v := range src {
		c, err := cardFromPB(v)
		if err != nil {
			return 0, err
		}
		dst[i] = c
	}
	return len(src), nil
}

// ObservationFromPB converts a wire observation back into an engine
// observation, for clients that run engine-based bots.
func ObservationFromPB(p *pb.Observation) (engine.Observation, error) {
	o := engine.Observation{
		Player:       int(p.Seat),
		Phase:        engine.Phase(max(p.Phase, 1) - 1),
		Dealer:       int(p.Dealer),
		Turn:         int(p.Turn),
		MustAct:      p.MustAct,
		HandNum:      int(p.HandNumber),
		Winner:       int(p.Winner),
		Discarded:    p.Discarded,
		OppDiscarded: p.OppDiscarded,
		Starter:      engine.NoCard,
		Count:        int(p.Count),
		SeqStart:     int(p.SeqStart),
		OppHandN:     int(p.OppHandSize),
	}
	if len(p.Scores) == 2 {
		o.Scores = [2]int{int(p.Scores[0]), int(p.Scores[1])}
	}
	var err error
	if o.HandN, err = fillCards(o.Hand[:], p.Hand); err != nil {
		return o, err
	}
	if _, err = fillCards(o.Kept[:], p.Kept); err != nil {
		return o, err
	}
	if _, err = fillCards(o.Discards[:], p.Discards); err != nil {
		return o, err
	}
	if p.Starter != nil {
		if o.Starter, err = cardFromPB(*p.Starter); err != nil {
			return o, err
		}
	}
	if len(p.Played) > len(o.Played) {
		return o, fmt.Errorf("too many played cards: %d", len(p.Played))
	}
	for i := range o.Played {
		o.Played[i] = engine.PlayedCard{Card: engine.NoCard, Player: -1}
	}
	for i, pc := range p.Played {
		c, err := cardFromPB(pc.Card)
		if err != nil {
			return o, err
		}
		o.Played[i] = engine.PlayedCard{Card: c, Player: int(pc.Seat)}
	}
	o.PlayedN = len(p.Played)
	if o.SeqStart < 0 || o.SeqStart > o.PlayedN {
		return o, fmt.Errorf("invalid seq_start %d", o.SeqStart)
	}
	return o, nil
}

// EventToPB converts an engine event.
func EventToPB(e *engine.Event) *pb.Event {
	out := &pb.Event{
		Kind:   pb.EventKind(e.Kind),
		Seat:   int32(e.Player),
		Card:   optCard(e.Card),
		Count:  int32(e.Count),
		Points: int32(e.Points),
		Scores: scores(e.Scores),
		Skunk:  int32(e.Skunk),
	}
	switch e.Kind {
	case engine.EvCutForDeal:
		out.Cards = cardsToPB(e.Cards[:2])
	case engine.EvShowHand, engine.EvShowCrib:
		out.Cards = cardsToPB(e.Cards[:])
		out.Show = &pb.ShowScore{
			Fifteens: int32(e.Show.Fifteens), Pairs: int32(e.Show.Pairs), Runs: int32(e.Show.Runs),
			Flush: int32(e.Show.Flush), Nobs: int32(e.Show.Nobs),
		}
	case engine.EvPlay:
		out.Peg = &pb.PegScore{
			Fifteen: int32(e.Peg.Fifteen), ThirtyOne: int32(e.Peg.ThirtyOne),
			Pairs: int32(e.Peg.Pairs), Run: int32(e.Peg.Run),
		}
	}
	return out
}

// AppendEvents converts events and appends them to dst.
func AppendEvents(dst []*pb.Event, evs []engine.Event) []*pb.Event {
	for i := range evs {
		dst = append(dst, EventToPB(&evs[i]))
	}
	return dst
}
