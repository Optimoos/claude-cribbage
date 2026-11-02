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

// hashed runs fn, which hashes a password, once a hashing slot is free.
func hashed[T any](s *Server, ctx context.Context, fn func() (T, error)) (T, error) {
	select {
	case s.hashing <- struct{}{}:
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
	defer func() { <-s.hashing }()
	return fn()
}

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
	return &pb.Account{
		Email: a.Email, PendingEmail: a.PendingEmail, MustChangePassword: a.MustChange, EmailEnabled: a.EmailEnabled,
	}, nil
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

// sendAccountError reports a failed account request.
func (s *Server) sendAccountError(c *Conn, id uint32, what string, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInput), errors.Is(err, auth.ErrMailDisabled):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS, "", "wrong password")
	case errors.Is(err, auth.ErrBadToken):
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

// passwordKey is the failures limiter key for password checks by a
// logged-in user from c's address, as for logins.
func passwordKey(c *Conn) string {
	return strings.ToLower(c.user.Username) + "|" + addrKey(remoteIP(c.t))
}

// allowPasswordCheck spends one of c's user's password checks and one of
// their failures, which the caller returns with passwordChecked.
func (s *Server) allowPasswordCheck(c *Conn, id uint32) bool {
	if !limited(c) {
		return true
	}
	now := time.Now()
	if !s.passwordChecks.Take(c.userKey, now) || !s.failures.Take(passwordKey(c), now) {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_RATE_LIMITED, "", "too many attempts; try again later")
		return false
	}
	return true
}

// passwordChecked returns the failure allowPasswordCheck took, unless the
// password was wrong.
func (s *Server) passwordChecked(c *Conn, key string, err error) {
	if limited(c) && !errors.Is(err, auth.ErrInvalidCredentials) {
		s.failures.Return(key, time.Now())
	}
}

// changePassword runs on c's reader, like a login.
func (s *Server) changePassword(c *Conn, id uint32, m *pb.ChangePassword) {
	if !s.allowPasswordCheck(c, id) {
		return
	}
	key := passwordKey(c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := hashed(s, ctx, func() (*store.User, error) {
		return s.auth.ChangePassword(ctx, c.user.ID, m.OldPassword, m.NewPassword, c.token)
	})
	s.passwordChecked(c, key, err)
	if err != nil {
		s.sendAccountError(c, id, "change password", err)
		return
	}
	c.mustChange.Store(false)
	s.log.Info("password changed", "user", u.ID)
	// Other sessions ended; close their connections too.
	s.closeUserConns(u.ID, c)
	a, err := s.accountMessage(ctx, u)
	if err != nil {
		s.sendAccountError(c, id, "account", err)
		return
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Account{Account: a}})
}

// setEmail checks the password and mails a link, off the reader.
func (s *Server) setEmail(c *Conn, id uint32, m *pb.SetEmail) {
	if !s.allowPasswordCheck(c, id) {
		return
	}
	key, user := passwordKey(c), c.user
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_, err := hashed(s, ctx, func() (struct{}, error) {
			return struct{}{}, s.auth.SetEmail(ctx, user.ID, m.Password, m.Email)
		})
		s.passwordChecked(c, key, err)
		if err != nil {
			s.sendAccountError(c, id, "set email", err)
			return
		}
		if err := s.pushAccount(ctx, user.ID, reply{c, id}); err != nil {
			s.sendAccountError(c, id, "account", err)
		}
	})
}

// requestReset answers at once, then looks up the account and mails it in
// the background, so neither the reply nor its timing says whether the
// account exists.
func (s *Server) requestReset(c *Conn, id uint32, name string) {
	if !s.auth.MailEnabled() {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_BAD_REQUEST, "", auth.ErrMailDisabled.Error())
		return
	}
	if !s.allowFrom(c, id, s.resetRequests) {
		return
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.auth.RequestReset(ctx, name); err != nil {
			s.log.Error("password reset email", "err", err)
		}
	})
}

// resetPassword sets a new password from a reset link and logs c in.
func (s *Server) resetPassword(c *Conn, id uint32, m *pb.ResetPassword) {
	if c.user != nil {
		c.sendError(id, pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED, "", "already logged in")
		return
	}
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	s.authenticate(c, id, func(ctx context.Context) (*auth.Result, error) {
		res, err := hashed(s, ctx, func() (*auth.Result, error) {
			return s.auth.ResetPassword(ctx, m.Token, m.NewPassword)
		})
		if err == nil {
			s.log.Info("password reset by email", "user", res.User.ID)
			s.closeUserConns(res.User.ID, nil)
		}
		return res, err
	})
}

func (s *Server) verifyEmail(c *Conn, id uint32, token string) {
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	userID, err := s.auth.VerifyEmail(ctx, token)
	if err != nil {
		s.sendAccountError(c, id, "verify email", err)
		return
	}
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
	if err := s.pushAccount(ctx, userID, reply{}); err != nil {
		s.log.Error("account", "user", userID, "err", err)
	}
}

// closeUserConns closes every connection logged in as userID except keep,
// after their sessions have ended. Their clients reconnect, cannot resume,
// and ask for a password.
func (s *Server) closeUserConns(userID int64, keep *Conn) {
	s.cmu.RLock()
	var cs []*Conn
	for c := range s.conns[userID] {
		if c != keep {
			cs = append(cs, c)
		}
	}
	s.cmu.RUnlock()
	for _, c := range cs {
		c.close()
	}
}

// AdminResetPassword gives a user a temporary password that they must
// change when they next log in, and disconnects them. It also returns the
// email address it removed from the account, if any.
func (s *Server) AdminResetPassword(ctx context.Context, username string) (temp, email string, err error) {
	var u *store.User
	_, err = hashed(s, ctx, func() (struct{}, error) {
		var err error
		u, temp, err = s.auth.AdminResetPassword(ctx, username)
		return struct{}{}, err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("no user %q", username)
	}
	if err != nil {
		return "", "", err
	}
	s.log.Info("password reset by admin", "user", u.ID, "username", u.Username)
	s.closeUserConns(u.ID, nil)
	return temp, u.Email, nil
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
