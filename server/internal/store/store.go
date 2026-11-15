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

// User is a registered account. There are no passwords: people sign in
// with codes sent to their email address, or with passkeys.
type User struct {
	ID       int64
	Username string // as registered; lookups are case-insensitive
	Nickname string
	Created  time.Time
	Email    string // verified address for sign-in codes; "" only for bots
	// Bot accounts sign in with the server's bot key (for headless
	// clients), never by email.
	Bot bool
}

// Kinds of AccountToken.
const (
	TokenVerify = 2 // a link to a new email address, confirming it
	TokenDelete = 3 // a link confirming that an account should be deleted
	TokenChange = 4 // a link to the current address, confirming a change of address
)

// Passkey is a WebAuthn credential that signs a user in.
type Passkey struct {
	ID       []byte // the credential ID
	UserID   int64
	Name     string // chosen by the user, e.g. "iPhone"
	Data     []byte // the credential, as the auth package encodes it
	Created  time.Time
	LastUsed time.Time // zero if never
}

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
	// CreateUser inserts u, with its email address, and sets u.ID.
	// Returns ErrUsernameTaken if the username exists (case-insensitively).
	CreateUser(ctx context.Context, u *User) error
	UserByUsername(ctx context.Context, username string) (*User, error)
	UserByID(ctx context.Context, id int64) (*User, error)
	// CreateSession starts a session, returning ErrNotFound if the user
	// does not exist (it was deleted while signing in).
	CreateSession(ctx context.Context, token string, userID int64, expires time.Time) error
	// SessionUser returns the user ID and expiry of a valid, unexpired
	// token.
	SessionUser(ctx context.Context, token string) (int64, time.Time, error)
	// ExtendSession moves a session's expiry; an unknown token is not an
	// error.
	ExtendSession(ctx context.Context, token string, expires time.Time) error
	// DeleteSession ends a session; deleting an unknown token is not an
	// error.
	DeleteSession(ctx context.Context, token string) error
	// DeleteSessions ends every session of a user but keep ("" for none).
	DeleteSessions(ctx context.Context, userID int64, keep string) error
	// DeleteExpiredSessions removes sessions and account tokens that
	// expired before t and reports how many sessions.
	DeleteExpiredSessions(ctx context.Context, t time.Time) (int64, error)

	// SetEmail sets a user's verified address and deletes their account
	// tokens. An account given an address is no longer a bot.
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
	// PendingEmail is the address of a user's unexpired token of this
	// kind (TokenVerify or TokenChange), or "".
	PendingEmail(ctx context.Context, userID int64, kind int) (string, error)
	// DeleteUser removes a user with their sessions, account tokens and
	// passkeys. User IDs are never reused.
	DeleteUser(ctx context.Context, userID int64) error

	// AddPasskey stores a new passkey.
	AddPasskey(ctx context.Context, p *Passkey) error
	// Passkeys returns a user's passkeys, oldest first.
	Passkeys(ctx context.Context, userID int64) ([]Passkey, error)
	// UpdatePasskey saves a used passkey's Data and LastUsed.
	UpdatePasskey(ctx context.Context, p *Passkey) error
	// DeletePasskey removes one of a user's passkeys, or returns
	// ErrNotFound.
	DeletePasskey(ctx context.Context, userID int64, id []byte) error
	// DeletePasskeys removes all of a user's passkeys.
	DeletePasskeys(ctx context.Context, userID int64) error

	// Addresses whose owners asked, by the link in an email, for no more
	// email. Each is kept as a keyed hash of its mailbox (mail.Mailbox),
	// so a stop covers the address's +tag variants and reveals no address.
	//
	// StopToken is the token for the stop link in email to an address.
	StopToken(email string) (string, error)
	// StopEmail stops email to a stop token's address. A token the store
	// did not make is ErrNotFound.
	StopEmail(ctx context.Context, token string) error
	// EmailStopped reports whether email to an address was stopped.
	EmailStopped(ctx context.Context, email string) (bool, error)
	// AllowEmail undoes StopEmail for an address and reports whether it
	// was stopped.
	AllowEmail(ctx context.Context, email string) (bool, error)
}

// DeletedNickname replaces a deleted user's nickname in the games they
// played, which stay in their opponents' history.
const DeletedNickname = "Deleted player"

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

	// ForgetUser shows a deleted user as DeletedNickname in their games
	// and removes their invites. Their unfinished games must have ended.
	ForgetUser(ctx context.Context, userID int64) error

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
