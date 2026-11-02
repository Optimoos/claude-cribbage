package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cribbage/internal/auth"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// Invites are held in memory (they are few: each player may send only a
// handful) and written through to the store so they survive a restart. An
// invite costs no game, goroutine or bot until it is accepted.
type invites struct {
	byID   map[string]*store.Invite
	byCode map[string]*store.Invite
	byUser map[int64]map[string]*store.Invite // sender's and recipient's
}

func newInvites() invites {
	return invites{byID: map[string]*store.Invite{}, byCode: map[string]*store.Invite{}, byUser: map[int64]map[string]*store.Invite{}}
}

// add and remove must be called with imu held.
func (iv *invites) add(inv *store.Invite) {
	iv.byID[inv.ID] = inv
	if inv.Code != "" {
		iv.byCode[inv.Code] = inv
	}
	for _, u := range []int64{inv.From, inv.To} {
		if u == 0 {
			continue
		}
		m := iv.byUser[u]
		if m == nil {
			m = map[string]*store.Invite{}
			iv.byUser[u] = m
		}
		m[inv.ID] = inv
	}
}

func (iv *invites) remove(inv *store.Invite) {
	delete(iv.byID, inv.ID)
	if inv.Code != "" {
		delete(iv.byCode, inv.Code)
	}
	for _, u := range []int64{inv.From, inv.To} {
		if m := iv.byUser[u]; m != nil {
			delete(m, inv.ID)
			if len(m) == 0 {
				delete(iv.byUser, u)
			}
		}
	}
}

// codeAlphabet leaves out letters and digits that are easy to confuse
// (I, L, O, 0, 1), so a code read over the phone arrives intact.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// newCode returns a code like "K7M-Q2X": 31^6, about 900 million, codes,
// against at most a few thousand live ones and rate-limited guessing.
func newCode() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b[:3]) + "-" + string(b[3:])
}

// normalizeCode accepts "k7m q2x", "K7MQ2X" and so on.
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	c := b.String()
	if len(c) != 6 {
		return ""
	}
	return c[:3] + "-" + c[3:]
}

// loadInvites reads stored invites at startup, dropping expired ones.
func (s *Server) loadInvites(ctx context.Context) error {
	all, err := s.gameStore.LoadInvites(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	s.imu.Lock()
	defer s.imu.Unlock()
	for i := range all {
		inv := &all[i]
		if now.After(inv.Expires) {
			if err := s.gameStore.DeleteInvite(ctx, inv.ID); err != nil {
				return err
			}
			continue
		}
		s.inv.add(inv)
	}
	return nil
}

func (s *Server) sendInvite(c *Conn, id uint32, username string) {
	if !s.invitesSent.Take(fmt.Sprint(c.user.ID), time.Now()) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many invites; try again later")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now()
	inv := &store.Invite{
		ID: auth.RandomID(12), From: c.user.ID, FromName: c.user.Nickname,
		Created: now, Expires: now.Add(s.limits.InviteFor),
	}
	username = strings.TrimSpace(username)
	if username == "" {
		inv.Code = newCode()
	} else {
		if !auth.ValidUsername(username) {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", fmt.Sprintf("no player is called %q", username))
			return
		}
		u, err := s.auth.Store.UserByUsername(ctx, username)
		if errors.Is(err, store.ErrNotFound) {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", fmt.Sprintf("no player is called %q", username))
			return
		}
		if err != nil {
			s.log.Error("invite lookup", "err", err)
			c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not send invite")
			return
		}
		if u.ID == c.user.ID {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "you cannot invite yourself")
			return
		}
		inv.To, inv.ToName = u.ID, u.Nickname
	}

	// Checked and stored under imu, so the limits hold under concurrent
	// requests; invites are rare enough that holding it across the write
	// costs nothing.
	s.imu.Lock()
	out, in := 0, 0
	for _, o := range s.inv.byUser[c.user.ID] {
		if o.From == c.user.ID {
			out++
		}
		if inv.To != 0 && (o.From == inv.To || o.To == inv.To) {
			s.imu.Unlock()
			c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "there is already an invite between you and "+inv.ToName)
			return
		}
	}
	if inv.To != 0 {
		for _, o := range s.inv.byUser[inv.To] {
			if o.To == inv.To {
				in++
			}
		}
	}
	switch {
	case out >= s.limits.InvitesOut:
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES, "",
			fmt.Sprintf("you have %d invites waiting for an answer, the most allowed; cancel one first", out))
		return
	case inv.To != 0 && in >= s.limits.InvitesIn:
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES, "", inv.ToName+" has too many invites waiting; try again later")
		return
	}
	for inv.Code != "" && s.inv.byCode[inv.Code] != nil {
		inv.Code = newCode()
	}
	if err := s.gameStore.CreateInvite(ctx, inv); err != nil {
		s.imu.Unlock()
		s.log.Error("create invite", "err", err)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not send invite")
		return
	}
	s.inv.add(inv)
	s.imu.Unlock()
	s.sendInvites(c, id)
	if inv.To != 0 {
		s.pushInvites(inv.To)
	}
}

func (s *Server) respondInvite(c *Conn, id uint32, inviteID string, accept bool) {
	s.imu.Lock()
	inv := s.inv.byID[inviteID]
	switch {
	case inv == nil || (inv.From != c.user.ID && inv.To != c.user.ID):
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", "that invite has expired or been withdrawn")
		return
	case accept && inv.To != c.user.ID:
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "you cannot accept your own invite")
		return
	}
	// Taken out now, so it can be answered only once.
	s.inv.remove(inv)
	s.imu.Unlock()
	if accept {
		s.startInvited(c, id, inv)
		return
	}
	s.deleteInvite(inv)
	s.sendInvites(c, id)
	s.pushInvites(inv.From, inv.To)
}

func (s *Server) redeemCode(c *Conn, id uint32, code string) {
	if !s.codeTries.Take(fmt.Sprint(c.user.ID), time.Now()) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many tries; try again later")
		return
	}
	code = normalizeCode(code)
	s.imu.Lock()
	inv := s.inv.byCode[code]
	switch {
	case code == "" || inv == nil:
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", "no invite has that code; codes last 48 hours")
		return
	case inv.From == c.user.ID:
		s.imu.Unlock()
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "that is your own invite code; give it to the person you want to play")
		return
	}
	s.inv.remove(inv)
	s.imu.Unlock()
	inv.To, inv.ToName = c.user.ID, c.user.Nickname
	s.startInvited(c, id, inv)
}

// startInvited starts the game for an accepted invite, which the caller
// has taken out of s.inv. If the game cannot start, the invite is put back.
func (s *Server) startInvited(c *Conn, id uint32, inv *store.Invite) {
	restore := func() {
		if inv.Code != "" {
			inv.To, inv.ToName = 0, ""
		}
		s.imu.Lock()
		s.inv.add(inv)
		s.imu.Unlock()
	}
	release, err := s.reserve(gameKind{},
		reserveFor{id: inv.From, name: inv.FromName},
		reserveFor{id: c.user.ID, self: true})
	if err != nil {
		restore()
		s.sendLimit(c, id, err)
		return
	}
	defer release()
	g, err := s.newGame([2]seat{{userID: inv.From, nickname: inv.FromName}, {userID: c.user.ID, nickname: c.user.Nickname}}, true)
	if err != nil {
		restore()
		if !s.shuttingDown() {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not create game")
		}
		return
	}
	s.deleteInvite(inv)
	g.submit(func() { g.start(reply{c, id}) })
	s.pushInvites(inv.From, c.user.ID)
}

func (s *Server) deleteInvite(inv *store.Invite) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.gameStore.DeleteInvite(ctx, inv.ID); err != nil {
		// It is gone from memory; at worst it reappears after a restart
		// until it expires.
		s.log.Error("delete invite", "invite", inv.ID, "err", err)
	}
}

// expireInvites drops invites past their time and tells both sides.
func (s *Server) expireInvites(now time.Time) {
	var gone []*store.Invite
	s.imu.Lock()
	for _, inv := range s.inv.byID {
		if now.After(inv.Expires) {
			gone = append(gone, inv)
		}
	}
	for _, inv := range gone {
		s.inv.remove(inv)
	}
	s.imu.Unlock()
	for _, inv := range gone {
		s.deleteInvite(inv)
		s.pushInvites(inv.From, inv.To)
	}
}

// invitesOf lists userID's pending invites, oldest first.
func (s *Server) invitesOf(userID int64) []*pb.Invite {
	s.imu.Lock()
	list := make([]*store.Invite, 0, len(s.inv.byUser[userID]))
	for _, inv := range s.inv.byUser[userID] {
		list = append(list, inv)
	}
	s.imu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	out := make([]*pb.Invite, 0, len(list))
	for _, inv := range list {
		p := &pb.Invite{
			InviteId: inv.ID,
			From:     &pb.PlayerInfo{UserId: inv.From, Nickname: inv.FromName, Connected: s.connected(inv.From)},
			Expires:  inv.Expires.UnixMilli(),
			Incoming: inv.To == userID,
		}
		if inv.To != 0 {
			p.To = &pb.PlayerInfo{UserId: inv.To, Nickname: inv.ToName, Connected: s.connected(inv.To)}
		}
		if inv.From == userID {
			p.Code = inv.Code
		}
		out = append(out, p)
	}
	return out
}

func (s *Server) sendInvites(c *Conn, id uint32) {
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Invites{Invites: &pb.Invites{Invites: s.invitesOf(c.user.ID)}}})
}

// pushInvites sends each user (other than 0) their current invites.
func (s *Server) pushInvites(users ...int64) {
	for _, u := range users {
		if u != 0 {
			s.pushUser(u, &pb.ServerMessage{Body: &pb.ServerMessage_Invites{Invites: &pb.Invites{Invites: s.invitesOf(u)}}}, nil)
		}
	}
}
