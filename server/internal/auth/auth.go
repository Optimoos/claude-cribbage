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

	dummyOnce sync.Once
	dummy     []byte
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
	if n := len(password); n < 8 || n > 72 { // bcrypt uses at most 72 bytes
		return fmt.Errorf("%w: password must be 8-72 bytes", ErrInvalidInput)
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

func (s *Service) newSession(ctx context.Context, u *store.User) (*Result, error) {
	ttl := s.SessionTTL
	if ttl == 0 {
		ttl = 30 * 24 * time.Hour
	}
	token := RandomID(32)
	if err := s.Store.CreateSession(ctx, token, u.ID, time.Now().Add(ttl)); err != nil {
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
