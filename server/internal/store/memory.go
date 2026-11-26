package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"slices"
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
	passkeys map[string]Passkey // by ID
	stops    map[string]bool    // stopped mailboxes, by stop index
	stopKey  *emailCrypt        // for stop links only: addresses are kept as they are
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
		passkeys: map[string]Passkey{},
		stops:    map[string]bool{},
		stopKey:  randomEmailCrypt(),
	}
}

// randomEmailCrypt has a key that lasts as long as the process, like
// everything in a Memory.
func randomEmailCrypt() *emailCrypt {
	key := make([]byte, MinEmailKey)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	e, err := newEmailCrypt(key)
	if err != nil {
		panic(err)
	}
	return e
}

func (m *Memory) StopToken(email string) (string, error) { return m.stopKey.stopToken(email), nil }

func (m *Memory) StopEmail(_ context.Context, token string) error {
	idx, ok := m.stopKey.checkStopToken(token)
	if !ok {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops[idx] = true
	return nil
}

func (m *Memory) EmailStopped(_ context.Context, email string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stops[m.stopKey.stopIndex(email)], nil
}

func (m *Memory) AllowEmail(_ context.Context, email string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.stopKey.stopIndex(email)
	was := m.stops[idx]
	delete(m.stops, idx)
	return was, nil
}

func (m *Memory) DeleteUser(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.byID[userID]
	if !ok {
		return nil
	}
	delete(m.byID, userID)
	delete(m.byName, strings.ToLower(u.Username))
	for k, s := range m.sessions {
		if s.userID == userID {
			delete(m.sessions, k)
		}
	}
	m.deleteTokens(userID)
	m.deletePasskeys(userID)
	return nil
}

func (m *Memory) ForgetUser(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rename := func(seats *[2]Seat) {
		for i := range seats {
			if seats[i].UserID == userID {
				seats[i].Nickname = DeletedNickname
			}
		}
	}
	for _, g := range m.games {
		rename(&g.Game.Seats)
	}
	for _, list := range m.recent {
		for i := range list {
			rename(&list[i].Seats)
		}
	}
	for id, inv := range m.invites {
		if inv.From == userID || inv.To == userID {
			delete(m.invites, id)
		}
	}
	return nil
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
	if _, ok := m.byID[userID]; !ok {
		return ErrNotFound
	}
	m.sessions[token] = session{userID: userID, expires: expires}
	return nil
}

func (m *Memory) SessionUser(_ context.Context, token string) (int64, time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expires) {
		return 0, time.Time{}, ErrNotFound
	}
	return s.userID, s.expires, nil
}

func (m *Memory) ExtendSession(_ context.Context, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[token]; ok {
		s.expires = expires
		m.sessions[token] = s
	}
	return nil
}

func (m *Memory) DeleteSessions(_ context.Context, userID int64, keep string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if s.userID == userID && k != keep {
			delete(m.sessions, k)
		}
	}
	for id, p := range m.passkeys {
		if p.UserID == userID && (keep == "" || p.Session != keep) {
			delete(m.passkeys, id)
		}
	}
	return nil
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

// deleteTokens removes a user's account tokens of the given kinds, or of
// every kind if none are given. Callers hold mu.
func (m *Memory) deleteTokens(userID int64, kinds ...int) {
	for k, t := range m.tokens {
		if t.UserID == userID && (len(kinds) == 0 || slices.Contains(kinds, t.Kind)) {
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
	if email != "" {
		u.Bot = false
	}
	m.deleteTokens(userID)
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

func (m *Memory) PendingEmail(_ context.Context, userID int64, kind int) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tokens {
		if t.UserID == userID && t.Kind == kind && time.Now().Before(t.Expires) {
			return t.Email, nil
		}
	}
	return "", nil
}

func (m *Memory) AddPasskey(_ context.Context, p *Passkey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[p.UserID]; !ok {
		return ErrNotFound
	}
	if s, ok := m.sessions[p.Session]; p.Session != "" && (!ok || s.userID != p.UserID || time.Now().After(s.expires)) {
		return ErrNotFound
	}
	if _, ok := m.passkeys[string(p.ID)]; ok {
		return fmt.Errorf("passkey exists")
	}
	c := *p
	c.ID, c.Data = bytes.Clone(p.ID), bytes.Clone(p.Data)
	m.passkeys[string(p.ID)] = c
	return nil
}

func (m *Memory) Passkeys(_ context.Context, userID int64) ([]Passkey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Passkey
	for _, p := range m.passkeys {
		if p.UserID == userID {
			p.ID, p.Data = bytes.Clone(p.ID), bytes.Clone(p.Data)
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return bytes.Compare(out[i].ID, out[j].ID) < 0
	})
	return out, nil
}

func (m *Memory) UpdatePasskey(_ context.Context, p *Passkey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.passkeys[string(p.ID)]; ok && q.UserID == p.UserID {
		q.Data, q.LastUsed = bytes.Clone(p.Data), p.LastUsed
		m.passkeys[string(p.ID)] = q
	}
	return nil
}

func (m *Memory) DeletePasskey(_ context.Context, userID int64, id []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.passkeys[string(id)]; !ok || q.UserID != userID {
		return ErrNotFound
	}
	delete(m.passkeys, string(id))
	return nil
}

func (m *Memory) DeletePasskeys(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletePasskeys(userID)
	return nil
}

// deletePasskeys removes a user's passkeys. Callers hold mu.
func (m *Memory) deletePasskeys(userID int64) {
	for k, p := range m.passkeys {
		if p.UserID == userID {
			delete(m.passkeys, k)
		}
	}
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
