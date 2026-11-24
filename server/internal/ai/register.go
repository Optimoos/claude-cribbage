package ai

import (
	"cribbage/internal/ai/nn"
	"cribbage/internal/bots"
)

// BotName is the name under which Register adds the network player.
const BotName = "nn"

// Load reads a model file and checks that it matches the current encoding.
func Load(path string) (*nn.Net, nn.Meta, error) {
	net, meta, err := nn.Load(path)
	if err != nil {
		return nil, meta, err
	}
	if err := CheckNet(net, meta); err != nil {
		return nil, meta, err
	}
	return net, meta, nil
}

// Register loads a model and makes it available as the "nn" bot. Every bot
// created shares the network's weights and plays greedily.
func Register(path string) (nn.Meta, error) {
	net, meta, err := Load(path)
	if err != nil {
		return meta, err
	}
	bots.Register(BotName, func(seed uint64) bots.Bot { return NewAgent(net, 0, seed) })
	bots.Describe(BotName, bots.Info{Label: "Neural network", Blurb: "Learned by self-play", Icon: "◆"})
	return meta, nil
}
