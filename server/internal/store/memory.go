package store

import (
	"context"
	"fmt"
	"sort"
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
	tokens   map[string]AccountToken
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
		tokens:   map[string]AccountToken{},
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
	for k, tok := range m.tokens {
		if tok.Expires.Before(t) {
			delete(m.tokens, k)
		}
	}
	return n, nil
}

func (m *Memory) SetPassword(_ context.Context, userID int64, hash []byte, mustChange bool, keep string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[userID]
	if !ok {
		return ErrNotFound
	}
	u.PasswordHash, u.MustChange = append([]byte(nil), hash...), mustChange
	for k, s := range m.sessions {
		if s.userID == userID && k != keep {
			delete(m.sessions, k)
		}
	}
	m.deleteTokens(userID, TokenReset)
	return nil
}

// deleteTokens removes a user's tokens of a kind. Callers hold mu.
func (m *Memory) deleteTokens(userID int64, kind int) {
	for k, t := range m.tokens {
		if t.UserID == userID && t.Kind == kind {
			delete(m.tokens, k)
		}
	}
}

func (m *Memory) SetEmail(_ context.Context, userID int64, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[userID]
	if !ok {
		return ErrNotFound
	}
	u.Email = email
	m.deleteTokens(userID, TokenVerify)
	return nil
}

func (m *Memory) UsersByEmail(_ context.Context, email string) ([]*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*User
	if email == "" {
		return nil, nil
	}
	for _, u := range m.byID {
		if strings.EqualFold(u.Email, email) {
			c := *u
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) CreateAccountToken(_ context.Context, t *AccountToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteTokens(t.UserID, t.Kind)
	m.tokens[t.Token] = *t
	return nil
}

func (m *Memory) TakeAccountToken(_ context.Context, token string, kind int) (*AccountToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[token]
	if !ok || t.Kind != kind {
		return nil, ErrNotFound
	}
	delete(m.tokens, token)
	if !time.Now().Before(t.Expires) {
		return nil, ErrNotFound
	}
	return &t, nil
}

func (m *Memory) PendingEmail(_ context.Context, userID int64) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tokens {
		if t.UserID == userID && t.Kind == TokenVerify && time.Now().Before(t.Expires) {
			return t.Email, nil
		}
	}
	return "", nil
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

// played reports whether a finished game had an opponent for userID.
func played(g Game, userID int64) bool {
	for i, st := range g.Seats {
		o := g.Seats[1-i]
		if st.UserID == userID && (o.UserID != 0 || o.Bot != "") {
			return true
		}
	}
	return false
}

// History pages through the games kept for RecentGames (the last
// maxRecent).
func (m *Memory) History(_ context.Context, userID int64, before Cursor, limit int) ([]Game, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []Game
	for _, g := range m.recent[userID] {
		if !played(g, userID) {
			continue
		}
		if !before.Updated.IsZero() && !cursorBefore(g, before) {
			continue
		}
		all = append(all, g)
	}
	sort.Slice(all, func(i, j int) bool { return !cursorBefore(all[i], Cursor{all[j].Updated, all[j].ID}) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// cursorBefore reports whether g comes before c in (Updated, ID) order,
// at the millisecond precision that SQLite stores.
func cursorBefore(g Game, c Cursor) bool {
	a, b := g.Updated.UnixMilli(), c.Updated.UnixMilli()
	return a < b || a == b && g.ID < c.ID
}

func (m *Memory) Records(_ context.Context, userID int64) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byOpp := map[Seat]*Record{}
	var order []Seat
	for _, g := range m.recent[userID] {
		if !played(g, userID) {
			continue
		}
		me := 0
		if g.Seats[1].UserID == userID {
			me = 1
		}
		o := g.Seats[1-me]
		key := Seat{UserID: o.UserID, Bot: o.Bot}
		r := byOpp[key]
		if r == nil {
			r = &Record{}
			byOpp[key] = r
			order = append(order, key)
		}
		if !g.Updated.Before(r.Last) {
			r.Opponent, r.Last = o, g.Updated
		}
		r.Games++
		switch g.Winner {
		case me:
			r.Wins++
		case 1 - me:
			r.Losses++
		default:
			r.Unfinished++
		}
	}
	out := make([]Record, 0, len(order))
	for _, k := range order {
		out = append(out, *byOpp[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Games != out[j].Games {
			return out[i].Games > out[j].Games
		}
		return out[i].Last.After(out[j].Last)
	})
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
