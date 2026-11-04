package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

// deleteAccount mails a confirmation link or, for a bot account (which has
// no address), deletes it at once.
func (s *Server) deleteAccount(c *Conn, id uint32, m *pb.DeleteAccount) {
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		u, err := s.auth.Store.UserByID(ctx, c.user.ID)
		if err != nil {
			s.sendAccountError(c, id, "delete account", err)
			return
		}
		mailed, err := s.auth.RequestDelete(ctx, u, m.Username)
		if err != nil {
			s.sendAccountError(c, id, "delete account", err)
			return
		}
		if mailed {
			c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_Ack{Ack: &pb.Ack{}}})
			return
		}
		s.finishDelete(ctx, c, id, u.ID, u.Username)
	})
}

// confirmDeleteAccount deletes the account a link's token belongs to.
func (s *Server) confirmDeleteAccount(c *Conn, id uint32, token string) {
	if !s.allowFrom(c, id, s.attempts) {
		return
	}
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		userID, err := s.auth.TakeDeleteToken(ctx, token)
		if err != nil {
			s.sendAccountError(c, id, "confirm delete", err)
			return
		}
		u, err := s.auth.Store.UserByID(ctx, userID)
		if err != nil {
			s.sendAccountError(c, id, "confirm delete", err)
			return
		}
		s.finishDelete(ctx, c, id, u.ID, u.Username)
	})
}

// finishDelete deletes the account and tells c, which is closed if it was
// logged in as that account (every other such connection is closed too).
func (s *Server) finishDelete(ctx context.Context, c *Conn, id uint32, userID int64, username string) {
	if err := s.deleteUser(ctx, userID, c); err != nil {
		s.log.Error("delete account", "user", userID, "err", err)
		c.sendError(id, pb.ErrorCode_ERROR_CODE_INTERNAL, "", "could not delete the account; try again")
		return
	}
	s.log.Info("account deleted", "user", userID)
	c.send(&pb.ServerMessage{RequestId: id, Body: &pb.ServerMessage_AccountDeleted{AccountDeleted: &pb.AccountDeleted{Username: username}}})
	if c.authed.Load() && c.user.ID == userID {
		c.drainAndClose()
	}
}

// AdminDeleteAccount deletes a user's account, as if they had confirmed it
// themselves.
func (s *Server) AdminDeleteAccount(ctx context.Context, username string) error {
	u, err := s.auth.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q", username)
	}
	if err != nil {
		return err
	}
	if err := s.deleteUser(ctx, u.ID, nil); err != nil {
		return err
	}
	s.log.Info("account deleted by admin", "user", u.ID, "username", u.Username)
	return nil
}

// deleteUser ends a user's games and invites and removes their account.
// The user can start nothing new meanwhile: they are marked deleted first,
// which reserve refuses, and their connections but keep are closed (before
// and after, in case one was logging in).
func (s *Server) deleteUser(ctx context.Context, userID int64, keep *Conn) error {
	s.gmu.Lock()
	s.deleted[userID] = true
	s.gmu.Unlock()
	s.closeUserConns(userID, keep)
	s.cancelMatch(userID)

	for _, g := range s.gamesOf(userID) {
		done := make(chan struct{})
		if !g.submit(func() { defer close(done); g.leave(userID) }) {
			continue // already over
		}
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("ending game %s: %w", g.id, ctx.Err())
		}
	}
	for _, g := range s.gamesOf(userID) {
		g.smu.Lock()
		over := g.sum.status == pb.GameStatus_GAME_STATUS_FINISHED
		g.smu.Unlock()
		if !over {
			return fmt.Errorf("game %s did not end", g.id)
		}
	}
	s.dropInvites(userID)
	if err := s.auth.Store.DeleteUser(ctx, userID); err != nil {
		return err
	}
	if err := s.gameStore.ForgetUser(ctx, userID); err != nil {
		return err
	}
	s.dropInvites(userID) // any sent while this ran
	s.closeUserConns(userID, keep)
	return nil
}

// leave ends the game for a player whose account is being deleted: an
// open game is cancelled, and an active one resigned.
func (g *game) leave(userID int64) {
	p := g.seatOf(userID)
	if p < 0 {
		return
	}
	snap := g.snapshot()
	switch g.status {
	case pb.GameStatus_GAME_STATUS_WAITING:
		g.status = pb.GameStatus_GAME_STATUS_FINISHED
		g.dirty = true
		g.commit(snap, nil)
	case pb.GameStatus_GAME_STATUS_ACTIVE:
		g.commit(snap, []*pb.Event{g.resignSeat(p)})
	}
	g.updateSummary()
}

// dropInvites removes every invite to or from a user and tells the other
// players.
func (s *Server) dropInvites(userID int64) {
	s.imu.Lock()
	var gone []*store.Invite
	for _, inv := range s.inv.byUser[userID] {
		gone = append(gone, inv)
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
