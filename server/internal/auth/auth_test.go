package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cribbage/internal/auth/passkeytest"
	"cribbage/internal/mail"
	"cribbage/internal/store"
)

type sent struct{ to, subject, body string }

// mailbox records emails instead of sending them.
type mailbox struct {
	mu   sync.Mutex
	msgs []sent
}

func (m *mailbox) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, sent{msg.To, msg.Subject, msg.Body})
	return nil
}

func (m *mailbox) last(t *testing.T) sent {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) == 0 {
		t.Fatal("no email")
	}
	return m.msgs[len(m.msgs)-1]
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

var (
	codeRE = regexp.MustCompile(`\b(\d{4}) (\d{4})\b`)
	linkRE = regexp.MustCompile(`#(signin|verify|change|delete)=([0-9a-f]{64})`)
)

// codes returns every code in an email, in order.
func codes(body string) []string {
	var out []string
	for _, m := range codeRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

func links(body, kind string) []string {
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(body, -1) {
		if m[1] == kind {
			out = append(out, m[2])
		}
	}
	return out
}

func newService(t *testing.T) (*Service, *mailbox) {
	t.Helper()
	mb := &mailbox{}
	return &Service{Store: store.NewMemory(), Mailer: mb, BaseURL: "https://crib.example/"}, mb
}

// register creates an account through the new-account code in a sign-in
// email: the last code, after any for accounts the address has already.
func register(t *testing.T, s *Service, mb *mailbox, user, email string) *Result {
	t.Helper()
	ctx := context.Background()
	ch, _ := s.NewChallenge()
	if _, err := s.RequestSignIn(ctx, ch, email); err != nil {
		t.Fatal(err)
	}
	cs := codes(mb.last(t).body)
	res, err := s.EnterCode(ctx, ch, cs[len(cs)-1], user, "")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNewAccountAndSignIn(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	ch, _ := s.NewChallenge()
	if _, err := s.RequestSignIn(ctx, ch, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	m := mb.last(t)
	if m.to != "alice@example.com" || len(codes(m.body)) != 1 || !strings.Contains(m.subject, codes(m.body)[0][:4]) {
		t.Fatalf("new-account email: %+v", m)
	}
	// Nothing exists until the code is entered.
	if _, err := s.Store.UserByUsername(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account created early: %v", err)
	}
	if _, err := s.EnterCode(ctx, ch, "0000 0000", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong code: %v", err)
	}
	res, err := s.EnterCode(ctx, ch, " "+strings.ReplaceAll(codes(m.body)[0], "", "-")+" ", "alice", "Al")
	if err != nil {
		t.Fatalf("code with dashes and spaces: %v", err)
	}
	if res.User.Username != "alice" || res.User.Nickname != "Al" || res.User.Email != "alice@example.com" || res.Token == "" {
		t.Fatalf("registered: %+v", res.User)
	}
	if _, err := s.EnterCode(ctx, ch, codes(m.body)[0], "alice2", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("code used twice: %v", err)
	}

	// Sign in by username: a code, and a link that works instead.
	ch, _ = s.NewChallenge()
	if _, err := s.RequestSignIn(ctx, ch, "Alice"); err != nil {
		t.Fatal(err)
	}
	m = mb.last(t)
	if len(codes(m.body)) != 1 || len(links(m.body, "signin")) != 1 {
		t.Fatalf("sign-in email: %q", m.body)
	}
	res2, err := s.UseSignInLink(ctx, links(m.body, "signin")[0], "", "")
	if err != nil || res2.User.ID != res.User.ID || res2.Token == res.Token {
		t.Fatalf("link: %+v, %v", res2, err)
	}
	// The link and code are one: using either uses up both.
	if _, err := s.EnterCode(ctx, ch, codes(m.body)[0], "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("code after its link: %v", err)
	}
	if _, err := s.UseSignInLink(ctx, links(m.body, "signin")[0], "", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("link used twice: %v", err)
	}
}

// Asking again sends the same code, so the first email still works.
func TestSignInCodeResent(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	register(t, s, mb, "alice", "alice@example.com")
	ch1, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch1, "alice")
	first := codes(mb.last(t).body)[0]
	ch2, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch2, "alice@EXAMPLE.com")
	if again := codes(mb.last(t).body)[0]; again != first {
		t.Fatalf("second email has %s, first %s", again, first)
	}
	if _, err := s.EnterCode(ctx, ch2, first, "", ""); err != nil {
		t.Fatal(err)
	}
	// Used: neither challenge takes it again.
	if _, err := s.EnterCode(ctx, ch1, first, "", ""); err == nil {
		t.Fatal("a code signed in twice")
	}
}

// Unknown usernames and bots get a challenge like anyone, and no email.
// (An unknown address gets a new-account code: TestNewAccountByEmail.)
func TestSignInUnknown(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	s.BotKey = "0123456789abcdef"
	if _, err := s.BotLogin(ctx, "robot", "", s.BotKey); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nobody", "not-an-address@", "robot", "", "x y"} {
		ch, err := s.NewChallenge()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RequestSignIn(ctx, ch, name); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if _, err := s.EnterCode(ctx, ch, "12345678", "", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%q: code %v", name, err)
		}
	}
	if mb.count() != 0 {
		t.Fatalf("%d emails sent", mb.count())
	}
}

// An address shared by several accounts gets one email with a code for
// each, and each code signs in its own account.
func TestSharedAddress(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	a := register(t, s, mb, "grandma", "family@example.com")
	b := register(t, s, mb, "grandpa", "Family@Example.com")
	n := mb.count()
	ch, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch, "family@example.com")
	if mb.count() != n+1 {
		t.Fatalf("%d emails for a shared address", mb.count()-n)
	}
	body := mb.last(t).body
	cs := codes(body)
	// A code for each account, and one for a new account.
	if len(cs) != 3 || !strings.Contains(body, "grandma") || !strings.Contains(body, "grandpa") || len(links(body, "signin")) != 3 {
		t.Fatalf("shared email: %q", body)
	}
	res, err := s.EnterCode(ctx, ch, cs[1], "", "")
	if err != nil || res.User.ID != b.User.ID {
		t.Fatalf("second code: %+v, %v", res, err)
	}
	_ = a

	// The third code makes another account with the address.
	ch, _ = s.NewChallenge()
	s.RequestSignIn(ctx, ch, "family@example.com")
	cs = codes(mb.last(t).body)
	var needs *NeedsUsername
	if _, err := s.EnterCode(ctx, ch, cs[2], "", ""); !errors.As(err, &needs) || needs.Email != "family@example.com" {
		t.Fatalf("new-account code: %v", err)
	}
	res, err = s.EnterCode(ctx, ch, cs[2], "grandkid", "Kiddo")
	if err != nil || res.User.Email != "family@example.com" || res.User.Nickname != "Kiddo" {
		t.Fatalf("new account: %+v, %v", res, err)
	}
	if us, _ := s.Store.UsersByEmail(ctx, "family@example.com"); len(us) != 3 {
		t.Fatalf("%d accounts share the address", len(us))
	}
}

// TestNewAccountByEmail checks the one-box sign-in: an address with no
// account gets a code that creates one, named once the code is in.
func TestNewAccountByEmail(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	register(t, s, mb, "taken", "someone@example.com")
	n := mb.count()

	ch, _ := s.NewChallenge()
	if found, err := s.RequestSignIn(ctx, ch, " New@Example.com "); err != nil || found != 0 {
		t.Fatalf("RequestSignIn: %d, %v", found, err)
	}
	if mb.count() != n+1 {
		t.Fatal("no email to a new address")
	}
	m := mb.last(t)
	cs := codes(m.body)
	if m.to != "New@Example.com" || len(cs) != 1 || !strings.Contains(m.body, "create a cribbage account") || !strings.Contains(m.subject, cs[0][:4]) {
		t.Fatalf("new-account email to %s: %q", m.to, m.body)
	}
	// Asked again soon: the same code, so the first email still works.
	ch2, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch2, "new@example.com")
	if codes(mb.last(t).body)[0] != cs[0] {
		t.Fatal("a second email changed the code")
	}

	var needs *NeedsUsername
	if _, err := s.EnterCode(ctx, ch, cs[0], "", ""); !errors.As(err, &needs) {
		t.Fatalf("no username: %v", err)
	}
	// A bad or taken name leaves the code good.
	if _, err := s.EnterCode(ctx, ch, cs[0], "a", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid username: %v", err)
	}
	if _, err := s.EnterCode(ctx, ch, cs[0], "TAKEN", ""); !errors.Is(err, store.ErrUsernameTaken) {
		t.Fatalf("taken username: %v", err)
	}
	res, err := s.EnterCode(ctx, ch, cs[0], "newbie", "")
	if err != nil || res.User.Username != "newbie" || res.User.Nickname != "newbie" || res.User.Email != "New@Example.com" || res.Token == "" {
		t.Fatalf("created: %+v, %v", res, err)
	}
	// Used up: neither the code (through the other challenge) nor the link
	// makes a second account.
	if _, err := s.EnterCode(ctx, ch2, cs[0], "newbie2", ""); err == nil {
		t.Fatal("code made a second account")
	}
	if _, err := s.UseSignInLink(ctx, links(m.body, "signin")[0], "newbie3", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("link after the code: %v", err)
	}
	// Next time, the address signs in to its account.
	ch, _ = s.NewChallenge()
	if found, _ := s.RequestSignIn(ctx, ch, "new@example.com"); found != 1 {
		t.Fatalf("found %d accounts", found)
	}
}

func TestNewAccountByLink(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	ch, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch, "linky@example.com")
	link := links(mb.last(t).body, "signin")[0]
	var needs *NeedsUsername
	if _, err := s.UseSignInLink(ctx, link, "", ""); !errors.As(err, &needs) || needs.Email != "linky@example.com" {
		t.Fatalf("link without a username: %v", err)
	}
	res, err := s.UseSignInLink(ctx, link, "linky", "Link")
	if err != nil || res.User.Username != "linky" {
		t.Fatalf("link with a username: %+v, %v", res, err)
	}
	if _, err := s.UseSignInLink(ctx, link, "linky2", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("link used twice: %v", err)
	}
}

// TestNewAccountCodeLimit checks that new-account emails count against
// the cap on registration emails from everyone.
func TestNewAccountCodeLimit(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	for i := 0; i < 25; i++ {
		ch, _ := s.NewChallenge()
		s.RequestSignIn(ctx, ch, fmt.Sprintf("person%d@example.com", i))
	}
	if mb.count() != 20 {
		t.Fatalf("%d new-account emails, want 20", mb.count())
	}
}

func TestWrongCodes(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	register(t, s, mb, "alice", "alice@example.com")
	ch, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch, "alice")
	code := codes(mb.last(t).body)[0]
	wrong := "00000000"
	if wrong == code {
		wrong = "11111111"
	}
	for i := 1; i < triesPerChallenge; i++ {
		if _, err := s.EnterCode(ctx, ch, wrong, "", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, err := s.EnterCode(ctx, ch, wrong, "", ""); !errors.Is(err, ErrTooManyTries) {
		t.Fatalf("last wrong code: %v", err)
	}
	if _, err := s.EnterCode(ctx, ch, code, "", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("right code after too many wrong: %v", err)
	}
	// The account's limit spans challenges: after ten wrong codes in all,
	// even the right one is refused for a while. The link still works.
	ch2, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch2, "alice")
	for i := 0; i < 5; i++ {
		s.EnterCode(ctx, ch2, wrong, "", "")
	}
	ch3, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch3, "alice")
	body := mb.last(t).body
	if _, err := s.EnterCode(ctx, ch3, codes(body)[0], "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("right code past the account's limit: %v", err)
	}
	if _, err := s.UseSignInLink(ctx, links(body, "signin")[0], "", ""); err != nil {
		t.Fatalf("link past the code limit: %v", err)
	}
}

// A code sent to an old address stops working when the address changes.
func TestCodeForOldAddress(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	res := register(t, s, mb, "alice", "old@example.com")
	ch, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch, "alice")
	code := codes(mb.last(t).body)[0]
	if err := s.Store.SetEmail(ctx, res.User.ID, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnterCode(ctx, ch, code, "", ""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("code for the old address: %v", err)
	}
}

// Changing the address takes a link to the current address, then one to
// the new address; the old address is told.
func TestChangeEmail(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	res := register(t, s, mb, "alice", "old@example.com")
	u := res.User
	if err := s.SetEmail(ctx, u, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	m := mb.last(t)
	if m.to != "old@example.com" || len(links(m.body, "change")) != 1 || !strings.Contains(m.body, "new@example.com") {
		t.Fatalf("first email: %+v", m)
	}
	if a, _ := s.Account(ctx, u); a.PendingEmail != "new@example.com" || !a.PendingNeedsCurrent || a.Email != "old@example.com" {
		t.Fatalf("account while waiting for the current address: %+v", a)
	}
	id, changed, err := s.VerifyEmail(ctx, links(m.body, "change")[0])
	if err != nil || id != u.ID || changed {
		t.Fatalf("first link: %d %v %v", id, changed, err)
	}
	m = mb.last(t)
	if m.to != "new@example.com" || len(links(m.body, "verify")) != 1 {
		t.Fatalf("second email: %+v", m)
	}
	if a, _ := s.Account(ctx, u); a.PendingEmail != "new@example.com" || a.PendingNeedsCurrent {
		t.Fatalf("account while waiting for the new address: %+v", a)
	}
	if _, changed, err = s.VerifyEmail(ctx, links(m.body, "verify")[0]); err != nil || !changed {
		t.Fatalf("second link: %v %v", changed, err)
	}
	if got, _ := s.Store.UserByID(ctx, u.ID); got.Email != "new@example.com" {
		t.Fatalf("address is %q", got.Email)
	}
	if m = mb.last(t); m.to != "old@example.com" || !strings.Contains(m.body, "n***@example.com") {
		t.Fatalf("notice to the old address: %+v", m)
	}
	if _, _, err := s.VerifyEmail(ctx, strings.Repeat("ab", 32)); !errors.Is(err, ErrBadToken) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestResumeRenews(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	s.SessionTTL = 10 * 24 * time.Hour
	res := register(t, s, mb, "alice", "alice@example.com")
	// Pretend the session is about to expire.
	if err := s.Store.ExtendSession(ctx, res.Token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	_, exp, _ := s.Store.SessionUser(ctx, res.Token)
	if time.Until(exp) < 9*24*time.Hour {
		t.Fatalf("session expires in %v after use", time.Until(exp))
	}
	if err := s.SignOutOthers(ctx, res.User.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(ctx, res.Token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("resume after signing out: %v", err)
	}
}

func TestBotLogin(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	if _, err := s.BotLogin(ctx, "robot", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("no bot key configured: %v", err)
	}
	s.BotKey = "0123456789abcdef"
	if _, err := s.BotLogin(ctx, "robot", "", "wrong-key-wrong-key"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong key: %v", err)
	}
	res, err := s.BotLogin(ctx, "robot", "Robbie", s.BotKey)
	if err != nil || !res.User.Bot || res.User.Nickname != "Robbie" || res.User.Email != "" {
		t.Fatalf("bot: %+v, %v", res, err)
	}
	again, err := s.BotLogin(ctx, "ROBOT", "", s.BotKey)
	if err != nil || again.User.ID != res.User.ID {
		t.Fatalf("second login: %+v, %v", again, err)
	}
	// A person's account cannot be signed in to with the bot key.
	register(t, s, mb, "alice", "alice@example.com")
	if _, err := s.BotLogin(ctx, "alice", "", s.BotKey); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bot key on a person's account: %v", err)
	}
	// A bot deletes itself by typing its name; a person by email.
	if mailed, err := s.RequestDelete(ctx, res.User, "robot"); err != nil || mailed {
		t.Fatalf("delete bot: %v %v", mailed, err)
	}
	if _, err := s.RequestDelete(ctx, res.User, "other"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("delete bot with the wrong name: %v", err)
	}
}

func TestAdminSetEmail(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	res := register(t, s, mb, "alice", "thief@example.com")
	if err := s.Store.AddPasskey(ctx, &store.Passkey{ID: []byte("k"), UserID: res.User.ID, Data: []byte("{}"), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	old, err := s.AdminSetEmail(ctx, "alice", "owner@example.com")
	if err != nil || old.Email != "thief@example.com" {
		t.Fatalf("admin: %+v, %v", old, err)
	}
	if _, err := s.Resume(ctx, res.Token); err == nil {
		t.Fatal("session survived")
	}
	if keys, _ := s.Store.Passkeys(ctx, res.User.ID); len(keys) != 0 {
		t.Fatal("passkey survived")
	}
	if m := mb.last(t); m.to != "thief@example.com" {
		t.Fatalf("old address not told: %+v", m)
	}
	if _, err := s.AdminSetEmail(ctx, "alice", "nope"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad address: %v", err)
	}
	if _, err := s.AdminSetEmail(ctx, "nobody", "a@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestPasskeys(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	wa, err := NewWebAuthn("https://crib.example")
	if err != nil {
		t.Fatal(err)
	}
	s.WebAuthn = wa
	alice := register(t, s, mb, "alice", "alice@example.com").User
	bob := register(t, s, mb, "bob", "bob@example.com").User
	dev := &passkeytest.Authenticator{Origin: "https://crib.example"}

	add := func(u *store.User, name string) error {
		opts, sd, err := s.BeginAddPasskey(ctx, u)
		if err != nil {
			return err
		}
		cred, err := dev.Create(opts)
		if err != nil {
			return err
		}
		return s.FinishAddPasskey(ctx, u, "", sd, cred, name)
	}
	if err := add(alice, "Phone"); err != nil {
		t.Fatal(err)
	}
	if err := add(bob, ""); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Account(ctx, alice)
	if !a.PasskeysEnabled || len(a.Passkeys) != 1 || a.Passkeys[0].Name != "Phone" {
		t.Fatalf("account: %+v", a)
	}

	signIn := func(i int) (*Result, error) {
		opts, sd, err := s.BeginPasskeySignIn()
		if err != nil {
			return nil, err
		}
		cred, err := dev.Get(opts, i)
		if err != nil {
			return nil, err
		}
		return s.FinishPasskeySignIn(ctx, sd, cred)
	}
	res, err := signIn(0)
	if err != nil || res.User.ID != alice.ID {
		t.Fatalf("alice's passkey: %+v, %v", res, err)
	}
	if res, err := signIn(1); err != nil || res.User.ID != bob.ID {
		t.Fatalf("bob's passkey: %+v, %v", res, err)
	}
	if keys, _ := s.Store.Passkeys(ctx, alice.ID); keys[0].LastUsed.IsZero() {
		t.Fatal("last use not recorded")
	}

	// An answer is good for its own ceremony only.
	opts, sd, _ := s.BeginPasskeySignIn()
	cred, _ := dev.Get(opts, 0)
	if _, err := s.FinishPasskeySignIn(ctx, sd, cred); err != nil {
		t.Fatal(err)
	}
	_, sd2, _ := s.BeginPasskeySignIn()
	if _, err := s.FinishPasskeySignIn(ctx, sd2, cred); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("replayed answer: %v", err)
	}
	// Alice's key claiming to be Bob's is refused.
	dev.SetUserHandle(0, userHandle(bob.ID))
	if _, err := signIn(0); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("key claiming another user: %v", err)
	}
	dev.SetUserHandle(0, userHandle(alice.ID))
	// A passkey for another site is refused.
	other := &passkeytest.Authenticator{Origin: "https://evil.example"}
	opts, sd, _ = s.BeginAddPasskey(ctx, alice)
	if c, err := other.Create(opts); err == nil {
		if err := s.FinishAddPasskey(ctx, alice, "", sd, c, "x"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("passkey from another origin: %v", err)
		}
	}
	if _, err := s.FinishPasskeySignIn(ctx, nil, "{}"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("no ceremony: %v", err)
	}

	// Removing a passkey stops it working.
	keys, _ := s.Store.Passkeys(ctx, alice.ID)
	if err := s.RemovePasskey(ctx, bob.ID, PasskeyID(keys[0].ID)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bob removing alice's passkey: %v", err)
	}
	if err := s.RemovePasskey(ctx, alice.ID, PasskeyID(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := signIn(0); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("removed passkey: %v", err)
	}
}

var stopRE = regexp.MustCompile(`https://crib\.example/#stop=([0-9a-f]{96})`)

// TestStopEmail checks the link in every email that stops email to its
// address: after it, nothing more is sent there, by any route, and
// nothing is deleted.
func TestStopEmail(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	s.StopURL = "https://api.crib.example/api/stop"
	register(t, s, mb, "alice", "alice@example.com")
	m := mb.last(t)
	stop := stopRE.FindStringSubmatch(m.body)
	if stop == nil {
		t.Fatalf("no stop link in:\n%s", m.body)
	}
	if err := s.StopEmail(ctx, stop[1][:95]+"x"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("forged token: %v", err)
	}
	if err := s.StopEmail(ctx, stop[1]); err != nil {
		t.Fatal(err)
	}

	n := mb.count()
	ch, _ := s.NewChallenge()
	if found, err := s.RequestSignIn(ctx, ch, "alice"); err != nil || found != 1 {
		t.Fatalf("sign-in to a stopped address: %d, %v", found, err)
	}
	ch, _ = s.NewChallenge()
	if found, err := s.RequestSignIn(ctx, ch, "Alice+cribbage@example.com"); err != nil || found != 0 {
		t.Fatalf("new account at a stopped address's +tag variant: %d, %v", found, err)
	}
	if mb.count() != n {
		t.Fatal("an email went to a stopped address")
	}
	bob := register(t, s, mb, "bob", "bob@example.com")
	n = mb.count()
	if err := s.SetEmail(ctx, bob.User, "alice@example.com"); !errors.Is(err, ErrEmailStopped) {
		t.Fatalf("change to a stopped address: %v", err)
	}
	if mb.count() != n {
		t.Fatal("an email went to a stopped address")
	}
	if u, err := s.Store.UserByUsername(ctx, "alice"); err != nil || u.Email != "alice@example.com" {
		t.Fatalf("stopping email changed the account: %+v, %v", u, err)
	}

	// Whoever runs the server can undo it.
	if was, err := s.AllowEmail(ctx, "alice@example.com"); err != nil || !was {
		t.Fatalf("AllowEmail: %v, %v", was, err)
	}
	ch, _ = s.NewChallenge()
	s.RequestSignIn(ctx, ch, "alice")
	if mb.count() != n+1 || mb.last(t).to != "alice@example.com" {
		t.Fatal("no email after AllowEmail")
	}
}

func TestStopLinkHeader(t *testing.T) {
	s, _ := newService(t)
	rec := &recordingMailer{}
	s.Mailer = rec
	s.StopURL = "https://api.crib.example/api/stop"
	ch, _ := s.NewChallenge()
	if _, err := s.RequestSignIn(context.Background(), ch, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	stop := stopRE.FindStringSubmatch(rec.m.Body)
	if stop == nil || rec.m.Unsubscribe != s.StopURL+"?t="+stop[1] {
		t.Fatalf("unsubscribe %q for body:\n%s", rec.m.Unsubscribe, rec.m.Body)
	}
}

type recordingMailer struct{ m mail.Message }

func (r *recordingMailer) Send(_ context.Context, m mail.Message) error { r.m = m; return nil }

// TestMailboxLimit checks that +tag variants of an address share its
// email limit, so they cannot be used to flood one inbox.
func TestMailboxLimit(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	// Accounts made directly, so the cap on new-account emails plays no part.
	for i := 0; i < 25; i++ {
		u := &store.User{Username: fmt.Sprintf("user%d", i), Nickname: "v", Email: fmt.Sprintf("Victim+%d@example.com", i), Created: time.Now()}
		if err := s.Store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		ch, _ := s.NewChallenge()
		if _, err := s.RequestSignIn(ctx, ch, u.Username); err != nil {
			t.Fatal(err)
		}
	}
	if mb.count() != 20 {
		t.Fatalf("%d emails, want 20", mb.count())
	}
}
