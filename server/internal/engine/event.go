package engine

import "fmt"

// EventKind identifies what happened in an Event.
type EventKind uint8

const (
	EvCutForDeal EventKind = iota + 1 // Cards[0] cut by player 0, Cards[1] by player 1
	EvDeal                            // Player is the dealer of a new hand
	EvDiscard                         // Player put two cards in the crib (cards hidden)
	EvStarter                         // Card is the starter; Player (dealer) scored Points for his heels
	EvPlay                            // Player played Card, bringing the count to Count, scoring Peg
	EvGo                              // Player could not play
	EvGoPoint                         // Player scored 1 for the go; count resets
	EvLastCard                        // Player scored 1 for the last card
	EvCountReset                      // The count returned to zero
	EvShowHand                        // Player's hand (Cards) scored Show
	EvShowCrib                        // Dealer's crib (Cards) scored Show
	EvGameOver                        // Player won; Skunk is 0, 1 (skunk) or 2 (double skunk)
)

var eventNames = [...]string{
	EvCutForDeal: "cut-for-deal", EvDeal: "deal", EvDiscard: "discard", EvStarter: "starter",
	EvPlay: "play", EvGo: "go", EvGoPoint: "go-point", EvLastCard: "last-card",
	EvCountReset: "count-reset", EvShowHand: "show-hand", EvShowCrib: "show-crib", EvGameOver: "game-over",
}

func (k EventKind) String() string {
	if int(k) < len(eventNames) && eventNames[k] != "" {
		return eventNames[k]
	}
	return fmt.Sprintf("event(%d)", k)
}

// Event describes one public occurrence in the game. Events never reveal
// private information (hands, discards) before the show.
type Event struct {
	Kind   EventKind
	Player int // -1 when not applicable
	Card   Card
	Cards  [4]Card
	Count  int
	Points int
	Peg    PegScore
	Show   Breakdown
	Scores [2]int // scores after the event
	Skunk  int
}

func (e Event) String() string {
	switch e.Kind {
	case EvCutForDeal:
		return fmt.Sprintf("cut for deal: P0 %v, P1 %v", e.Cards[0], e.Cards[1])
	case EvDeal:
		return fmt.Sprintf("P%d deals", e.Player)
	case EvDiscard:
		return fmt.Sprintf("P%d discards to crib", e.Player)
	case EvStarter:
		if e.Points > 0 {
			return fmt.Sprintf("starter %v, P%d scores %d for his heels", e.Card, e.Player, e.Points)
		}
		return fmt.Sprintf("starter %v", e.Card)
	case EvPlay:
		return fmt.Sprintf("P%d plays %v (count %d) +%d", e.Player, e.Card, e.Count, e.Points)
	case EvGo:
		return fmt.Sprintf("P%d says go", e.Player)
	case EvGoPoint:
		return fmt.Sprintf("P%d +1 for go", e.Player)
	case EvLastCard:
		return fmt.Sprintf("P%d +1 for last card", e.Player)
	case EvCountReset:
		return "count reset"
	case EvShowHand:
		return fmt.Sprintf("P%d shows %v +%d %+v", e.Player, e.Cards, e.Points, e.Show)
	case EvShowCrib:
		return fmt.Sprintf("P%d crib %v +%d %+v", e.Player, e.Cards, e.Points, e.Show)
	case EvGameOver:
		return fmt.Sprintf("P%d wins %d-%d (skunk %d)", e.Player, e.Scores[e.Player], e.Scores[1-e.Player], e.Skunk)
	}
	return e.Kind.String()
}
