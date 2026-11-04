package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/auth"
	"cribbage/internal/client"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

type sentMail struct{ to, subject, body string }

// fakeMailer delivers messages to a channel.
type fakeMailer chan sentMail

func (f fakeMailer) Send(_ context.Context, to, subject, body string) error {
	f <- sentMail{to, subject, body}
	return nil
}

var linkRE = regexp.MustCompile(`https://crib\.example/#(reset|verify|delete)=([0-9a-f]{64})`)

// link waits for a message to "to" and returns its link's kind and token.
func (f fakeMailer) link(t *testing.T, to string) (string, string) {
	t.Helper()
	select {
	case m := <-f:
		if m.to != to {
			t.Fatalf("mail to %s, want %s", m.to, to)
		}
		l := linkRE.FindStringSubmatch(m.body)
		if l == nil {
			t.Fatalf("no link in %q", m.body)
		}
		return l[1], l[2]
	case <-time.After(5 * time.Second):
		t.Fatal("no mail sent")
	}
	return "", ""
}

func (f fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-f:
		t.Fatalf("unexpected mail %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// startAccountServer starts a server whose mail goes to the returned
// channel (none if nomail).
func startAccountServer(t *testing.T, nomail bool) (*Server, string, fakeMailer) {
	t.Helper()
	mails := make(fakeMailer, 16)
	a := &auth.Service{Store: store.NewMemory(), BcryptCost: bcrypt.MinCost, BaseURL: "https://crib.example"}
	if !nomail {
		a.Mailer = mails
	}
	srv := New(Config{Auth: a, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTCP(ln)
	t.Cleanup(func() { ln.Close(); srv.Shutdown() })
	return srv, ln.Addr().String(), mails
}

// anon is a connection that has not logged in.
func anon(t *testing.T, addr string) *tclient {
	c := connect(t, addr)
	c.startReading()
	return c
}

func loginAs(t *testing.T, addr, user, password string) (*pb.AuthOk, error) {
	t.Helper()
	return connect(t, addr).Login(user, password, "", false)
}

// closed waits for the server to close c.
func closed(t *testing.T, c *tclient) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("connection still open")
		}
	}
}

func changePassword(old, new string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_ChangePassword{ChangePassword: &pb.ChangePassword{OldPassword: old, NewPassword: new}}}
}

func setEmail(email, password string) *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_SetEmail{SetEmail: &pb.SetEmail{Email: email, Password: password}}}
}

func listGames() *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}}
}

func TestChangePassword(t *testing.T) {
	_, addr, _ := startAccountServer(t, false)
	a := login(t, addr, "alice")
	other := connect(t, addr)
	if _, err := other.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	other.startReading()
	b, _ := loginAs(t, addr, "alice", "password123") // a second session
	bob := login(t, addr, "bob")

	wantError(t, a.next(t, replyTo(a.send(t, changePassword("wrong-password", "new-password")))), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, a.next(t, replyTo(a.send(t, changePassword("password123", "short")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, a.next(t, replyTo(a.send(t, changePassword("password123", "password123")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	m := a.next(t, replyTo(a.send(t, changePassword("password123", "new-password"))))
	if m.GetAccount() == nil {
		t.Fatalf("change password: %v", m)
	}
	// This connection carries on; the others, on this session or another,
	// are closed and cannot resume. Other users are untouched.
	if a.next(t, replyTo(a.send(t, listGames()))).GetGameList() == nil {
		t.Fatal("connection unusable after changing password")
	}
	closed(t, other)
	if _, err := connect(t, addr).Resume(b.Token); err == nil {
		t.Fatal("other session survived a password change")
	}
	if _, err := connect(t, addr).Resume(a.auth.Token); err != nil {
		t.Fatalf("this session ended: %v", err)
	}
	if bob.next(t, replyTo(bob.send(t, listGames()))).GetGameList() == nil {
		t.Fatal("another user was disturbed")
	}
	if _, err := loginAs(t, addr, "alice", "password123"); err == nil {
		t.Fatal("old password still works")
	}
	if _, err := loginAs(t, addr, "alice", "new-password"); err != nil {
		t.Fatal(err)
	}
}

func TestEmailVerifyAndReset(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := login(t, addr, "alice")
	if acct := a.auth.Account; acct == nil || !acct.EmailEnabled || acct.Email != "" {
		t.Fatalf("account at login: %v", acct)
	}

	wantError(t, a.next(t, replyTo(a.send(t, setEmail("alice@example.com", "wrong-password")))), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	wantError(t, a.next(t, replyTo(a.send(t, setEmail("not an address", "password123")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, a.next(t, replyTo(a.send(t, setEmail("Al <alice@example.com>", "password123")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	mails.none(t)

	acct := a.next(t, replyTo(a.send(t, setEmail("alice@example.com", "password123")))).GetAccount()
	if acct == nil || acct.Email != "" || acct.PendingEmail != "alice@example.com" {
		t.Fatalf("after SetEmail: %v", acct)
	}
	kind, verify := mails.link(t, "alice@example.com")
	if kind != "verify" {
		t.Fatalf("link kind %s", kind)
	}

	// A reset before the address is verified sends nothing.
	n := anon(t, addr)
	reset := func(name string) *pb.ClientMessage {
		return &pb.ClientMessage{Body: &pb.ClientMessage_RequestPasswordReset{RequestPasswordReset: &pb.RequestPasswordReset{Username: name}}}
	}
	if n.next(t, replyTo(n.send(t, reset("alice")))).GetAck() == nil {
		t.Fatal("reset request not acknowledged")
	}
	mails.none(t)

	verifyMsg := &pb.ClientMessage{Body: &pb.ClientMessage_VerifyEmail{VerifyEmail: &pb.VerifyEmail{Token: verify}}}
	if n.next(t, replyTo(n.send(t, verifyMsg))).GetAck() == nil {
		t.Fatal("verify not acknowledged")
	}
	pushed := a.next(t, func(m *pb.ServerMessage) bool { return m.GetAccount() != nil }).GetAccount()
	if pushed.Email != "alice@example.com" || pushed.PendingEmail != "" {
		t.Fatalf("account after verifying: %v", pushed)
	}
	wantError(t, n.next(t, replyTo(n.send(t, verifyMsg))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Unknown names and addresses get the same answer, and no mail.
	for _, name := range []string{"nobody", "nobody@example.com", "bad name!"} {
		if n.next(t, replyTo(n.send(t, reset(name)))).GetAck() == nil {
			t.Fatalf("reset request for %q not acknowledged", name)
		}
	}
	mails.none(t)

	// By address, in any case.
	n.next(t, replyTo(n.send(t, reset("ALICE@example.com"))))
	kind, token := mails.link(t, "alice@example.com")
	if kind != "reset" {
		t.Fatalf("link kind %s", kind)
	}
	resetMsg := func(tok, pw string) *pb.ClientMessage {
		return &pb.ClientMessage{Body: &pb.ClientMessage_ResetPassword{ResetPassword: &pb.ResetPassword{Token: tok, NewPassword: pw}}}
	}
	// A password that is too short does not use up the link.
	wantError(t, n.next(t, replyTo(n.send(t, resetMsg(token, "short")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	wantError(t, n.next(t, replyTo(n.send(t, resetMsg(strings.Repeat("0", 64), "brand-new-pw")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	ok := n.next(t, replyTo(n.send(t, resetMsg(token, "brand-new-pw")))).GetAuthOk()
	if ok == nil || ok.Username != "alice" || ok.Account.GetEmail() != "alice@example.com" {
		t.Fatalf("reset: %v", ok)
	}
	closed(t, a)
	if _, err := connect(t, addr).Resume(a.auth.Token); err == nil {
		t.Fatal("old session survived a reset")
	}
	m := anon(t, addr)
	wantError(t, m.next(t, replyTo(m.send(t, resetMsg(token, "another-pw-1")))), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	if _, err := loginAs(t, addr, "alice", "brand-new-pw"); err != nil {
		t.Fatal(err)
	}

	// Removing the address.
	c := login(t, addr, "carol")
	c.next(t, replyTo(c.send(t, setEmail("carol@example.com", "password123"))))
	_, verify = mails.link(t, "carol@example.com")
	n.next(t, replyTo(n.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_VerifyEmail{VerifyEmail: &pb.VerifyEmail{Token: verify}}})))
	c.next(t, func(m *pb.ServerMessage) bool { return m.GetAccount().GetEmail() != "" })
	if acct := c.next(t, replyTo(c.send(t, setEmail("", "password123")))).GetAccount(); acct.GetEmail() != "" {
		t.Fatalf("address not removed: %v", acct)
	}
	if m := <-mails; m.to != "carol@example.com" || !strings.Contains(m.body, "was removed") {
		t.Fatalf("removal notice: %+v", m)
	}
	n.next(t, replyTo(n.send(t, reset("carol"))))
	mails.none(t)
}

func TestMailLimitPerAccount(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := login(t, addr, "alice")
	a.next(t, replyTo(a.send(t, setEmail("alice@example.com", "password123"))))
	mails.link(t, "alice@example.com")
	a.next(t, replyTo(a.send(t, setEmail("alice2@example.com", "password123"))))
	mails.link(t, "alice2@example.com")
	a.next(t, replyTo(a.send(t, setEmail("alice3@example.com", "password123"))))
	mails.link(t, "alice3@example.com")
	wantError(t, a.next(t, replyTo(a.send(t, setEmail("alice4@example.com", "password123")))), pb.ErrorCode_ERROR_CODE_RATE_LIMITED)
	mails.none(t)
}

func TestNoMail(t *testing.T) {
	_, addr, _ := startAccountServer(t, true)
	a := login(t, addr, "alice")
	if a.auth.Account.GetEmailEnabled() {
		t.Fatal("email enabled without a mailer")
	}
	wantError(t, a.next(t, replyTo(a.send(t, setEmail("alice@example.com", "password123")))), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	n := anon(t, addr)
	m := n.next(t, replyTo(n.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_RequestPasswordReset{
		RequestPasswordReset: &pb.RequestPasswordReset{Username: "alice"}}})))
	wantError(t, m, pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
}

func TestAdminResetPassword(t *testing.T) {
	srv, addr, _ := startAccountServer(t, true)
	a := login(t, addr, "alice")
	ctx := context.Background()
	if _, _, err := srv.AdminResetPassword(ctx, "nobody"); err == nil {
		t.Fatal("reset an unknown user")
	}
	// An address someone else may have set goes with the password.
	if err := srv.auth.Store.SetEmail(ctx, a.auth.UserId, "thief@example.com"); err != nil {
		t.Fatal(err)
	}
	temp, removed, err := srv.AdminResetPassword(ctx, "ALICE")
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := srv.auth.Store.UserByID(ctx, a.auth.UserId); removed != "thief@example.com" || u.Email != "" {
		t.Fatalf("removed %q, address now %q", removed, u.Email)
	}
	if !regexp.MustCompile(`^[a-z2-9]{4}-[a-z2-9]{4}-[a-z2-9]{4}$`).MatchString(temp) {
		t.Fatalf("temporary password %q", temp)
	}
	closed(t, a)
	var se client.ServerError
	if _, err := connect(t, addr).Resume(a.auth.Token); !errors.As(err, &se) {
		t.Fatalf("session survived an admin reset: %v", err)
	}
	if _, err := loginAs(t, addr, "alice", "password123"); err == nil {
		t.Fatal("old password still works")
	}

	c := connect(t, addr)
	ok, err := c.Login("alice", temp, "", false)
	if err != nil || !ok.Account.GetMustChangePassword() {
		t.Fatalf("login with temporary password: %v, %v", ok, err)
	}
	c.startReading()
	wantError(t, c.next(t, replyTo(c.send(t, listGames()))), pb.ErrorCode_ERROR_CODE_PASSWORD_CHANGE_REQUIRED)
	wantError(t, c.next(t, replyTo(c.send(t, createGame("random")))), pb.ErrorCode_ERROR_CODE_PASSWORD_CHANGE_REQUIRED)
	acct := c.next(t, replyTo(c.send(t, changePassword(temp, "chosen-password")))).GetAccount()
	if acct == nil || acct.MustChangePassword {
		t.Fatalf("after changing: %v", acct)
	}
	if c.next(t, replyTo(c.send(t, listGames()))).GetGameList() == nil {
		t.Fatal("still blocked after changing password")
	}
	if ok, err := loginAs(t, addr, "alice", "chosen-password"); err != nil || ok.Account.GetMustChangePassword() {
		t.Fatalf("login after changing: %v, %v", ok, err)
	}
}

func TestHistory(t *testing.T) {
	_, addr, _ := startAccountServer(t, true)
	a := login(t, addr, "alice")
	for i := 0; i < 3; i++ {
		u := a.next(t, replyTo(a.send(t, createGame("random")))).GetGameUpdate()
		r := a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resign{Resign: &pb.Resign{GameId: u.Game.GameId}}})))
		if r.GetGameUpdate().GetGame().GetStatus() != pb.GameStatus_GAME_STATUS_FINISHED {
			t.Fatalf("resign: %v", r)
		}
		time.Sleep(2 * time.Millisecond) // distinct end times
	}
	// An open game cancelled before anyone joined is not history.
	u := a.next(t, replyTo(a.send(t, createGame("")))).GetGameUpdate()
	a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_Resign{Resign: &pb.Resign{GameId: u.Game.GameId}}})))

	get := func(after string, limit uint32) *pb.ServerMessage {
		return a.next(t, replyTo(a.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_GetHistory{GetHistory: &pb.GetHistory{After: after, Limit: limit}}})))
	}
	h := get("", 2).GetHistory()
	if h == nil || len(h.Games) != 2 || h.Next == "" {
		t.Fatalf("first page: %v", h)
	}
	if h.Games[0].Updated < h.Games[1].Updated || h.Games[0].Seat != 0 || h.Games[0].Winner != 1 || !h.Games[0].Resigned {
		t.Fatalf("first page games: %v", h.Games)
	}
	if len(h.Records) != 1 || h.Records[0].Opponent.Bot != "random" || h.Records[0].Games != 3 || h.Records[0].Losses != 3 ||
		h.Total.GetGames() != 3 || h.Total.GetWins() != 0 {
		t.Fatalf("records: %v, total %v", h.Records, h.Total)
	}
	h2 := get(h.Next, 2).GetHistory()
	if h2 == nil || len(h2.Games) != 1 || h2.Next != "" || h2.Records != nil || h2.Total != nil {
		t.Fatalf("second page: %v", h2)
	}
	if h2.Games[0].GameId == h.Games[0].GameId || h2.Games[0].GameId == h.Games[1].GameId {
		t.Fatal("pages overlap")
	}
	wantError(t, get("garbage", 0), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	// Someone with no games.
	b := login(t, addr, "bob")
	hb := b.next(t, replyTo(b.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_GetHistory{GetHistory: &pb.GetHistory{}}}))).GetHistory()
	if hb == nil || len(hb.Games) != 0 || hb.Total.GetGames() != 0 {
		t.Fatalf("empty history: %v", hb)
	}
}
