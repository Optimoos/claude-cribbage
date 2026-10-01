package protocol

import (
	"math/rand/v2"
	"testing"

	"cribbage/internal/engine"
	"cribbage/internal/protocol/pb"
)

func TestObservationRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for seed := uint64(0); seed < 200; seed++ {
		g := engine.NewGame(seed)
		for !g.Over() {
			for p := 0; p < 2; p++ {
				o := g.Observation(p)
				back, err := ObservationFromPB(ObservationToPB(&o))
				if err != nil {
					t.Fatal(err)
				}
				if back != o {
					t.Fatalf("seed %d: round trip mismatch\n got %+v\nwant %+v", seed, back, o)
				}
			}
			p := 0
			if !g.ToAct(0) {
				p = 1
			}
			legal := g.Legal(p)
			a := legal[r.IntN(len(legal))]
			back, err := ActionFromPB(ActionToPB(a))
			if err != nil || back != a {
				t.Fatalf("action %v round trip: %v %v", a, back, err)
			}
			if err := g.Apply(p, back); err != nil {
				t.Fatal(err)
			}
			for _, e := range g.Events() {
				if pe := EventToPB(&e); pe.Kind == pb.EventKind_EVENT_KIND_UNSPECIFIED {
					t.Fatalf("event %v converted to unspecified", e)
				}
			}
		}
	}
}

func TestActionFromPBRejectsGarbage(t *testing.T) {
	bad := []*pb.Action{
		nil,
		{},
		{Kind: pb.ActionKind_ACTION_KIND_PLAY},
		{Kind: pb.ActionKind_ACTION_KIND_PLAY, Cards: []uint32{52}},
		{Kind: pb.ActionKind_ACTION_KIND_DISCARD, Cards: []uint32{1}},
		{Kind: pb.ActionKind_ACTION_KIND_DISCARD, Cards: []uint32{1, 99}},
		{Kind: 77, Cards: []uint32{1}},
	}
	for _, a := range bad {
		if _, err := ActionFromPB(a); err == nil {
			t.Errorf("accepted %v", a)
		}
	}
}

func TestEventKindsMatch(t *testing.T) {
	// The wire enum mirrors the engine's numbering.
	pairs := map[engine.EventKind]pb.EventKind{
		engine.EvCutForDeal: pb.EventKind_EVENT_KIND_CUT_FOR_DEAL,
		engine.EvPlay:       pb.EventKind_EVENT_KIND_PLAY,
		engine.EvShowCrib:   pb.EventKind_EVENT_KIND_SHOW_CRIB,
		engine.EvGameOver:   pb.EventKind_EVENT_KIND_GAME_OVER,
	}
	for e, p := range pairs {
		if pb.EventKind(e) != p {
			t.Errorf("%v != %v", e, p)
		}
	}
}
