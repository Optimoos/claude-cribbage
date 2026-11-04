// Package auth handles accounts and signing in. There are no passwords:
// people sign in with a code or link mailed to them (signin.go), or with a
// passkey (passkey.go). Headless bots sign in with the server's bot key.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/webauthn"

	"cribbage/internal/mail"
	"cribbage/internal/ratelimit"
	"cribbage/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidInput       = errors.New("invalid input")
	ErrMailDisabled       = errors.New("this server cannot send email")
	ErrBadToken           = errors.New("this link or code has expired or has already been used")
	ErrMailLimit          = errors.New("too many emails for this account or address; try again later")
	ErrTooManyCodes       = errors.New("too many wrong codes for this account; try again later, or use the link in the email")
	ErrPasskeysDisabled   = errors.New("this server does not offer passkeys")
)

// credError is an ErrInvalidCredentials with its own message.
type credError string

func (e credError) Error() string        { return string(e) }
func (e credError) Is(target error) bool { return target == ErrInvalidCredentials }

var (
	// ErrWrongCode is a wrong sign-in code; it is an ErrInvalidCredentials.
	ErrWrongCode error = credError("that code is wrong; check it, or use the link in the email")
	// ErrBadPasskey is a passkey that does not sign anyone in; it is an
	// ErrInvalidCredentials.
	ErrBadPasskey error = credError("that passkey was not recognized")
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// Service registers and authenticates users.
type Service struct {
	Store store.Store
	// SessionTTL is how long a session lasts without being used; using it
	// (Resume) renews it. 90 days if zero.
	SessionTTL time.Duration

	// Mailer sends sign-in codes and links; nil means nobody can sign in
	// by email. BaseURL is the web client's address, for the links.
	Mailer    mail.Sender
	BaseURL   string
	VerifyTTL time.Duration // how long an address confirmation link works; 48 hours if zero
	DeleteTTL time.Duration // how long an account deletion link works; an hour if zero

	// BotKey lets headless clients sign in to bot accounts (BotLogin);
	// empty disables it.
	BotKey string

	// WebAuthn offers passkeys; nil disables them (see NewWebAuthn).
	WebAuthn *webauthn.WebAuthn

	initOnce  sync.Once
	mailLimit *ratelimit.Limiter // emails per user
	rcptLimit *ratelimit.Limiter // emails per address
	badCodes  *ratelimit.Limiter // wrong codes per account

	mu         sync.Mutex
	challenges map[string]*challenge
	tickets    map[int64]*ticket  // each user's current sign-in code
	links      map[string]*ticket // by link token
	swept      time.Time
}

func (s *Service) init() {
	s.initOnce.Do(func() {
		// Ten emails per account, then one every five minutes; twenty per
		// address (accounts can share one), then one every five minutes.
		// Plenty for someone asking for a code again or changing their
		// address, but nobody can use the server to flood an inbox.
		s.mailLimit = ratelimit.New(10, 5*time.Minute)
		s.rcptLimit = ratelimit.New(20, 5*time.Minute)
		// Ten wrong codes per account, then one an hour: with 8-digit codes,
		// guessing one would take about 10^8 / 2 / 24 / 365 = 5000 years.
		s.badCodes = ratelimit.New(10, time.Hour)
		s.challenges = map[string]*challenge{}
		s.tickets = map[int64]*ticket{}
		s.links = map[string]*ticket{}
	})
}

// Result of a successful authentication.
type Result struct {
	User  *store.User
	Token string
}

// ValidUsername reports whether s could be a username.
func ValidUsername(s string) bool { return usernameRE.MatchString(s) }

func validNickname(nickname string) error {
	if n := utf8.RuneCountInString(nickname); n < 1 || n > 24 {
		return fmt.Errorf("%w: nickname must be 1-24 characters", ErrInvalidInput)
	}
	for _, r := range nickname {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: nickname contains unprintable characters", ErrInvalidInput)
		}
	}
	return nil
}

func validUsername(username string) error {
	if !usernameRE.MatchString(username) {
		return fmt.Errorf("%w: username must be 3-32 letters, digits, '_' or '-'", ErrInvalidInput)
	}
	return nil
}

// MailEnabled reports whether the service can send email.
func (s *Service) MailEnabled() bool { return s.Mailer != nil }

// CheckEmail reports an address that cannot be used for an account.
func (s *Service) CheckEmail(email string) error {
	if !s.MailEnabled() {
		return ErrMailDisabled
	}
	if !mail.ValidAddress(strings.TrimSpace(email)) {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidInput)
	}
	return nil
}

// mailAllowed spends one of a user's emails (userID 0 for none yet) and
// one of the address's.
func (s *Service) mailAllowed(userID int64, to string) bool {
	s.init()
	now := time.Now()
	if userID != 0 && !s.mailLimit.Take(strconv.FormatInt(userID, 10), now) {
		return false
	}
	return s.rcptLimit.Take(strings.ToLower(to), now)
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL == 0 {
		return 90 * 24 * time.Hour
	}
	return s.SessionTTL
}

// Resume authenticates with an existing session token, renewing the
// session (at most once a day, to spare the database).
func (s *Service) Resume(ctx context.Context, token string) (*Result, error) {
	if !validToken(token) {
		return nil, ErrInvalidCredentials
	}
	id, expires, err := s.Store.SessionUser(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	u, err := s.Store.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	renewed := time.Now().Add(s.sessionTTL())
	if renewed.Sub(expires) > 24*time.Hour {
		if err := s.Store.ExtendSession(ctx, token, renewed); err != nil {
			return nil, err
		}
	}
	return &Result{User: u, Token: token}, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if !validToken(token) {
		return nil
	}
	return s.Store.DeleteSession(ctx, token)
}

// SignOutOthers ends every session of a user but keep.
func (s *Service) SignOutOthers(ctx context.Context, userID int64, keep string) error {
	return s.Store.DeleteSessions(ctx, userID, keep)
}

// Prune deletes expired sessions and reports how many, and forgets expired
// sign-in codes.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	s.init()
	s.mu.Lock()
	s.sweep(time.Now())
	s.mu.Unlock()
	return s.Store.DeleteExpiredSessions(ctx, time.Now())
}

// tokenBytes is the size of a session or link token before hex encoding.
const tokenBytes = 32

// validToken reports whether t could be a token from RandomID(tokenBytes).
func validToken(t string) bool {
	if len(t) != 2*tokenBytes {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// SignIn starts a session for a user.
func (s *Service) SignIn(ctx context.Context, u *store.User) (*Result, error) {
	token := RandomID(tokenBytes)
	err := s.Store.CreateSession(ctx, token, u.ID, time.Now().Add(s.sessionTTL()))
	if errors.Is(err, store.ErrNotFound) {
		// The account was deleted while signing in.
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	return &Result{User: u, Token: token}, nil
}

// BotLogin signs a headless client in to a bot account with the bot key,
// creating the account if there is none with this username.
func (s *Service) BotLogin(ctx context.Context, username, nickname, key string) (*Result, error) {
	if s.BotKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.BotKey)) != 1 {
		return nil, ErrInvalidCredentials
	}
	if err := validUsername(username); err != nil {
		return nil, err
	}
	u, err := s.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		nickname = strings.TrimSpace(nickname)
		if nickname == "" {
			nickname = username
		}
		if err := validNickname(nickname); err != nil {
			return nil, err
		}
		u = &store.User{Username: username, Nickname: nickname, Created: time.Now(), Bot: true}
		err = s.Store.CreateUser(ctx, u)
		if errors.Is(err, store.ErrUsernameTaken) {
			// Another bot client created it meanwhile.
			u, err = s.Store.UserByUsername(ctx, username)
		}
	}
	if err != nil {
		return nil, err
	}
	if !u.Bot {
		return nil, ErrInvalidCredentials // a person's account
	}
	return s.SignIn(ctx, u)
}

// RandomID returns n random bytes, hex encoded.
func RandomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// link is a web client URL for a token: kind is signin, verify or delete.
// The token is in the fragment, which browsers do not send to the server.
func (s *Service) link(kind, token string) string {
	base := s.BaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "#" + kind + "=" + token
}

func duration(d time.Duration) string {
	switch h := int(d.Hours()); {
	case h == 1:
		return "an hour"
	case h > 1:
		return fmt.Sprintf("%d hours", h)
	}
	if m := int(d.Minutes()); m > 0 {
		return fmt.Sprintf("%d minutes", m)
	}
	return d.String()
}
