// Package store persists users, sessions and games. Memory is for tests and
// throwaway servers; SQLite survives restarts.
package store

import (
	"context"
	"errors"
	"time"

	"cribbage/internal/engine"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username taken")
)

// User is a registered account.
type User struct {
	ID           int64
	Username     string // as registered; lookups are case-insensitive
	Nickname     string
	PasswordHash []byte
	Created      time.Time
}

// Store persists users and sessions. Implementations must be safe for
// concurrent use.
type Store interface {
	// CreateUser inserts u and sets u.ID. Returns ErrUsernameTaken if the
	// username exists (case-insensitively).
	CreateUser(ctx context.Context, u *User) error
	UserByUsername(ctx context.Context, username string) (*User, error)
	UserByID(ctx context.Context, id int64) (*User, error)
	CreateSession(ctx context.Context, token string, userID int64, expires time.Time) error
	// SessionUser returns the user ID for a valid, unexpired token.
	SessionUser(ctx context.Context, token string) (int64, error)
}

// Game status values; they match the wire protocol's GameStatus.
const (
	StatusWaiting  = 1
	StatusActive   = 2
	StatusFinished = 3
)

// Seat describes who sits in one seat of a game.
type Seat struct {
	UserID   int64 // 0 for bots and empty seats
	Nickname string
	Bot      string // server-side bot name, if any
}

// Game is the persistent header of a game. Its state is rebuilt by
// replaying its actions from Seed.
type Game struct {
	ID       string
	Seed     uint64
	Status   int
	Seats    [2]Seat
	Winner   int    // -1 if none
	Scores   [2]int // final scores once finished
	Resigned bool
	Created  time.Time
	Updated  time.Time
}

// Action is one applied action. Seq starts at 1 and has no gaps.
type Action struct {
	Seq    uint32
	Seat   int
	Action engine.Action
}

// GameWithActions is a game and its full action log.
type GameWithActions struct {
	Game    Game
	Actions []Action
}

// GameStore persists games. Writes for one game are issued sequentially by
// that game's owner.
type GameStore interface {
	CreateGame(ctx context.Context, g *Game) error
	// CommitGame atomically appends acts and, if g is non-nil, updates the
	// game header. It returns once the write is durable.
	CommitGame(ctx context.Context, gameID string, g *Game, acts []Action) error
	// LoadUnfinished returns every waiting or active game with its actions.
	LoadUnfinished(ctx context.Context) ([]GameWithActions, error)
	// RecentGames returns a user's most recently finished games, newest
	// first.
	RecentGames(ctx context.Context, userID int64, limit int) ([]Game, error)
}
