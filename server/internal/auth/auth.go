// Package auth handles account registration, login and session tokens.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/mail"
	"cribbage/internal/ratelimit"
	"cribbage/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidInput       = errors.New("invalid input")
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// Service registers and authenticates users.
type Service struct {
	Store      store.Store
	BcryptCost int           // bcrypt.DefaultCost if zero
	SessionTTL time.Duration // 30 days if zero

	// Mailer sends reset and verification links; nil disables email.
	// BaseURL is the web client's address, for the links.
	Mailer    mail.Sender
	BaseURL   string
	ResetTTL  time.Duration // how long a reset link works; an hour if zero
	VerifyTTL time.Duration // how long a verification link works; 48 hours if zero

	dummyOnce sync.Once
	dummy     []byte
	mailOnce  sync.Once
	mailLimit *ratelimit.Limiter // emails per user
	rcptLimit *ratelimit.Limiter // emails per address
}

// Result of a successful authentication.
type Result struct {
	User  *store.User
	Token string
}

// ValidUsername reports whether s could be a username.
func ValidUsername(s string) bool { return usernameRE.MatchString(s) }

func validate(username, password, nickname string) error {
	if !usernameRE.MatchString(username) {
		return fmt.Errorf("%w: username must be 3-32 letters, digits, '_' or '-'", ErrInvalidInput)
	}
	if err := validPassword(password); err != nil {
		return err
	}
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

// Register creates an account and a session. If nickname is empty the
// username is used.
func (s *Service) Register(ctx context.Context, username, password, nickname string) (*Result, error) {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = username
	}
	if err := validate(username, password, nickname); err != nil {
		return nil, err
	}
	cost := s.BcryptCost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return nil, err
	}
	u := &store.User{Username: username, Nickname: nickname, PasswordHash: hash, Created: time.Now()}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return s.newSession(ctx, u)
}

// Login checks a password and creates a session.
func (s *Service) Login(ctx context.Context, username, password string) (*Result, error) {
	u, err := s.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// Take as long as a wrong password would, so the time taken does
		// not reveal which usernames exist.
		bcrypt.CompareHashAndPassword(s.dummyHash(), []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword(u.PasswordHash, []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	return s.newSession(ctx, u)
}

// dummyHash is a hash of nothing in particular at the service's cost.
func (s *Service) dummyHash() []byte {
	s.dummyOnce.Do(func() {
		cost := s.BcryptCost
		if cost == 0 {
			cost = bcrypt.DefaultCost
		}
		s.dummy, _ = bcrypt.GenerateFromPassword([]byte(RandomID(16)), cost)
	})
	return s.dummy
}

// Resume authenticates with an existing session token.
func (s *Service) Resume(ctx context.Context, token string) (*Result, error) {
	if !validToken(token) {
		return nil, ErrInvalidCredentials
	}
	id, err := s.Store.SessionUser(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	u, err := s.Store.UserByID(ctx, id)
	if err != nil {
		return nil, err
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

// Prune deletes expired sessions and reports how many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	return s.Store.DeleteExpiredSessions(ctx, time.Now())
}

// tokenBytes is the size of a session token before hex encoding.
const tokenBytes = 32

// validToken reports whether t could be a token from newSession.
func validToken(t string) bool {
	if len(t) != 2*tokenBytes {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

func (s *Service) newSession(ctx context.Context, u *store.User) (*Result, error) {
	ttl := s.SessionTTL
	if ttl == 0 {
		ttl = 30 * 24 * time.Hour
	}
	token := RandomID(tokenBytes)
	err := s.Store.CreateSession(ctx, token, u.ID, u.PasswordHash, time.Now().Add(ttl))
	if errors.Is(err, store.ErrNotFound) {
		// The password changed while this one was being checked.
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return &Result{User: u, Token: token}, nil
}

// RandomID returns n random bytes, hex encoded.
func RandomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
