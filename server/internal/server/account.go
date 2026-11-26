package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cribbage/internal/auth"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/ratelimit"
	"cribbage/internal/store"
)

// background runs fn on its own goroutine, which Shutdown waits for. It
// reports false, without running fn, once the server is shutting down.
func (s *Server) background(fn func()) bool {
	s.cmu.RLock()
	defer s.cmu.RUnlock()
	if s.shuttingDown() {
		return false
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
	return true
}

// allowFrom spends a token of l for c's address, sending an error if there
// is none. Connections that are not limited always pass.
func (s *Server) allowFrom(c *Conn, id uint32, l *ratelimit.Limiter) bool {
	if !limited(c) || l.Take(addrKey(remoteIP(c.t)), time.Now()) {
		return true
	}
	c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many attempts; try again later")
	return false
}

// accountMessage describes a user's account settings.
func (s *Server) accountMessage(ctx context.Context, u *store.User) (*pb.Account, error) {
	a, err := s.auth.Account(ctx, u)
	if err != nil {
		return nil, err
	}
	m := &pb.Account{
		Email: a.Email, PendingEmail: a.PendingEmail, PendingNeedsCurrent: a.PendingNeedsCurrent,
		PasskeysEnabled: a.PasskeysEnabled,
	}
	for _, k := range a.Passkeys {
		p := &pb.Passkey{Id: auth.PasskeyID(k.ID), Name: k.Name, Created: k.Created.UnixMilli()}
		if !k.LastUsed.IsZero() {
			p.LastUsed = k.LastUsed.UnixMilli()
		}
		m.Passkeys = append(m.Passkeys, p)
	}
	return m, nil
}

// pushAccount sends a user's account settings to each of their
// connections, with the request id on reply's copy.
func (s *Server) pushAccount(ctx context.Context, userID int64, r reply) error {
	u, err := s.auth.Store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	a, err := s.accountMessage(ctx, u)
	if err != nil {
		return err
	}
	s.pushUser(userID, &pb.ServerMessage{Body: &pb.ServerMessage_Account{Account: a}}, []reply{r})
	return nil
}

// sendAccountError reports a failed account or sign-in request.
func (s *Server) sendAccountError(c *Conn, id uint32, what string, err error) {
	var needs *auth.NeedsUsername
	switch {
	case errors.As(err, &needs):
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_NeedsUsername{NeedsUsername: &pb.NeedsUsername{Email: needs.Email}}})
	case errors.Is(err, auth.ErrInvalidInput), errors.Is(err, auth.ErrMailDisabled),
		errors.Is(err, auth.ErrPasskeysDisabled), errors.Is(err, auth.ErrEmailStopped):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", err.Error())
	case errors.Is(err, store.ErrUsernameTaken):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_USERNAME_TAKEN, "", "that username is taken")
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS, "", err.Error())
	case errors.Is(err, auth.ErrBadToken), errors.Is(err, auth.ErrTooManyTries):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_NOT_FOUND, "", err.Error())
	case errors.Is(err, auth.ErrMailLimit):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", err.Error())
	default:
		s.log.Error(what, "err", err)
		msg := "internal error"
		if strings.HasPrefix(err.Error(), "send ") {
			msg = "could not send the email; try again later"
		}
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", msg)
	}
}

func signInStarted(id uint32, challenge string) *pb.ServerMessage {
	return &pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_SignInStarted{SignInStarted: &pb.SignInStarted{
		Challenge: challenge, CodeLength: auth.CodeLength, ExpiresIn: uint32(auth.CodeLifetime.Seconds()),
	}}}
}

// requestSignIn answers at once with a challenge, then looks up the
// account and mails its code in the background, so neither the reply nor
// its timing says whether the account exists.
func (s *Server) requestSignIn(c *Conn, id uint32, name string) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already signed in")
		return
	}
	if !s.auth.MailEnabled() {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", auth.ErrMailDisabled.Error())
		return
	}
	if limited(c) {
		now, ip := time.Now(), addrKey(remoteIP(c.t))
		if !s.attempts.Take(ip, now) || !s.codes.Take(ip, now) {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many attempts; try again later")
			return
		}
	}
	challenge, err := s.auth.NewChallenge()
	if err != nil {
		s.sendAccountError(c, id, "sign in", err)
		return
	}
	c.signingIn.Store(true)
	c.send(signInStarted(id, challenge))
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The log says whether the name matched an account, never the name.
		found, err := s.auth.RequestSignIn(ctx, challenge, name)
		if err != nil {
			s.log.Error("sign-in email", "err", err)
		}
		s.log.Info("sign-in code asked for", "accounts", found)
	})
}

// setEmail mails the links that change the address, off the reader.
func (s *Server) setEmail(c *Conn, id uint32, m *pb.SetEmail) {
	if err := s.auth.CheckEmail(m.Email); err != nil {
		s.sendAccountError(c, id, "set email", err)
		return
	}
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		u, err := s.auth.Store.UserByID(ctx, c.user.ID)
		if err == nil {
			err = s.auth.SetEmail(ctx, u, m.Email)
		}
		if err != nil {
			s.sendAccountError(c, id, "set email", err)
			return
		}
		if err := s.pushAccount(ctx, u.ID, reply{c, id}); err != nil {
			s.sendAccountError(c, id, "account", err)
		}
	})
}

// verifyEmail takes a token from either link of an address change, off
// the reader (the first link sends another email).
func (s *Server) verifyEmail(c *Conn, id uint32, token string) {
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		userID, changed, err := s.auth.VerifyEmail(ctx, token)
		if err != nil {
			s.sendAccountError(c, id, "verify email", err)
			return
		}
		if changed {
			s.log.Info("email changed", "user", userID)
		}
		c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
		if err := s.pushAccount(ctx, userID, reply{}); err != nil {
			s.log.Error("account", "user", userID, "err", err)
		}
	})
}

// signOutOthers ends the user's other sessions and closes their other
// connections.
func (s *Server) signOutOthers(c *Conn, id uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.auth.SignOutOthers(ctx, c.user.ID, c.token); err != nil {
		s.sendAccountError(c, id, "sign out others", err)
		return
	}
	s.log.Info("signed out other devices", "user", c.user.ID)
	s.devices.dropApprovals(c.user.ID) // even this session's: it may have approved the intruder
	s.closeUserConns(c.user.ID, c)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
}

// closeUserConns closes every connection signed in as userID except keep,
// after their sessions have ended. Their clients reconnect, cannot resume,
// and ask the player to sign in.
func (s *Server) closeUserConns(userID int64, keep *Conn) {
	s.closeConnsWhere(userID, func(c *Conn) bool { return c != keep })
}

// closeConnsWhere closes the connections signed in as userID that match.
func (s *Server) closeConnsWhere(userID int64, match func(*Conn) bool) {
	s.cmu.RLock()
	var cs []*Conn
	for c := range s.conns[userID] {
		if match(c) {
			cs = append(cs, c)
		}
	}
	s.cmu.RUnlock()
	for _, c := range cs {
		c.close()
	}
}

// AdminSetEmail recovers an account: it sets the account's address, ends
// its sessions, removes its passkeys and disconnects it. It returns the
// address it replaced ("" if none).
// AdminAllowEmail lets email go to an address again after its owner (or
// someone with one of its emails) stopped it.
func (s *Server) AdminAllowEmail(ctx context.Context, email string) (bool, error) {
	was, err := s.auth.AllowEmail(ctx, email)
	if err == nil && was {
		s.log.Info("email allowed again by admin")
	}
	return was, err
}

func (s *Server) AdminSetEmail(ctx context.Context, username, email string) (old string, err error) {
	u, err := s.auth.AdminSetEmail(ctx, username, email)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no user %q", username)
	}
	if err != nil {
		return "", err
	}
	s.log.Info("email set by admin", "user", u.ID, "username", u.Username)
	s.devices.dropApprovals(u.ID)
	s.closeUserConns(u.ID, nil)
	return u.Email, nil
}

// Passkeys.

func (s *Server) passkeySignInBegin(c *Conn, id uint32) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already signed in")
		return
	}
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	opts, sd, err := s.auth.BeginPasskeySignIn()
	if err != nil {
		s.sendAccountError(c, id, "passkey", err)
		return
	}
	c.passkey = sd
	c.signingIn.Store(true)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_PasskeyOptions{PasskeyOptions: &pb.PasskeyOptions{OptionsJson: opts}}})
}

func (s *Server) passkeyAddBegin(c *Conn, id uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts, sd, err := s.auth.BeginAddPasskey(ctx, c.user)
	if err != nil {
		s.sendAccountError(c, id, "add passkey", err)
		return
	}
	c.passkey = sd
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_PasskeyOptions{PasskeyOptions: &pb.PasskeyOptions{OptionsJson: opts}}})
}

func (s *Server) passkeyAddFinish(c *Conn, id uint32, m *pb.PasskeyAddFinish) {
	sd := c.passkey
	c.passkey = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.auth.FinishAddPasskey(ctx, c.user, c.token, sd, m.CredentialJson, m.Name); err != nil {
		s.sendAccountError(c, id, "add passkey", err)
		return
	}
	s.log.Info("passkey added", "user", c.user.ID)
	if err := s.pushAccount(ctx, c.user.ID, reply{c, id}); err != nil {
		s.sendAccountError(c, id, "account", err)
	}
}

func (s *Server) passkeyRemove(c *Conn, id uint32, passkeyID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.auth.RemovePasskey(ctx, c.user.ID, passkeyID); err != nil {
		s.sendAccountError(c, id, "remove passkey", err)
		return
	}
	s.log.Info("passkey removed", "user", c.user.ID)
	if err := s.pushAccount(ctx, c.user.ID, reply{c, id}); err != nil {
		s.sendAccountError(c, id, "account", err)
	}
}

// History paging.

const (
	historyPage    = 50
	historyPageMax = 100
)

func (s *Server) getHistory(c *Conn, id uint32, m *pb.GetHistory) {
	limit := int(m.Limit)
	if limit <= 0 {
		limit = historyPage
	}
	limit = min(limit, historyPageMax)
	if m.After == "" && limited(c) && !s.records.Take(c.userKey, time.Now()) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many requests; slow down")
		return
	}
	var before store.Cursor
	if m.After != "" {
		var ok bool
		if before, ok = parseCursor(m.After); !ok {
			c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", "bad history cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	games, err := s.gameStore.History(ctx, c.user.ID, before, limit+1)
	if err != nil {
		s.log.Error("history", "user", c.user.ID, "err", err)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "internal error")
		return
	}
	h := &pb.History{}
	if len(games) > limit {
		games = games[:limit]
		last := games[limit-1]
		h.Next = strconv.FormatInt(last.Updated.UnixMilli(), 10) + ":" + last.ID
	}
	for _, g := range games {
		h.Games = append(h.Games, finishedSummary(g, c.user.ID))
	}
	if m.After == "" {
		recs, err := s.gameStore.Records(ctx, c.user.ID)
		if err != nil {
			s.log.Error("records", "user", c.user.ID, "err", err)
			c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "internal error")
			return
		}
		total := &pb.Record{}
		for _, r := range recs {
			h.Records = append(h.Records, &pb.Record{
				Opponent: &pb.PlayerInfo{UserId: r.Opponent.UserID, Nickname: r.Opponent.Nickname, Bot: r.Opponent.Bot},
				Games:    uint32(r.Games), Wins: uint32(r.Wins), Losses: uint32(r.Losses), Unfinished: uint32(r.Unfinished),
				Last: r.Last.UnixMilli(),
			})
			total.Games += uint32(r.Games)
			total.Wins += uint32(r.Wins)
			total.Losses += uint32(r.Losses)
			total.Unfinished += uint32(r.Unfinished)
			total.Last = max(total.Last, r.Last.UnixMilli())
		}
		h.Total = total
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_History{History: h}})
}

// parseCursor reads History.next: "<updated ms>:<game id>".
func parseCursor(s string) (store.Cursor, bool) {
	ms, gid, ok := strings.Cut(s, ":")
	if !ok || gid == "" || len(gid) > 64 {
		return store.Cursor{}, false
	}
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || n <= 0 {
		return store.Cursor{}, false
	}
	return store.Cursor{Updated: time.UnixMilli(n), ID: gid}, true
}
