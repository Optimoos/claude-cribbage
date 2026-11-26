package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cribbage/internal/mail"
	"cribbage/internal/store"
)

// Signing in by email: the server mails a code, which the client that
// asked enters with the challenge it was given, and a link, which signs in
// whichever browser opens it. A challenge is a secret held by the client
// that asked, so a code can only be guessed through a challenge, a few
// times each, and each account's wrong codes are limited too. Codes and
// challenges are kept in memory only: they last minutes, and a restart
// just means asking again.

// CodeLength is the number of digits in a sign-in code.
const CodeLength = 8

// ErrTooManyTries means a challenge had too many wrong codes.
var ErrTooManyTries = errors.New("too many wrong codes; ask for a new code")

const (
	triesPerChallenge = 5
	maxChallenges     = 100000 // live challenges, so they cannot exhaust memory
)

// ticket is one account's code and link, or a new account's (fresh), whose
// username is chosen once the code is in.
type ticket struct {
	userID  int64  // 0 for a fresh account
	fresh   bool   // creates an account at email, named when used
	email   string // where it was sent
	code    string
	link    string
	expires time.Time
	used    bool
}

type challenge struct {
	tickets []*ticket
	wrong   int
	expires time.Time
}

// CodeLifetime is how long a sign-in code and link work.
const CodeLifetime = 15 * time.Minute

func (s *Service) codeTTL() time.Duration { return CodeLifetime }

// newCode is CodeLength random digits.
func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%08d", n.Int64())
}

// showCode writes a code in two groups, for reading: "1234 5678".
func showCode(c string) string { return c[:4] + " " + c[4:] }

// normalCode keeps only the digits of a typed code.
func normalCode(c string) string {
	var b strings.Builder
	for _, r := range c {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sweep forgets expired challenges and tickets. Callers hold mu.
func (s *Service) sweep(now time.Time) {
	s.swept = now
	for id, c := range s.challenges {
		if now.After(c.expires) {
			delete(s.challenges, id)
		}
	}
	for id, t := range s.tickets {
		if now.After(t.expires) {
			delete(s.tickets, id)
		}
	}
	for k, t := range s.fresh {
		if now.After(t.expires) || t.used {
			delete(s.fresh, k)
		}
	}
	for l, t := range s.links {
		if now.After(t.expires) || t.used {
			delete(s.links, l)
		}
	}
}

// newChallenge stores an empty challenge. Callers hold mu.
func (s *Service) newChallenge(now time.Time) (string, *challenge, error) {
	if now.Sub(s.swept) > time.Minute || len(s.challenges) >= maxChallenges {
		s.sweep(now)
	}
	if len(s.challenges) >= maxChallenges {
		return "", nil, ErrMailLimit
	}
	id := RandomID(tokenBytes)
	c := &challenge{expires: now.Add(s.codeTTL())}
	s.challenges[id] = c
	return id, c, nil
}

// newTicket makes a ticket and indexes its link. Callers hold mu.
func (s *Service) newTicket(now time.Time, email string) *ticket {
	t := &ticket{email: email, code: newCode(), link: RandomID(tokenBytes), expires: now.Add(s.codeTTL())}
	s.links[t.link] = t
	return t
}

// NewChallenge starts a sign-in, returning the challenge for the client;
// RequestSignIn then finds the accounts and mails their codes.
func (s *Service) NewChallenge() (string, error) {
	if !s.MailEnabled() {
		return "", ErrMailDisabled
	}
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _, err := s.newChallenge(time.Now())
	return id, err
}

// RequestSignIn mails a code and link to the account with this username,
// or to each account with this email address, attaches the codes to the
// challenge, and reports how many accounts it found. Given an address, the
// email also has a code that creates a new account with it (the only one,
// if no account has it): see EnterCode. A name with no account is not an
// error, and nor is an address that asked for no email or has had too
// many. A code asked for again while it still has most of its time left is
// sent again, unchanged, so a second email does not make the first one's
// code wrong.
func (s *Service) RequestSignIn(ctx context.Context, challengeID, name string) (int, error) {
	if !s.MailEnabled() {
		return 0, ErrMailDisabled
	}
	s.init()
	name = strings.TrimSpace(name)
	users, err := s.usersNamed(ctx, name)
	if err != nil {
		return 0, err
	}
	byEmail := strings.Contains(name, "@") && mail.ValidAddress(name)
	if len(users) == 0 && !byEmail {
		return 0, nil
	}
	to := name
	if len(users) > 0 {
		to = users[0].Email // as verified, not as typed
	}
	now := time.Now()
	s.mu.Lock()
	c := s.challenges[challengeID]
	if c == nil {
		s.mu.Unlock()
		return len(users), nil // expired meanwhile
	}
	stale := func(t *ticket) bool {
		return t == nil || t.used || t.expires.Sub(now) < s.codeTTL()*2/3
	}
	var mine []signInCode
	for _, u := range users {
		t := s.tickets[u.ID]
		if stale(t) || !strings.EqualFold(t.email, u.Email) {
			t = s.newTicket(now, u.Email)
			t.userID = u.ID
			s.tickets[u.ID] = t
		}
		mine = append(mine, signInCode{u, t})
	}
	var fresh *ticket
	if byEmail {
		key := strings.ToLower(name)
		fresh = s.fresh[key]
		if stale(fresh) {
			fresh = s.newTicket(now, name)
			fresh.fresh = true
			s.fresh[key] = fresh
		}
	}
	s.mu.Unlock()

	var allowed []signInCode
	for _, e := range mine {
		if s.mailLimit.Take(strconv.FormatInt(e.u.ID, 10), now) {
			allowed = append(allowed, e)
		}
	}
	switch {
	case len(users) > 0 && len(allowed) == 0:
		s.log().Info("email not sent: too many for this account lately", "kind", "sign-in code")
		return len(users), nil
	case len(users) == 0 && !s.regLimit.Take("", now):
		// Only a new-account code: mail to an address nobody has shown is
		// theirs, limited for everyone together (regLimit).
		s.log().Warn("email not sent: too many new accounts from everyone lately", "kind", "new-account code")
		return 0, nil
	}
	if !s.rcptLimit.Take(mail.Mailbox(to), now) {
		s.log().Info("email not sent: too many for this address lately", "kind", "sign-in code")
		return len(users), nil
	}

	// The codes go on the challenge only once they are mailed.
	s.mu.Lock()
	if c := s.challenges[challengeID]; c != nil {
		for _, e := range allowed {
			c.tickets = append(c.tickets, e.t)
		}
		if fresh != nil {
			c.tickets = append(c.tickets, fresh)
		}
	}
	s.mu.Unlock()

	kind, subject, body := s.signInEmail(allowed, fresh)
	if err := s.send(ctx, kind, to, subject, body); err != nil && !errors.Is(err, ErrEmailStopped) {
		return len(users), fmt.Errorf("send %s: %w", kind, err)
	}
	return len(users), nil
}

type signInCode struct {
	u *store.User
	t *ticket
}

// signInEmail writes the email with accounts' sign-in codes and, if fresh
// is set, the code that creates a new account.
func (s *Service) signInEmail(codes []signInCode, fresh *ticket) (kind, subject, body string) {
	ttl := duration(s.codeTTL())
	var b strings.Builder
	switch len(codes) {
	case 0:
		kind, subject = "new-account code", "Your Cribbage code: "+showCode(fresh.code)
		fmt.Fprintf(&b, `Hello,

Your code to create a cribbage account with this address is:

    %s

Enter it in the game within %s, then choose a username. Or, to do it with the web browser on this device, open this link:

%s

If you did not ask for an account, ignore this email and none will be made.
`, showCode(fresh.code), ttl, s.link("signin", fresh.link))
		return kind, subject, b.String()
	case 1:
		e := codes[0]
		kind, subject = "sign-in code", "Your Cribbage sign-in code: "+showCode(e.t.code)
		fmt.Fprintf(&b, `Hello %s,

Your code to sign in to the cribbage account "%s" is:

    %s

Enter it in the game within %s. Or, to sign in with the web browser on this device, open this link:

%s
`, e.u.Nickname, e.u.Username, showCode(e.t.code), ttl, s.link("signin", e.t.link))
	default:
		kind, subject = "sign-in code", "Your Cribbage sign-in codes"
		fmt.Fprintf(&b, "Hello,\n\nThis address belongs to %d cribbage accounts. Enter the code of the one you want within %s, or open its link to sign in with the web browser on this device.\n", len(codes), ttl)
		for _, e := range codes {
			fmt.Fprintf(&b, "\n%s (%s):\n    %s\n    %s\n", e.u.Username, e.u.Nickname, showCode(e.t.code), s.link("signin", e.t.link))
		}
	}
	if fresh != nil {
		fmt.Fprintf(&b, "\nTo create another account with this address instead, enter this code and then choose a username:\n    %s\n    %s\n", showCode(fresh.code), s.link("signin", fresh.link))
	}
	b.WriteString("\nIf you did not ask to sign in, ignore this email. Nobody can sign in without a code or a link.\n")
	return kind, subject, b.String()
}

// usersNamed finds the account with a username, or the accounts with an
// address. Bots, which have no address, are left out.
func (s *Service) usersNamed(ctx context.Context, name string) ([]*store.User, error) {
	var users []*store.User
	if strings.Contains(name, "@") {
		if !mail.ValidAddress(name) {
			return nil, nil
		}
		var err error
		if users, err = s.Store.UsersByEmail(ctx, name); err != nil {
			return nil, err
		}
	} else if ValidUsername(name) {
		u, err := s.Store.UserByUsername(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	out := users[:0]
	for _, u := range users {
		if u.Email != "" && !u.Bot {
			out = append(out, u)
		}
	}
	return out, nil
}

// NeedsUsername is the error from a code or link that creates an account
// (a fresh one) given without a username. The code or link stays good:
// give it again with one.
type NeedsUsername struct {
	Email string // the address the account gets
}

func (e *NeedsUsername) Error() string { return "choose a username for the new account" }

// EnterCode signs in (or creates an account) with a challenge and the
// code mailed for it. A code that creates an account needs a username (and
// optionally a nickname): without one it returns *NeedsUsername, and the
// code stays good, as it does if the username is taken or invalid.
func (s *Service) EnterCode(ctx context.Context, challengeID, code, username, nickname string) (*Result, error) {
	s.init()
	code = normalCode(code)
	now := time.Now()
	s.mu.Lock()
	c := s.challenges[challengeID]
	if c == nil || now.After(c.expires) {
		s.mu.Unlock()
		return nil, ErrBadToken
	}
	var match *ticket
	var live []*ticket
	for _, t := range c.tickets {
		if t.used || now.After(t.expires) {
			continue
		}
		live = append(live, t)
		// An account that has had too many wrong codes takes none, right
		// or wrong, until its limit refills. Its owner can use the link.
		if t.userID != 0 && !s.badCodes.Allow(strconv.FormatInt(t.userID, 10), now) {
			continue
		}
		if len(code) == CodeLength && subtle.ConstantTimeCompare([]byte(code), []byte(t.code)) == 1 {
			match = t
		}
	}
	if match == nil {
		c.wrong++
		for _, t := range live {
			if t.userID != 0 {
				s.badCodes.Take(strconv.FormatInt(t.userID, 10), now)
			}
		}
		if c.wrong >= triesPerChallenge {
			delete(s.challenges, challengeID)
			s.mu.Unlock()
			return nil, ErrTooManyTries
		}
		s.mu.Unlock()
		return nil, ErrWrongCode
	}
	if match.fresh {
		s.mu.Unlock()
		return s.createFresh(ctx, match, username, nickname, func() { delete(s.challenges, challengeID) })
	}
	s.use(match)
	delete(s.challenges, challengeID)
	s.mu.Unlock()
	return s.complete(ctx, match)
}

// createFresh creates the account a fresh ticket was mailed for, named
// username, and signs it in; done runs (holding mu) once it is created.
// The ticket stays good if the name is missing, invalid or taken.
func (s *Service) createFresh(ctx context.Context, t *ticket, username, nickname string, done func()) (*Result, error) {
	username, nickname = strings.TrimSpace(username), strings.TrimSpace(nickname)
	if username == "" {
		return nil, &NeedsUsername{Email: t.email}
	}
	if nickname == "" {
		nickname = username
	}
	if err := validUsername(username); err != nil {
		return nil, err
	}
	if err := validNickname(nickname); err != nil {
		return nil, err
	}
	// Take the ticket while creating, so it makes one account however many
	// times it is sent at once, and give it back if that fails.
	s.mu.Lock()
	if t.used || time.Now().After(t.expires) {
		s.mu.Unlock()
		return nil, ErrBadToken
	}
	s.use(t)
	s.mu.Unlock()
	u := &store.User{Username: username, Nickname: nickname, Email: t.email, Created: time.Now()}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		s.mu.Lock()
		t.used = false
		s.links[t.link] = t
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	done()
	s.mu.Unlock()
	s.log().Info("account created", "user", u.ID)
	return s.SignIn(ctx, u)
}

// UseSignInLink signs in (or creates an account) with a link's token. A
// link that creates an account needs a username, as its code does
// (EnterCode).
func (s *Service) UseSignInLink(ctx context.Context, token, username, nickname string) (*Result, error) {
	if !validToken(token) {
		return nil, ErrBadToken
	}
	s.init()
	s.mu.Lock()
	t := s.links[token]
	if t == nil || t.used || time.Now().After(t.expires) {
		s.mu.Unlock()
		return nil, ErrBadToken
	}
	if t.fresh {
		s.mu.Unlock()
		return s.createFresh(ctx, t, username, nickname, func() {})
	}
	s.use(t)
	s.mu.Unlock()
	return s.complete(ctx, t)
}

// use marks a ticket used. Callers hold mu.
func (s *Service) use(t *ticket) {
	t.used = true
	delete(s.links, t.link)
	if t.userID != 0 && s.tickets[t.userID] == t {
		delete(s.tickets, t.userID)
	}
	if t.fresh && s.fresh[strings.ToLower(t.email)] == t {
		delete(s.fresh, strings.ToLower(t.email))
	}
}

// complete checks that a sign-in's account still has the address the code
// went to, and starts a session.
func (s *Service) complete(ctx context.Context, t *ticket) (*Result, error) {
	u, err := s.Store.UserByID(ctx, t.userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Email, t.email) {
		return nil, ErrBadToken // the address changed since
	}
	return s.SignIn(ctx, u)
}
