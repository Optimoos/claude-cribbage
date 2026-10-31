package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store and GameStore. Finished games are dropped so
// that long-running servers do not grow without bound.
type Memory struct {
	mu       sync.RWMutex
	nextID   int64
	byID     map[int64]*User
	byName   map[string]*User
	sessions map[string]session
	games    map[string]*GameWithActions
	recent   map[int64][]Game // newest last, capped at maxRecent
	invites  map[string]Invite
}

const maxRecent = 50

type session struct {
	userID  int64
	expires time.Time
}

func NewMemory() *Memory {
	return &Memory{
		byID:     map[int64]*User{},
		byName:   map[string]*User{},
		sessions: map[string]session{},
		games:    map[string]*GameWithActions{},
		recent:   map[int64][]Game{},
		invites:  map[string]Invite{},
	}
}

func (m *Memory) CreateInvite(_ context.Context, inv *Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[inv.ID] = *inv
	return nil
}

func (m *Memory) DeleteInvite(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.invites, id)
	return nil
}

func (m *Memory) LoadInvites(context.Context) ([]Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Invite, 0, len(m.invites))
	for _, inv := range m.invites {
		out = append(out, inv)
	}
	return out, nil
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

func (m *Memory) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *Memory) DeleteExpiredSessions(_ context.Context, t time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, s := range m.sessions {
		if s.expires.Before(t) {
			delete(m.sessions, k)
			n++
		}
	}
	return n, nil
}

func (m *Memory) CreateGame(_ context.Context, g *Game) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.games[g.ID]; ok {
		return fmt.Errorf("game %s exists", g.ID)
	}
	m.games[g.ID] = &GameWithActions{Game: *g}
	return nil
}

func (m *Memory) CommitGame(_ context.Context, id string, g *Game, acts []Action) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.games[id]
	if !ok {
		return ErrNotFound
	}
	for _, a := range acts {
		if want := uint32(len(rec.Actions)) + 1; a.Seq != want {
			return fmt.Errorf("game %s: action seq %d, want %d", id, a.Seq, want)
		}
		rec.Actions = append(rec.Actions, a)
	}
	if g != nil {
		rec.Game = *g
		if g.Status == StatusFinished {
			delete(m.games, id)
			for _, s := range g.Seats {
				if s.UserID != 0 {
					r := append(m.recent[s.UserID], *g)
					if len(r) > maxRecent {
						r = r[len(r)-maxRecent:]
					}
					m.recent[s.UserID] = r
				}
			}
		}
	}
	return nil
}

func (m *Memory) RecentGames(_ context.Context, userID int64, limit int) ([]Game, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.recent[userID]
	var out []Game
	for i := len(r) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r[i])
	}
	return out, nil
}

func (m *Memory) LoadGame(_ context.Context, id string) (*GameWithActions, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.games[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &GameWithActions{Game: g.Game, Actions: append([]Action(nil), g.Actions...)}, nil
}

func (m *Memory) LoadUnfinished(context.Context) ([]GameWithActions, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []GameWithActions
	for _, g := range m.games {
		out = append(out, GameWithActions{Game: g.Game, Actions: append([]Action(nil), g.Actions...)})
	}
	return out, nil
}
