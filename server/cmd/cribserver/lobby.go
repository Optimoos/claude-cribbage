package main

import (
	"slices"
	"strings"

	"cribbage/internal/bots"
)

// Lobby is what /api/lobby serves: the bots to offer, in order, and how to
// show every bot the server has, including ones in old games and history
// that are no longer offered.
type Lobby struct {
	Offer []string    `json:"offer"`
	Bots  []bots.Info `json:"bots"`
}

// lobbyInfo builds the lobby from a comma-separated list of bot names,
// dropping names that are not registered.
func lobbyInfo(list string) Lobby {
	have := bots.Names()
	l := Lobby{Offer: []string{}, Bots: bots.Infos()}
	for _, n := range strings.Split(list, ",") {
		n = strings.TrimSpace(n)
		if n != "" && slices.Contains(have, n) && !slices.Contains(l.Offer, n) {
			l.Offer = append(l.Offer, n)
		}
	}
	return l
}
