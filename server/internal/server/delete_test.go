package server

import (
	"context"
	"testing"

	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

func deleteAccount(username string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_DeleteAccount{DeleteAccount: &pb.DeleteAccount{Username: username}}}
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

// Deleting takes a link sent to the account's address. The account's
// games end, its opponent wins, and its history is kept for the opponent.
func TestDeleteAccountByEmail(t *testing.T) {
	srv, addr, mails := startAccountServer(t, false)
	a, b := signUp(t, addr, mails, "alice", "alice@example.com"), login(t, addr, "bob")
	ctx := context.Background()
	gameID := startHumanGame(t, a, b)
	bot := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "random"}}}))).GetGameUpdate().Game.GameId
	if m := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_SendInvite{SendInvite: &pb.SendInvite{Username: "bob"}}}))); m.GetInvites() == nil {
		t.Fatalf("invite: %v", m)
	}

	if m := a.next(t, replyTo(a.send(t, deleteAccount("")))); m.GetAck() == nil {
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
	if _, err := srv.auth.Store.UserByUsername(ctx, "alice"); err == nil {
		t.Fatal("deleted account still there")
	}
	h := b.next(t, replyTo(b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_GetHistory{GetHistory: &pb.GetHistory{}}}))).GetHistory()
	if len(h.Games) != 1 || h.Games[0].Players[0].Nickname != store.DeletedNickname {
		t.Fatalf("opponent's history: %v", h)
	}
	wantError(t, n.next(t, replyTo(n.send(t, confirmDelete(token)))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	// The name is free again.
	signUp(t, addr, mails, "alice", "alice@example.com")
}

// A bot account has no address: its username is typed again and it is
// deleted at once.
func TestDeleteAccountWithoutEmail(t *testing.T) {
	srv, addr, _ := startAccountServer(t, false)
	a := login(t, addr, "alice")
	other := connect(t, addr)
	if _, err := other.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	other.startReading()
	wantError(t, a.next(t, replyTo(a.send(t, deleteAccount("alicia")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	if m := a.next(t, replyTo(a.send(t, deleteAccount("ALICE")))); m.GetAccountDeleted() == nil {
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
	if _, err := srv.auth.Store.UserByUsername(ctx, "alice"); err == nil {
		t.Fatal("deleted account still there")
	}
}
