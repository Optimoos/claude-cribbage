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

// ticket is one account's code and link, or a registration's.
type ticket struct {
	userID  int64       // 0 for a registration
	reg     *store.User // the account a registration creates
	email   string      // where it was sent
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
// or to each account with this email address, and attaches the codes to
// the challenge. A name with no account is not an error. An account's
// code is sent again, unchanged, if it is asked for while it still has
// most of its time left, so a second email does not make the first one's
// code wrong.
func (s *Service) RequestSignIn(ctx context.Context, challengeID, name string) error {
	if !s.MailEnabled() {
		return ErrMailDisabled
	}
	s.init()
	users, err := s.usersNamed(ctx, strings.TrimSpace(name))
	if err != nil || len(users) == 0 {
		return err
	}
	now := time.Now()
	type entry struct {
		u *store.User
		t *ticket
	}
	var entries []entry
	s.mu.Lock()
	c := s.challenges[challengeID]
	if c == nil {
		s.mu.Unlock()
		return nil // expired meanwhile
	}
	for _, u := range users {
		t := s.tickets[u.ID]
		if t == nil || t.used || t.expires.Sub(now) < s.codeTTL()*2/3 || !strings.EqualFold(t.email, u.Email) {
			t = s.newTicket(now, u.Email)
			t.userID = u.ID
			s.tickets[u.ID] = t
		}
		c.tickets = append(c.tickets, t)
		entries = append(entries, entry{u, t})
	}
	s.mu.Unlock()

	// One email per address (accounts found by address all share it).
	var errs []error
	for len(entries) > 0 {
		to := entries[0].u.Email
		var mine, rest []entry
		for _, e := range entries {
			if strings.EqualFold(e.u.Email, to) {
				mine = append(mine, e)
			} else {
				rest = append(rest, e)
			}
		}
		entries = rest
		var allowed []entry
		for _, e := range mine {
			if s.mailLimit.Take(strconv.FormatInt(e.u.ID, 10), now) {
				allowed = append(allowed, e)
			}
		}
		if len(allowed) == 0 || !s.rcptLimit.Take(strings.ToLower(to), now) {
			continue
		}
		var subject, body string
		if len(allowed) == 1 {
			e := allowed[0]
			subject = "Your Cribbage sign-in code: " + showCode(e.t.code)
			body = fmt.Sprintf(`Hello %s,

Your code to sign in to the cribbage account "%s" is:

    %s

Enter it in the game within %s. Or, to sign in with the web browser on this device, open this link:

%s

If you did not ask to sign in, ignore this email. Nobody can sign in without the code or the link.
`, e.u.Nickname, e.u.Username, showCode(e.t.code), duration(s.codeTTL()), s.link("signin", e.t.link))
		} else {
			subject = "Your Cribbage sign-in codes"
			var b strings.Builder
			fmt.Fprintf(&b, "Hello,\n\nThis address belongs to %d cribbage accounts. Enter the code of the one you want within %s, or open its link to sign in with the web browser on this device.\n", len(allowed), duration(s.codeTTL()))
			for _, e := range allowed {
				fmt.Fprintf(&b, "\n%s (%s):\n    %s\n    %s\n", e.u.Username, e.u.Nickname, showCode(e.t.code), s.link("signin", e.t.link))
			}
			b.WriteString("\nIf you did not ask to sign in, ignore this email. Nobody can sign in without a code or a link.\n")
			body = b.String()
		}
		if err := s.Mailer.Send(ctx, to, subject, body); err != nil {
			errs = append(errs, fmt.Errorf("send sign-in code to user %d: %w", allowed[0].u.ID, err))
		}
	}
	return errors.Join(errs...)
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

// Register starts creating an account: it mails a code and link to email
// and returns the challenge. The account is created when one is used.
func (s *Service) Register(ctx context.Context, username, nickname, email string) (string, error) {
	email = strings.TrimSpace(email)
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = username
	}
	if err := validUsername(username); err != nil {
		return "", err
	}
	if err := validNickname(nickname); err != nil {
		return "", err
	}
	if err := s.CheckEmail(email); err != nil {
		return "", err
	}
	s.init()
	if _, err := s.Store.UserByUsername(ctx, username); err == nil {
		return "", store.ErrUsernameTaken
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if !s.mailAllowed(0, email) {
		return "", ErrMailLimit
	}
	now := time.Now()
	s.mu.Lock()
	id, c, err := s.newChallenge(now)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	t := s.newTicket(now, email)
	t.reg = &store.User{Username: username, Nickname: nickname, Email: email}
	c.tickets = append(c.tickets, t)
	s.mu.Unlock()

	body := fmt.Sprintf(`Hello %s,

Your code to finish creating the cribbage account "%s" is:

    %s

Enter it in the game within %s. Or, to finish with the web browser on this device, open this link:

%s

If you did not ask for an account, ignore this email and none will be made.
`, nickname, username, showCode(t.code), duration(s.codeTTL()), s.link("signin", t.link))
	if err := s.Mailer.Send(ctx, email, "Your Cribbage code: "+showCode(t.code), body); err != nil {
		return "", fmt.Errorf("send registration code: %w", err)
	}
	return id, nil
}

// EnterCode signs in (or finishes registering) with a challenge and the
// code mailed for it.
func (s *Service) EnterCode(ctx context.Context, challengeID, code string) (*Result, error) {
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
	s.use(match)
	delete(s.challenges, challengeID)
	s.mu.Unlock()
	return s.complete(ctx, match)
}

// UseSignInLink signs in (or finishes registering) with a link's token.
func (s *Service) UseSignInLink(ctx context.Context, token string) (*Result, error) {
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
}

// complete creates a registration's account, or checks that a sign-in's
// account still has the address the code went to, and starts a session.
func (s *Service) complete(ctx context.Context, t *ticket) (*Result, error) {
	if t.reg != nil {
		u := *t.reg
		u.Created = time.Now()
		if err := s.Store.CreateUser(ctx, &u); err != nil {
			return nil, err
		}
		return s.SignIn(ctx, &u)
	}
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
