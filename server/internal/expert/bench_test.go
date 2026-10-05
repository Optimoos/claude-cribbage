package expert

import (
	"testing"

	"cribbage/internal/bots"
)

func BenchmarkExpertGame(b *testing.B) {
	bundle, err := LoadBundle("../../models-expert/expert-m13.bin")
	if err != nil {
		b.Skip(err)
	}
	bundle.keepModel()
	x := NewBot(bundle, 400)
	h := bots.NewHeuristic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bots.PlayGame(uint64(i), [2]bots.Bot{x, h}, nil)
	}
}
