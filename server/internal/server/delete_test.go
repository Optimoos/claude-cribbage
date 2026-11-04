package server

import (
	"context"
	"strings"
	"testing"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

func deleteAccount(password, username string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_DeleteAccount{DeleteAccount: &pb.DeleteAccount{Password: password, Username: username}}}
}

func confirmDelete(token string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_ConfirmDeleteAccount{ConfirmDeleteAccount: &pb.ConfirmDeleteAccount{Token: token}}}
}

// startHumanGame has a create an open game and b join it.
func startHumanGame(t *testing.T, a, b *tclient) string {
	t.Helper()
	gameID := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{}}}))).GetGameUpdate().Game.GameId
	b.next(t, replyTo(b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_JoinGame{JoinGame: &pb.JoinGame{GameId: gameID}}})))
	a.next(t, isUpdate(gameID))
	return gameID
}

// With an email address, deleting takes a link sent there. The account's
// games end, its opponent wins, and its history is kept for the opponent.
func TestDeleteAccountByEmail(t *testing.T) {
	srv, addr, mails := startAccountServer(t, false)
	a, b := login(t, addr, "alice"), login(t, addr, "bob")
	ctx := context.Background()
	if err := srv.auth.Store.SetEmail(ctx, a.auth.UserId, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	gameID := startHumanGame(t, a, b)
	bot := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "random"}}}))).GetGameUpdate().Game.GameId
	if m := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SendInvite{SendInvite: &pb.SendInvite{Username: "bob"}}}))); m.GetInvites() == nil {
		t.Fatalf("invite: %v", m)
	}

	wantError(t, a.next(t, replyTo(a.send(t, deleteAccount("wrong-password", "")))), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	if m := a.next(t, replyTo(a.send(t, deleteAccount("password123", "")))); m.GetAck() == nil {
		t.Fatalf("delete request: %v", m)
	}
	kind, token := mails.link(t, "alice@example.com")
	if kind != "delete" {
		t.Fatalf("link kind %q", kind)
	}
	// Nothing has happened yet.
	if _, err := srv.auth.Store.UserByID(ctx, a.auth.UserId); err != nil {
		t.Fatal("deleted before the link was opened")
	}

	n := anon(t, addr)
	m := n.next(t, replyTo(n.send(t, confirmDelete(token))))
	if m.GetAccountDeleted().GetUsername() != "alice" {
		t.Fatalf("confirm: %v", m)
	}
	closed(t, a)
	u := b.next(t, func(m *pb.ServerMessage) bool {
		u := m.GetGameUpdate()
		return u != nil && u.Game.GameId == gameID && u.Game.Status == pb.GameStatus_GAME_STATUS_FINISHED
	}).GetGameUpdate()
	if u.Game.Winner != 1 || !u.Game.Resigned {
		t.Fatalf("opponent's game: %v", u.Game)
	}
	b.next(t, func(m *pb.ServerMessage) bool { return m.GetInvites() != nil && len(m.GetInvites().Invites) == 0 })
	if g, err := srv.gameStore.(*store.Memory).LoadGame(ctx, bot); err == nil && g.Game.Status != store.StatusFinished {
		t.Fatalf("bot game still going: %+v", g.Game)
	}
	if _, err := loginAs(t, addr, "alice", "password123"); err == nil {
		t.Fatal("deleted account can log in")
	}
	h := b.next(t, replyTo(b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_GetHistory{GetHistory: &pb.GetHistory{}}}))).GetHistory()
	if len(h.Games) != 1 || h.Games[0].Players[0].Nickname != store.DeletedNickname {
		t.Fatalf("opponent's history: %v", h)
	}
	wantError(t, n.next(t, replyTo(n.send(t, confirmDelete(token)))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	// The name is free again.
	login(t, addr, "alice")
}

// Without an address, the username is typed again and the account is
// deleted at once.
func TestDeleteAccountWithoutEmail(t *testing.T) {
	srv, addr, _ := startAccountServer(t, false)
	a := login(t, addr, "alice")
	other := connect(t, addr)
	if _, err := other.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	other.startReading()
	wantError(t, a.next(t, replyTo(a.send(t, deleteAccount("password123", "alicia")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	if m := a.next(t, replyTo(a.send(t, deleteAccount("password123", "ALICE")))); m.GetAccountDeleted() == nil {
		t.Fatalf("delete: %v", m)
	}
	closed(t, a)
	closed(t, other)
	if _, err := srv.auth.Store.UserByUsername(context.Background(), "alice"); err == nil {
		t.Fatal("account still there")
	}
}

func TestAdminDeleteAccount(t *testing.T) {
	srv, addr, _ := startAccountServer(t, true)
	a := login(t, addr, "alice")
	ctx := context.Background()
	if err := srv.AdminDeleteAccount(ctx, "nobody"); err == nil {
		t.Fatal("deleted an unknown user")
	}
	if err := srv.AdminDeleteAccount(ctx, "ALICE"); err != nil {
		t.Fatal(err)
	}
	closed(t, a)
	if _, err := loginAs(t, addr, "alice", "password123"); err == nil {
		t.Fatal("deleted account can log in")
	}
}

// Changing or removing an address tells the old one.
func TestOldAddressTold(t *testing.T) {
	srv, addr, mails := startAccountServer(t, false)
	a := login(t, addr, "alice")
	if err := srv.auth.Store.SetEmail(context.Background(), a.auth.UserId, "old@example.com"); err != nil {
		t.Fatal(err)
	}
	if m := a.next(t, replyTo(a.send(t, setEmail("new@example.com", "password123")))); m.GetAccount() == nil {
		t.Fatalf("set email: %v", m)
	}
	_, token := mails.link(t, "new@example.com")
	n := anon(t, addr)
	n.next(t, replyTo(n.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_VerifyEmail{VerifyEmail: &pb.VerifyEmail{Token: token}}})))
	m := <-mails
	if m.to != "old@example.com" || !strings.Contains(m.body, "changed to another address") {
		t.Fatalf("notice: %+v", m)
	}
	a.next(t, replyTo(a.send(t, setEmail("", "password123"))))
	if m := <-mails; m.to != "new@example.com" || !strings.Contains(m.body, "removed") {
		t.Fatalf("removal notice: %+v", m)
	}
}
