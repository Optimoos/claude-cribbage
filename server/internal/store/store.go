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
	Email        string // verified address for password reset links; "" if none
	// MustChange is set when the admin resets the password: the user must
	// choose a new one before doing anything else.
	MustChange bool
}

// Kinds of AccountToken.
const (
	TokenReset  = 1 // a password reset link
	TokenVerify = 2 // a link confirming an email address
)

// AccountToken is a single-use secret sent in a link by email. Only a hash
// of the token is stored.
type AccountToken struct {
	Token   string
	UserID  int64
	Kind    int
	Email   string // the address being verified (TokenVerify)
	Expires time.Time
}

// Store persists users and sessions. Implementations must be safe for
// concurrent use.
type Store interface {
	// CreateUser inserts u and sets u.ID. Returns ErrUsernameTaken if the
	// username exists (case-insensitively).
	CreateUser(ctx context.Context, u *User) error
	UserByUsername(ctx context.Context, username string) (*User, error)
	UserByID(ctx context.Context, id int64) (*User, error)
	// CreateSession starts a session for a user whose password hash is
	// still hash, returning ErrNotFound if it is not: a password checked
	// before a reset cannot start a session after it.
	CreateSession(ctx context.Context, token string, userID int64, hash []byte, expires time.Time) error
	// SessionUser returns the user ID for a valid, unexpired token.
	SessionUser(ctx context.Context, token string) (int64, error)
	// DeleteSession ends a session; deleting an unknown token is not an
	// error.
	DeleteSession(ctx context.Context, token string) error
	// DeleteExpiredSessions removes sessions and account tokens that
	// expired before t and reports how many sessions.
	DeleteExpiredSessions(ctx context.Context, t time.Time) (int64, error)

	// SetPassword replaces a user's password hash and MustChange flag, and
	// deletes their reset and verification tokens and every session but
	// keep ("" for none), in one transaction.
	SetPassword(ctx context.Context, userID int64, hash []byte, mustChange bool, keep string) error
	// SetEmail sets a user's verified address ("" to remove it), and
	// deletes their verification and reset tokens.
	SetEmail(ctx context.Context, userID int64, email string) error
	// UsersByEmail returns the users with this verified address
	// (case-insensitively).
	UsersByEmail(ctx context.Context, email string) ([]*User, error)
	// CreateAccountToken stores a token. A user has at most one token of a
	// kind: an earlier one is replaced.
	CreateAccountToken(ctx context.Context, t *AccountToken) error
	// TakeAccountToken deletes an unexpired token of the given kind and
	// returns it, or ErrNotFound.
	TakeAccountToken(ctx context.Context, token string, kind int) (*AccountToken, error)
	// PendingEmail is the address of a user's unexpired verification
	// token, or "".
	PendingEmail(ctx context.Context, userID int64) (string, error)
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
	// History returns a page of a user's finished games that had an
	// opponent, newest first: those before cursor (zero for the first
	// page), ordered by Updated then ID.
	History(ctx context.Context, userID int64, before Cursor, limit int) ([]Game, error)
	// Records counts a user's finished games against each opponent.
	Records(ctx context.Context, userID int64) ([]Record, error)
	// LoadGame returns an unfinished game with its actions, to wake a game
	// that was put to sleep.
	LoadGame(ctx context.Context, id string) (*GameWithActions, error)

	CreateInvite(ctx context.Context, inv *Invite) error
	DeleteInvite(ctx context.Context, id string) error
	// LoadInvites returns every stored invite, expired or not.
	LoadInvites(ctx context.Context) ([]Invite, error)
}

// Cursor is a position in a user's history: the game with this Updated
// time and ID.
type Cursor struct {
	Updated time.Time
	ID      string
}

// Record counts a user's finished games against one opponent: a person
// (Opponent.UserID) or a bot (Opponent.Bot). Opponent.Nickname is from the
// latest game.
type Record struct {
	Opponent   Seat
	Games      int
	Wins       int
	Losses     int
	Unfinished int // no winner: expired or cancelled
	Last       time.Time
}

// Invite is a pending invitation to a game, from one user to another or,
// with a code and no To, to whoever enters the code.
type Invite struct {
	ID       string
	From     int64
	FromName string // nickname when sent
	To       int64  // 0 for a code invite
	ToName   string
	Code     string // "" unless a code invite
	Created  time.Time
	Expires  time.Time
}
