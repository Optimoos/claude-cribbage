// Package engine implements the rules of two-player cribbage as a pure,
// deterministic state machine. It performs no I/O and is safe to use from
// many goroutines as long as each Game is owned by a single goroutine.
package engine

import (
	"fmt"
	"strings"
)

// Card is a playing card encoded as suit*13 + (rank-1), in the range [0, 52).
type Card uint8

// NoCard marks an absent or hidden card.
const NoCard Card = 255

// Suit of a card.
type Suit uint8

const (
	Clubs Suit = iota
	Diamonds
	Hearts
	Spades
)

const (
	rankChars = "A23456789TJQK"
	suitChars = "CDHS"
)

var (
	rankTable  [52]uint8
	valueTable [52]uint8
)

func init() {
	for i := 0; i < 52; i++ {
		r := i%13 + 1
		rankTable[i] = uint8(r)
		valueTable[i] = uint8(min(r, 10))
	}
}

// NewCard builds a card from a rank (1=Ace .. 13=King) and suit.
func NewCard(rank int, s Suit) Card { return Card(int(s)*13 + rank - 1) }

// Rank returns 1 (Ace) through 13 (King).
func (c Card) Rank() int { return int(rankTable[c]) }

// Suit returns the card's suit.
func (c Card) Suit() Suit { return Suit(c / 13) }

// Value returns the counting value: face cards are 10, Ace is 1.
func (c Card) Value() int { return int(valueTable[c]) }

// Valid reports whether c is a real card.
func (c Card) Valid() bool { return c < 52 }

func (c Card) String() string {
	if !c.Valid() {
		return "--"
	}
	return string([]byte{rankChars[c.Rank()-1], suitChars[c.Suit()]})
}

// ParseCard parses strings like "AS", "TH", "10H", "kd".
func ParseCard(s string) (Card, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if strings.HasPrefix(s, "10") {
		s = "T" + s[2:]
	}
	if len(s) != 2 {
		return NoCard, fmt.Errorf("invalid card %q", s)
	}
	r := strings.IndexByte(rankChars, s[0])
	su := strings.IndexByte(suitChars, s[1])
	if r < 0 || su < 0 {
		return NoCard, fmt.Errorf("invalid card %q", s)
	}
	return NewCard(r+1, Suit(su)), nil
}

// MustParseCards parses a space-separated list of cards and panics on error.
// Intended for tests and tooling.
func MustParseCards(s string) []Card {
	fields := strings.Fields(s)
	out := make([]Card, len(fields))
	for i, f := range fields {
		c, err := ParseCard(f)
		if err != nil {
			panic(err)
		}
		out[i] = c
	}
	return out
}

// sortKey orders cards by rank, then suit, for display.
func (c Card) sortKey() int { return c.Rank()*4 + int(c.Suit()) }

func sortCards(cs []Card) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].sortKey() < cs[j-1].sortKey(); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}
