package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cribbage/internal/auth"
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

// Invites sent on the deleting connection while the deletion runs do not
// outlive the account (audit run-1, 4.4).
func TestInvitesSentWhileDeletingAreDropped(t *testing.T) {
	in := startPersistent(t, filepath.Join(t.TempDir(), "d.db"), "127.0.0.1:0")
	defer in.stop(t)
	addr := in.ln.Addr().String()
	code := &pb.ClientMessage{Body: &pb.ClientMessage_SendInvite{SendInvite: &pb.SendInvite{}}}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("user%02d", i)
		a := login(t, addr, name)
		a.send(t, deleteAccount(name))
		for k := 0; k < 10; k++ {
			if _, err := a.Send(code); err != nil {
				break
			}
			time.Sleep(200 * time.Microsecond)
		}
		closed(t, a)
		rows, err := in.db.LoadInvites(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.From == a.auth.UserId {
				t.Fatalf("trial %d: an invite from the deleted account survived", i)
			}
		}
	}
}

// After a restart, invites to or from accounts that are gone are dropped.
func TestRestoreDropsInvitesOfDeletedAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	in := startPersistent(t, path, "127.0.0.1:0")
	addr := in.ln.Addr().String()
	bob := login(t, addr, "bob").auth.UserId
	ctx := context.Background()
	if err := in.db.CreateInvite(ctx, &store.Invite{ID: "ghost", From: 999, FromName: "Ghost", To: bob, ToName: "Nick bob",
		Created: time.Now(), Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	in.stop(t)
	in = startPersistent(t, path, addr)
	defer in.stop(t)
	if rows, _ := in.db.LoadInvites(ctx); len(rows) != 0 {
		t.Fatalf("invites left: %+v", rows)
	}
	if got := in.srv.invitesOf(bob); len(got) != 0 {
		t.Fatalf("bob still sees %v", got)
	}
}

// gatedGames holds CreateGame, once armed, after the game is stored.
type gatedGames struct {
	*store.SQLite
	armed         atomic.Bool
	entered, gate chan struct{}
}

func (g *gatedGames) CreateGame(ctx context.Context, rec *store.Game) error {
	err := g.SQLite.CreateGame(ctx, rec)
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.gate
	}
	return err
}

// Deleting an account waits for a game being created for it, and ends it
// (audit run-1, 4.5).
func TestDeleteWaitsForGameBeingCreated(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gs := &gatedGames{SQLite: db, entered: make(chan struct{}), gate: make(chan struct{})}
	srv := New(Config{Auth: &auth.Service{Store: db, BotKey: testBotKey}, Games: gs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTCP(ln)
	t.Cleanup(func() { ln.Close(); srv.Shutdown() })
	a, b := login(t, ln.Addr().String(), "alice"), login(t, ln.Addr().String(), "bob")
	a.next(t, replyTo(a.send(t, sendInvite("bob"))))
	inv := b.next(t, pushedInvites).GetInvites().Invites
	gs.armed.Store(true)
	b.send(t, respond(inv[0].InviteId, true))
	<-gs.entered
	deleted := make(chan error, 1)
	go func() { deleted <- srv.AdminDeleteAccount(context.Background(), "alice") }()
	time.Sleep(50 * time.Millisecond) // let the deletion reach the wait
	close(gs.gate)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	for _, g := range srv.gamesOf(a.auth.UserId) {
		g.smu.Lock()
		st := g.sum.status
		g.smu.Unlock()
		if st != pb.GameStatus_GAME_STATUS_FINISHED {
			t.Fatalf("game %s seating the deleted account is %v", g.id, st)
		}
	}
	recs, _ := db.LoadUnfinished(context.Background())
	if len(recs) != 0 {
		t.Fatalf("unfinished games left: %d", len(recs))
	}
}
