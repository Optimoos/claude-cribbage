package expert

import (
	"testing"

	"cribbage/internal/bots"
)

func BenchmarkISGame(b *testing.B) {
	bundle, err := LoadBundle("../../models-expert/expert.bin")
	if err != nil {
		b.Skip(err)
	}
	bundle.keepModel()
	bots.Prepare()
	x := NewBotWith(bundle, Options{Effort: 400, ISSearch: true})
	h := bots.NewHeuristic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bots.PlayGame(uint64(i), [2]bots.Bot{x, h}, nil)
	}
}
