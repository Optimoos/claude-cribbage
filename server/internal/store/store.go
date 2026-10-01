// Package store defines persistence for users and sessions. Game
// persistence is added in milestone 4.
package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
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

// Memory is an in-memory Store.
type Memory struct {
	mu       sync.RWMutex
	nextID   int64
	byID     map[int64]*User
	byName   map[string]*User
	sessions map[string]session
}

type session struct {
	userID  int64
	expires time.Time
}

func NewMemory() *Memory {
	return &Memory{byID: map[int64]*User{}, byName: map[string]*User{}, sessions: map[string]session{}}
}

func (m *Memory) CreateUser(_ context.Context, u *User) error {
	key := strings.ToLower(u.Username)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[key]; ok {
		return ErrUsernameTaken
	}
	m.nextID++
	u.ID = m.nextID
	c := *u
	m.byID[c.ID] = &c
	m.byName[key] = &c
	return nil
}

func (m *Memory) UserByUsername(_ context.Context, username string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byName[strings.ToLower(username)]
	if !ok {
		return nil, ErrNotFound
	}
	c := *u
	return &c, nil
}

func (m *Memory) UserByID(_ context.Context, id int64) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *u
	return &c, nil
}

func (m *Memory) CreateSession(_ context.Context, token string, userID int64, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = session{userID: userID, expires: expires}
	return nil
}

func (m *Memory) SessionUser(_ context.Context, token string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expires) {
		return 0, ErrNotFound
	}
	return s.userID, nil
}
