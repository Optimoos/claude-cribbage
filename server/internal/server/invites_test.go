package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cribbage/internal/protocol/pb"
)

func sendInvite(user string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_SendInvite{SendInvite: &pb.SendInvite{Username: user}}}
}

func respond(id string, accept bool) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_RespondInvite{RespondInvite: &pb.RespondInvite{InviteId: id, Accept: accept}}}
}

func redeem(code string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_RedeemCode{RedeemCode: &pb.RedeemCode{Code: code}}}
}

// pushedInvites waits for an unrequested Invites message.
func pushedInvites(m *pb.ServerMessage) bool { return m.RequestId == 0 && m.GetInvites() != nil }

func TestInviteAccept(t *testing.T) {
	srv, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	b := login(t, addr, "bob")

	r := a.next(t, replyTo(a.send(t, sendInvite("BOB")))).GetInvites()
	if len(r.GetInvites()) != 1 || r.Invites[0].Incoming || r.Invites[0].To.Nickname != "Nick bob" || r.Invites[0].Code != "" {
		t.Fatalf("sender's invites: %v", r)
	}
	in := b.next(t, pushedInvites).GetInvites().Invites
	if len(in) != 1 || !in[0].Incoming || in[0].From.Nickname != "Nick alice" {
		t.Fatalf("recipient's invites: %v", in)
	}
	if exp := time.UnixMilli(in[0].Expires); time.Until(exp) < 47*time.Hour || time.Until(exp) > 49*time.Hour {
		t.Fatalf("invite expires %v", exp)
	}
	// The sender cannot accept their own invite.
	wantError(t, a.next(t, replyTo(a.send(t, respond(in[0].InviteId, true)))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	u := b.next(t, replyTo(b.send(t, respond(in[0].InviteId, true)))).GetGameUpdate()
	if u == nil || u.Game.Status != pb.GameStatus_GAME_STATUS_ACTIVE || u.Game.Seat != 1 || u.Game.Players[0].Nickname != "Nick alice" {
		t.Fatalf("accepting: %v", u)
	}
	if g := a.next(t, isUpdate(u.Game.GameId)).GetGameUpdate(); g.Game.Seat != 0 {
		t.Fatalf("sender's game: %v", g.Game)
	}
	// (Its Invites push may come before or after the game update.)
	if left := srv.invitesOf(a.auth.UserId); len(left) != 0 {
		t.Fatalf("invite still listed after accepting: %v", left)
	}
	// Answered once only.
	wantError(t, b.next(t, replyTo(b.send(t, respond(in[0].InviteId, true)))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
}

func TestInviteDeclineCancelAndRules(t *testing.T) {
	_, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	b := login(t, addr, "bob")
	login(t, addr, "carol")

	wantError(t, a.next(t, replyTo(a.send(t, sendInvite("nobody")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, a.next(t, replyTo(a.send(t, sendInvite("not a name!")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, a.next(t, replyTo(a.send(t, sendInvite("alice")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	id := a.next(t, replyTo(a.send(t, sendInvite("bob")))).GetInvites().Invites[0].InviteId
	// One invite between two players, whichever way.
	wantError(t, a.next(t, replyTo(a.send(t, sendInvite("bob")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, b.next(t, replyTo(b.send(t, sendInvite("alice")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	// Carol is not part of it.
	cc := login(t, addr, "carol")
	wantError(t, cc.next(t, replyTo(cc.send(t, respond(id, false)))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Declined: gone for both.
	if left := b.next(t, replyTo(b.send(t, respond(id, false)))).GetInvites().Invites; len(left) != 0 {
		t.Fatalf("after declining: %v", left)
	}
	if left := a.next(t, pushedInvites).GetInvites().Invites; len(left) != 0 {
		t.Fatalf("sender after decline: %v", left)
	}
	// Cancelled by the sender.
	id = a.next(t, replyTo(a.send(t, sendInvite("bob")))).GetInvites().Invites[0].InviteId
	if left := a.next(t, replyTo(a.send(t, respond(id, false)))).GetInvites().Invites; len(left) != 0 {
		t.Fatalf("after cancelling: %v", left)
	}
}

func TestInviteCode(t *testing.T) {
	_, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	c := login(t, addr, "carol")
	inv := a.next(t, replyTo(a.send(t, sendInvite("")))).GetInvites().Invites[0]
	if len(inv.Code) != 7 || inv.Code[3] != '-' || inv.To != nil {
		t.Fatalf("code invite: %v", inv)
	}
	wantError(t, a.next(t, replyTo(a.send(t, redeem(inv.Code)))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, c.next(t, replyTo(c.send(t, redeem("AAA-AAA")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, c.next(t, replyTo(c.send(t, redeem("hello")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Typed loosely: lower case, a space instead of the dash.
	loose := strings.ToLower(strings.Replace(inv.Code, "-", " ", 1))
	u := c.next(t, replyTo(c.send(t, redeem(loose)))).GetGameUpdate()
	if u == nil || u.Game.Status != pb.GameStatus_GAME_STATUS_ACTIVE || u.Game.Players[0].Nickname != "Nick alice" {
		t.Fatalf("redeeming: %v", u)
	}
	a.next(t, isUpdate(u.Game.GameId))
	wantError(t, c.next(t, replyTo(c.send(t, redeem(inv.Code)))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
}

func TestInviteLimits(t *testing.T) {
	l := *loadLimits
	l.InvitesOut, l.InvitesIn, l.Games = 2, 1, 1
	srv, addr := startServerWith(t, &l)
	a := login(t, addr, "alice")
	login(t, addr, "bob")
	login(t, addr, "carol")
	d := login(t, addr, "dave")

	a.next(t, replyTo(a.send(t, sendInvite("bob"))))
	a.next(t, replyTo(a.send(t, sendInvite(""))))
	wantError(t, a.next(t, replyTo(a.send(t, sendInvite("carol")))), pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)
	// Bob already has one incoming invite, his limit.
	wantError(t, d.next(t, replyTo(d.send(t, sendInvite("bob")))), pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)

	// A player at the game limit cannot accept, and the invite stays.
	id := d.next(t, replyTo(d.send(t, sendInvite("carol")))).GetInvites().Invites[0].InviteId
	c := login(t, addr, "carol")
	c.next(t, replyTo(c.send(t, createGame("random"))))
	wantError(t, c.next(t, replyTo(c.send(t, respond(id, true)))), pb.ErrorCode_ERROR_CODE_TOO_MANY_GAMES)
	if got := srv.invitesOf(d.auth.UserId); len(got) != 1 {
		t.Fatalf("invite lost after a refused accept: %v", got)
	}
}

func TestInvitesExpire(t *testing.T) {
	srv, addr := startServerWith(t, loadLimits)
	a := login(t, addr, "alice")
	b := login(t, addr, "bob")
	a.next(t, replyTo(a.send(t, sendInvite("bob"))))
	b.next(t, pushedInvites)
	srv.expireInvites(time.Now().Add(47 * time.Hour))
	if got := srv.invitesOf(a.auth.UserId); len(got) != 1 {
		t.Fatalf("expired early: %v", got)
	}
	srv.expireInvites(time.Now().Add(49 * time.Hour))
	for _, c := range []*tclient{a, b} {
		if left := c.next(t, pushedInvites).GetInvites().Invites; len(left) != 0 {
			t.Fatalf("after expiry: %v", left)
		}
	}
}

// Invites survive a restart, and an offline player sees theirs on login.
func TestInvitesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	in := startPersistent(t, path, "127.0.0.1:0")
	addr := in.ln.Addr().String()
	a := login(t, addr, "alice")
	login(t, addr, "bob").Close()
	a.next(t, replyTo(a.send(t, sendInvite("bob"))))
	in.stop(t)

	in = startPersistent(t, path, addr)
	defer in.stop(t)
	b := login(t, addr, "bob")
	list := b.next(t, replyTo(b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}}))).GetGameList()
	if len(list.Invites) != 1 || !list.Invites[0].Incoming {
		t.Fatalf("invites after restart: %v", list.Invites)
	}
	if u := b.next(t, replyTo(b.send(t, respond(list.Invites[0].InviteId, true)))).GetGameUpdate(); u == nil {
		t.Fatal("could not accept after restart")
	}
}
