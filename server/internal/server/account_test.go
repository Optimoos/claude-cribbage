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

	"cribbage/internal/auth"
	"cribbage/internal/auth/passkeytest"
	"cribbage/internal/client"
	"cribbage/internal/mail"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/store"
)

type sentMail struct{ to, subject, body string }

// fakeMailer delivers messages to a channel.
type fakeMailer chan sentMail

func (f fakeMailer) Send(_ context.Context, m mail.Message) error {
	f <- sentMail{m.To, m.Subject, m.Body}
	return nil
}

var (
	linkRE = regexp.MustCompile(`https://crib\.example/#(signin|verify|change|delete)=([0-9a-f]{64})`)
	codeRE = regexp.MustCompile(`\b(\d{4}) (\d{4})\b`)
)

// mail waits for a message to "to".
func (f fakeMailer) mail(t *testing.T, to string) sentMail {
	t.Helper()
	select {
	case m := <-f:
		if m.to != to {
			t.Fatalf("mail to %s, want %s", m.to, to)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no mail sent")
	}
	return sentMail{}
}

// link waits for a message to "to" and returns its link's kind and token.
func (f fakeMailer) link(t *testing.T, to string) (string, string) {
	t.Helper()
	m := f.mail(t, to)
	l := linkRE.FindStringSubmatch(m.body)
	if l == nil {
		t.Fatalf("no link in %q", m.body)
	}
	return l[1], l[2]
}

// code waits for a sign-in email to "to" and returns its code and link.
func (f fakeMailer) code(t *testing.T, to string) (string, string) {
	t.Helper()
	m := f.mail(t, to)
	c, l := codeRE.FindStringSubmatch(m.body), linkRE.FindStringSubmatch(m.body)
	if c == nil || l == nil || l[1] != "signin" {
		t.Fatalf("no code and link in %q", m.body)
	}
	return c[1] + c[2], l[2]
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
// channel (none if nomail), with passkeys for https://crib.example.
func startAccountServer(t *testing.T, nomail bool) (*Server, string, fakeMailer) {
	t.Helper()
	mails := make(fakeMailer, 16)
	wa, err := auth.NewWebAuthn("https://crib.example")
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Service{Store: store.NewMemory(), BaseURL: "https://crib.example", BotKey: testBotKey, WebAuthn: wa}
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

// anon is a connection that has not signed in.
func anon(t *testing.T, addr string) *tclient {
	c := connect(t, addr)
	c.startReading()
	return c
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

func msg(body any) *pb.ClientMessage {
	m := &pb.ClientMessage{}
	switch b := body.(type) {
	case *pb.RequestSignIn:
		m.Body = &pb.ClientMessage_RequestSignIn{RequestSignIn: b}
	case *pb.EnterCode:
		m.Body = &pb.ClientMessage_EnterCode{EnterCode: b}
	case *pb.UseSignInLink:
		m.Body = &pb.ClientMessage_UseSignInLink{UseSignInLink: b}
	case *pb.StartDeviceSignIn:
		m.Body = &pb.ClientMessage_StartDeviceSignIn{StartDeviceSignIn: b}
	case *pb.ClaimDeviceSignIn:
		m.Body = &pb.ClientMessage_ClaimDeviceSignIn{ClaimDeviceSignIn: b}
	case *pb.ApproveDevice:
		m.Body = &pb.ClientMessage_ApproveDevice{ApproveDevice: b}
	case *pb.SetEmail:
		m.Body = &pb.ClientMessage_SetEmail{SetEmail: b}
	case *pb.VerifyEmail:
		m.Body = &pb.ClientMessage_VerifyEmail{VerifyEmail: b}
	case *pb.SignOutOthers:
		m.Body = &pb.ClientMessage_SignOutOthers{SignOutOthers: b}
	case *pb.PasskeySignInBegin:
		m.Body = &pb.ClientMessage_PasskeySignInBegin{PasskeySignInBegin: b}
	case *pb.PasskeySignInFinish:
		m.Body = &pb.ClientMessage_PasskeySignInFinish{PasskeySignInFinish: b}
	case *pb.PasskeyAddBegin:
		m.Body = &pb.ClientMessage_PasskeyAddBegin{PasskeyAddBegin: b}
	case *pb.PasskeyAddFinish:
		m.Body = &pb.ClientMessage_PasskeyAddFinish{PasskeyAddFinish: b}
	case *pb.PasskeyRemove:
		m.Body = &pb.ClientMessage_PasskeyRemove{PasskeyRemove: b}
	default:
		panic("msg: unknown body")
	}
	return m
}

func listGames() *pb.ClientMessage {
	return &pb.ClientMessage{Body: &pb.ClientMessage_ListGames{ListGames: &pb.ListGames{}}}
}

// call sends m on c and returns the reply.
func (c *tclient) call(t *testing.T, m *pb.ClientMessage) *pb.ServerMessage {
	t.Helper()
	return c.next(t, replyTo(c.send(t, m)))
}

// signUp creates a person's account through the new-account code emailed
// to the address (the last code, after any for its other accounts) and
// returns a connection signed in to it.
func signUp(t *testing.T, addr string, mails fakeMailer, user, email string) *tclient {
	t.Helper()
	c := anon(t, addr)
	st := c.call(t, msg(&pb.RequestSignIn{Name: email})).GetSignInStarted()
	if st == nil || st.CodeLength != auth.CodeLength {
		t.Fatalf("sign up: %v", st)
	}
	cs := codeRE.FindAllStringSubmatch(mails.mail(t, email).body, -1)
	if cs == nil {
		t.Fatal("sign up: no code")
	}
	code := cs[len(cs)-1][1] + cs[len(cs)-1][2]
	a := c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: code, Username: user})).GetAuthOk()
	if a == nil {
		t.Fatal("sign up: no AuthOk")
	}
	c.auth = a
	return c
}

// signIn signs in to an account by emailed code on a new connection.
func signIn(t *testing.T, addr string, mails fakeMailer, user, email string) *tclient {
	t.Helper()
	c := anon(t, addr)
	st := c.call(t, msg(&pb.RequestSignIn{Name: user})).GetSignInStarted()
	code, _ := mails.code(t, email)
	a := c.call(t, msg(&pb.EnterCode{Challenge: st.GetChallenge(), Code: code})).GetAuthOk()
	if a == nil {
		t.Fatal("sign in: no AuthOk")
	}
	c.auth = a
	return c
}

func TestSignUpAndSignIn(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "alice@example.com")
	if a.auth.Username != "alice" || a.auth.Account.GetEmail() != "alice@example.com" || !a.auth.Account.GetPasskeysEnabled() {
		t.Fatalf("auth: %v", a.auth)
	}

	// By code, by email address.
	b := signIn(t, addr, mails, "ALICE@example.com", "alice@example.com")
	if b.auth.UserId != a.auth.UserId || b.auth.Token == a.auth.Token {
		t.Fatalf("second sign-in: %v", b.auth)
	}
	// By link, on another connection than the one that asked.
	c := anon(t, addr)
	c.call(t, msg(&pb.RequestSignIn{Name: "alice"}))
	code, link := mails.code(t, "alice@example.com")
	d := anon(t, addr)
	if m := d.call(t, msg(&pb.UseSignInLink{Token: link})); m.GetAuthOk().GetUserId() != a.auth.UserId {
		t.Fatalf("link: %v", m)
	}
	wantError(t, anon(t, addr).call(t, msg(&pb.UseSignInLink{Token: link})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	_ = code

	// Wrong codes, then too many.
	st := c.call(t, msg(&pb.RequestSignIn{Name: "alice"})).GetSignInStarted()
	mails.code(t, "alice@example.com")
	for i := 0; i < 4; i++ {
		wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: "1"})), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)
	}
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: "1"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Signed in already.
	wantError(t, a.call(t, msg(&pb.RequestSignIn{Name: "alice"})), pb.ErrorCode_ERROR_CODE_ALREADY_AUTHENTICATED)
}

// An unknown username gets the same answer, and no email.
func TestSignInUnknownName(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	c := anon(t, addr)
	if st := c.call(t, msg(&pb.RequestSignIn{Name: "nobody"})).GetSignInStarted(); st == nil || st.Challenge == "" {
		t.Fatal("no challenge for an unknown username")
	}
	mails.none(t)
}

// An unknown address gets the same answer, and a code that creates an
// account once a username is chosen.
func TestNewAccountBySignIn(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	signUp(t, addr, mails, "taken", "taken@example.com")
	c := anon(t, addr)
	st := c.call(t, msg(&pb.RequestSignIn{Name: "new@example.com"})).GetSignInStarted()
	if st == nil {
		t.Fatal("no challenge")
	}
	code, _ := mails.code(t, "new@example.com")
	m := c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: code}))
	if m.GetNeedsUsername().GetEmail() != "new@example.com" {
		t.Fatalf("code: %v", m)
	}
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: code, Username: "taken"})), pb.ErrorCode_ERROR_CODE_USERNAME_TAKEN)
	wantError(t, c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: code, Username: "x"})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
	ok := c.call(t, msg(&pb.EnterCode{Challenge: st.Challenge, Code: code, Username: "newbie", Nickname: "Newbie"})).GetAuthOk()
	if ok.GetUsername() != "newbie" || ok.GetNickname() != "Newbie" || ok.GetToken() == "" || ok.GetAccount().GetEmail() != "new@example.com" {
		t.Fatalf("created: %v", ok)
	}
}

func TestNoMail(t *testing.T) {
	_, addr, _ := startAccountServer(t, true)
	c := anon(t, addr)
	wantError(t, c.call(t, msg(&pb.RequestSignIn{Name: "alice"})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
}

func TestMailLimitPerAccount(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	signUp(t, addr, mails, "alice", "alice@example.com")
	c := anon(t, addr)
	for i := 0; i < 10; i++ {
		c.call(t, msg(&pb.RequestSignIn{Name: "alice"}))
		mails.mail(t, "alice@example.com")
	}
	c.call(t, msg(&pb.RequestSignIn{Name: "alice"}))
	mails.none(t)
}

// Changing the address takes the current address's link, then the new
// one's; signed-in connections see each step.
func TestChangeEmail(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "old@example.com")
	acct := a.call(t, msg(&pb.SetEmail{Email: "new@example.com"})).GetAccount()
	if acct.GetPendingEmail() != "new@example.com" || !acct.GetPendingNeedsCurrent() {
		t.Fatalf("after asking: %v", acct)
	}
	kind, token := mails.link(t, "old@example.com")
	if kind != "change" {
		t.Fatalf("first link is %q", kind)
	}
	n := anon(t, addr)
	if m := n.call(t, msg(&pb.VerifyEmail{Token: token})); m.GetAck() == nil {
		t.Fatalf("first link: %v", m)
	}
	a.next(t, func(m *pb.ServerMessage) bool { return m.GetAccount() != nil && !m.GetAccount().PendingNeedsCurrent })
	kind, token = mails.link(t, "new@example.com")
	if kind != "verify" {
		t.Fatalf("second link is %q", kind)
	}
	n.call(t, msg(&pb.VerifyEmail{Token: token}))
	a.next(t, func(m *pb.ServerMessage) bool { return m.GetAccount().GetEmail() == "new@example.com" })
	if m := mails.mail(t, "old@example.com"); !strings.Contains(m.body, "now go to another address") {
		t.Fatalf("notice: %+v", m)
	}
	wantError(t, n.call(t, msg(&pb.VerifyEmail{Token: token})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, a.call(t, msg(&pb.SetEmail{Email: ""})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
}

func TestSignOutOthers(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "alice@example.com")
	other := connect(t, addr)
	if _, err := other.Resume(a.auth.Token); err != nil {
		t.Fatal(err)
	}
	other.startReading()
	b := signIn(t, addr, mails, "alice", "alice@example.com")
	bob := signUp(t, addr, mails, "bob", "bob@example.com")
	if m := b.call(t, msg(&pb.SignOutOthers{})); m.GetAck() == nil {
		t.Fatalf("sign out others: %v", m)
	}
	closed(t, a)
	closed(t, other)
	if _, err := connect(t, addr).Resume(a.auth.Token); err == nil {
		t.Fatal("an ended session resumed")
	}
	if m := b.call(t, listGames()); m.GetGameList() == nil {
		t.Fatalf("this device was signed out too: %v", m)
	}
	if m := bob.call(t, listGames()); m.GetGameList() == nil {
		t.Fatalf("another user was signed out: %v", m)
	}
}

func TestDeviceSignIn(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "alice@example.com")
	dev := anon(t, addr)
	dc := dev.call(t, msg(&pb.StartDeviceSignIn{})).GetDeviceCode()
	if dc == nil || len(dc.Code) != 9 || dc.Url != "https://crib.example/#device="+dc.Code {
		t.Fatalf("device code: %v", dc)
	}
	if m := dev.call(t, msg(&pb.ClaimDeviceSignIn{Challenge: dc.Challenge})); m.GetAck() == nil {
		t.Fatalf("claim before approval: %v", m)
	}
	wantError(t, dev.call(t, msg(&pb.ClaimDeviceSignIn{Challenge: "nope"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, a.call(t, msg(&pb.ApproveDevice{Code: "BCDF-BCDF"})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	// Case, spaces and the dash do not matter.
	typed := strings.ToLower(strings.ReplaceAll(dc.Code, "-", " "))
	if m := a.call(t, msg(&pb.ApproveDevice{Code: typed})); m.GetAck() == nil {
		t.Fatalf("approve: %v", m)
	}
	dev.next(t, func(m *pb.ServerMessage) bool { return m.GetDeviceApproved() != nil })
	ok := dev.call(t, msg(&pb.ClaimDeviceSignIn{Challenge: dc.Challenge})).GetAuthOk()
	if ok.GetUserId() != a.auth.UserId || ok.Token == a.auth.Token {
		t.Fatalf("device signed in as %v", ok)
	}
	if m := dev.call(t, listGames()); m.GetGameList() == nil {
		t.Fatalf("device: %v", m)
	}
	// Each code is approved and claimed once.
	wantError(t, a.call(t, msg(&pb.ApproveDevice{Code: dc.Code})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	wantError(t, anon(t, addr).call(t, msg(&pb.ClaimDeviceSignIn{Challenge: dc.Challenge})), pb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// Approved while the device was away: it claims on its return.
	dev2 := anon(t, addr)
	dc2 := dev2.call(t, msg(&pb.StartDeviceSignIn{})).GetDeviceCode()
	a.call(t, msg(&pb.ApproveDevice{Code: dc2.Code}))
	back := anon(t, addr)
	if m := back.call(t, msg(&pb.ClaimDeviceSignIn{Challenge: dc2.Challenge})); m.GetAuthOk().GetUserId() != a.auth.UserId {
		t.Fatalf("claim after reconnecting: %v", m)
	}
}

func TestPasskeySignIn(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "alice@example.com")
	dev := &passkeytest.Authenticator{Origin: "https://crib.example"}
	opts := a.call(t, msg(&pb.PasskeyAddBegin{})).GetPasskeyOptions()
	cred, err := dev.Create(opts.GetOptionsJson())
	if err != nil {
		t.Fatal(err)
	}
	acct := a.call(t, msg(&pb.PasskeyAddFinish{CredentialJson: cred, Name: "Phone"})).GetAccount()
	if len(acct.GetPasskeys()) != 1 || acct.Passkeys[0].Name != "Phone" {
		t.Fatalf("account: %v", acct)
	}
	// A second finish without a begin is refused.
	wantError(t, a.call(t, msg(&pb.PasskeyAddFinish{CredentialJson: cred})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)

	c := anon(t, addr)
	opts = c.call(t, msg(&pb.PasskeySignInBegin{})).GetPasskeyOptions()
	got, err := dev.Get(opts.GetOptionsJson(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if m := c.call(t, msg(&pb.PasskeySignInFinish{CredentialJson: got})); m.GetAuthOk().GetUserId() != a.auth.UserId {
		t.Fatalf("passkey sign-in: %v", m)
	}
	// The answer cannot be used again, on this connection or another.
	d := anon(t, addr)
	d.call(t, msg(&pb.PasskeySignInBegin{}))
	wantError(t, d.call(t, msg(&pb.PasskeySignInFinish{CredentialJson: got})), pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS)

	acct = a.call(t, msg(&pb.PasskeyRemove{Id: acct.Passkeys[0].Id})).GetAccount()
	if len(acct.GetPasskeys()) != 0 {
		t.Fatalf("after removing: %v", acct)
	}
	wantError(t, a.call(t, msg(&pb.PasskeyRemove{Id: "AAAA"})), pb.ErrorCode_ERROR_CODE_BAD_REQUEST)
}

// The admin recovers an account: a new address, every device signed out,
// passkeys gone.
func TestAdminSetEmail(t *testing.T) {
	srv, addr, mails := startAccountServer(t, false)
	a := signUp(t, addr, mails, "alice", "thief@example.com")
	ctx := context.Background()
	if err := srv.auth.Store.AddPasskey(ctx, &store.Passkey{ID: []byte("k"), UserID: a.auth.UserId, Data: []byte("{}"), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AdminSetEmail(ctx, "nobody", "a@example.com"); err == nil {
		t.Fatal("set an unknown user's address")
	}
	old, err := srv.AdminSetEmail(ctx, "ALICE", "owner@example.com")
	if err != nil || old != "thief@example.com" {
		t.Fatalf("admin: %q, %v", old, err)
	}
	closed(t, a)
	if _, err := connect(t, addr).Resume(a.auth.Token); err == nil {
		t.Fatal("session survived")
	}
	if keys, _ := srv.auth.Store.Passkeys(ctx, a.auth.UserId); len(keys) != 0 {
		t.Fatal("passkey survived")
	}
	mails.mail(t, "thief@example.com") // told
	signIn(t, addr, mails, "alice", "owner@example.com")
}

// A bot's key cannot sign in to a person's account.
func TestBotKeyOnlyForBots(t *testing.T) {
	_, addr, mails := startAccountServer(t, false)
	signUp(t, addr, mails, "alice", "alice@example.com")
	_, err := connect(t, addr).BotLogin("alice", "", testBotKey)
	var se client.ServerError
	if !errors.As(err, &se) || se.Code != pb.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS {
		t.Fatalf("bot key on a person's account: %v", err)
	}
	if _, err := connect(t, addr).BotLogin("robot", "", "not-the-key-at-all"); err == nil {
		t.Fatal("wrong bot key accepted")
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
