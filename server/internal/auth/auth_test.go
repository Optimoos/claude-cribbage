package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cribbage/internal/auth/passkeytest"
	"cribbage/internal/store"
)

type sent struct{ to, subject, body string }

// mailbox records emails instead of sending them.
type mailbox struct {
	mu   sync.Mutex
	msgs []sent
}

func (m *mailbox) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, sent{to, subject, body})
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

// register creates an account through its emailed code.
func register(t *testing.T, s *Service, mb *mailbox, user, email string) *Result {
	t.Helper()
	ctx := context.Background()
	ch, err := s.Register(ctx, user, "", email)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.EnterCode(ctx, ch, codes(mb.last(t).body)[0])
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRegisterAndSignIn(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	ch, err := s.Register(ctx, "alice", "Al", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	m := mb.last(t)
	if m.to != "alice@example.com" || len(codes(m.body)) != 1 || !strings.Contains(m.subject, codes(m.body)[0][:4]) {
		t.Fatalf("registration email: %+v", m)
	}
	// Nothing exists until the code is entered.
	if _, err := s.Store.UserByUsername(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account created early: %v", err)
	}
	if _, err := s.EnterCode(ctx, ch, "0000 0000"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong code: %v", err)
	}
	res, err := s.EnterCode(ctx, ch, " "+strings.ReplaceAll(codes(m.body)[0], "", "-")+" ")
	if err != nil {
		t.Fatalf("code with dashes and spaces: %v", err)
	}
	if res.User.Username != "alice" || res.User.Nickname != "Al" || res.User.Email != "alice@example.com" || res.Token == "" {
		t.Fatalf("registered: %+v", res.User)
	}
	if _, err := s.EnterCode(ctx, ch, codes(m.body)[0]); !errors.Is(err, ErrBadToken) {
		t.Fatalf("code used twice: %v", err)
	}
	if _, err := s.Register(ctx, "ALICE", "", "other@example.com"); !errors.Is(err, store.ErrUsernameTaken) {
		t.Fatalf("taken username: %v", err)
	}
	for _, bad := range [][3]string{{"a", "", "x@example.com"}, {"bob", "", "not-an-address"}, {"bob", "\x01", "b@example.com"}} {
		if _, err := s.Register(ctx, bad[0], bad[1], bad[2]); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("register %q: %v", bad, err)
		}
	}

	// Sign in by username: a code, and a link that works instead.
	ch, _ = s.NewChallenge()
	if err := s.RequestSignIn(ctx, ch, "Alice"); err != nil {
		t.Fatal(err)
	}
	m = mb.last(t)
	if len(codes(m.body)) != 1 || len(links(m.body, "signin")) != 1 {
		t.Fatalf("sign-in email: %q", m.body)
	}
	res2, err := s.UseSignInLink(ctx, links(m.body, "signin")[0])
	if err != nil || res2.User.ID != res.User.ID || res2.Token == res.Token {
		t.Fatalf("link: %+v, %v", res2, err)
	}
	// The link and code are one: using either uses up both.
	if _, err := s.EnterCode(ctx, ch, codes(m.body)[0]); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("code after its link: %v", err)
	}
	if _, err := s.UseSignInLink(ctx, links(m.body, "signin")[0]); !errors.Is(err, ErrBadToken) {
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
	if _, err := s.EnterCode(ctx, ch2, first); err != nil {
		t.Fatal(err)
	}
	// Used: neither challenge takes it again.
	if _, err := s.EnterCode(ctx, ch1, first); err == nil {
		t.Fatal("a code signed in twice")
	}
}

// Unknown names and bots get a challenge like anyone, and no email.
func TestSignInUnknown(t *testing.T) {
	ctx := context.Background()
	s, mb := newService(t)
	s.BotKey = "0123456789abcdef"
	if _, err := s.BotLogin(ctx, "robot", "", s.BotKey); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nobody", "nobody@example.com", "robot", "", "x y"} {
		ch, err := s.NewChallenge()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RequestSignIn(ctx, ch, name); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if _, err := s.EnterCode(ctx, ch, "12345678"); !errors.Is(err, ErrInvalidCredentials) {
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
	if len(cs) != 2 || !strings.Contains(body, "grandma") || !strings.Contains(body, "grandpa") || len(links(body, "signin")) != 2 {
		t.Fatalf("shared email: %q", body)
	}
	res, err := s.EnterCode(ctx, ch, cs[1])
	if err != nil || res.User.ID != b.User.ID {
		t.Fatalf("second code: %+v, %v", res, err)
	}
	_ = a
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
		if _, err := s.EnterCode(ctx, ch, wrong); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, err := s.EnterCode(ctx, ch, wrong); !errors.Is(err, ErrTooManyTries) {
		t.Fatalf("last wrong code: %v", err)
	}
	if _, err := s.EnterCode(ctx, ch, code); !errors.Is(err, ErrBadToken) {
		t.Fatalf("right code after too many wrong: %v", err)
	}
	// The account's limit spans challenges: after ten wrong codes in all,
	// even the right one is refused for a while. The link still works.
	ch2, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch2, "alice")
	for i := 0; i < 5; i++ {
		s.EnterCode(ctx, ch2, wrong)
	}
	ch3, _ := s.NewChallenge()
	s.RequestSignIn(ctx, ch3, "alice")
	body := mb.last(t).body
	if _, err := s.EnterCode(ctx, ch3, codes(body)[0]); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("right code past the account's limit: %v", err)
	}
	if _, err := s.UseSignInLink(ctx, links(body, "signin")[0]); err != nil {
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
	if _, err := s.EnterCode(ctx, ch, code); !errors.Is(err, ErrBadToken) {
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
		return s.FinishAddPasskey(ctx, u, sd, cred, name)
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
		if err := s.FinishAddPasskey(ctx, alice, sd, c, "x"); !errors.Is(err, ErrInvalidInput) {
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
