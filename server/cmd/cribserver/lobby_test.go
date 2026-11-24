package main

import (
	"slices"
	"testing"
)

func TestLobbyInfo(t *testing.T) {
	l := lobbyInfo(" heuristic2, nosuchbot,random,heuristic2,,heuristic")
	if want := []string{"heuristic2", "random", "heuristic"}; !slices.Equal(l.Offer, want) {
		t.Errorf("offer = %v, want %v", l.Offer, want)
	}
	labels := map[string]string{}
	for _, b := range l.Bots {
		labels[b.Name] = b.Label
	}
	if labels["heuristic2"] != "Heuristic bot (strong)" || labels["random"] != "Random bot" {
		t.Errorf("labels = %v", labels)
	}
	if l := lobbyInfo(""); l.Offer == nil || len(l.Offer) != 0 {
		t.Errorf("empty list gives offer %#v, want an empty slice", l.Offer)
	}
}
