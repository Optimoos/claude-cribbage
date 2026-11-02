package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/mail"
	"cribbage/internal/ratelimit"
	"cribbage/internal/store"
)

var (
	ErrMailDisabled = errors.New("this server cannot send email; ask whoever runs it to reset your password")
	ErrBadToken     = errors.New("this link has expired or has already been used")
	ErrMailLimit    = errors.New("too many emails for this account; try again later")
)

// Account is what a user can see and change about their account.
type Account struct {
	Email        string // verified
	PendingEmail string // set, awaiting its link
	MustChange   bool
	EmailEnabled bool
}

// MailEnabled reports whether the service can send email.
func (s *Service) MailEnabled() bool { return s.Mailer != nil }

func (s *Service) cost() int {
	if s.BcryptCost == 0 {
		return bcrypt.DefaultCost
	}
	return s.BcryptCost
}

func validPassword(p string) error {
	if n := len(p); n < 8 || n > 72 { // bcrypt uses at most 72 bytes
		return fmt.Errorf("%w: password must be 8-72 bytes", ErrInvalidInput)
	}
	return nil
}

// mailAllowed spends one of a user's emails: 3, then one every 20 minutes,
// so nobody can use the server to flood someone's inbox.
func (s *Service) mailAllowed(userID int64) bool {
	s.mailOnce.Do(func() { s.mailLimit = ratelimit.New(3, 20*time.Minute) })
	return s.mailLimit.Take(strconv.FormatInt(userID, 10), time.Now())
}

// Account describes a user's account settings.
func (s *Service) Account(ctx context.Context, u *store.User) (Account, error) {
	pending, err := s.Store.PendingEmail(ctx, u.ID)
	if err != nil {
		return Account{}, err
	}
	return Account{Email: u.Email, PendingEmail: pending, MustChange: u.MustChange, EmailEnabled: s.MailEnabled()}, nil
}

// checkPassword returns the user, freshly loaded, if password is theirs.
func (s *Service) checkPassword(ctx context.Context, userID int64, password string) (*store.User, error) {
	u, err := s.Store.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword(u.PasswordHash, []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// ChangePassword replaces a user's password, given the current one. Every
// session but keep ends. It returns the updated user.
func (s *Service) ChangePassword(ctx context.Context, userID int64, oldPassword, newPassword, keep string) (*store.User, error) {
	if err := validPassword(newPassword); err != nil {
		return nil, err
	}
	u, err := s.checkPassword(ctx, userID, oldPassword)
	if err != nil {
		return nil, err
	}
	if newPassword == oldPassword {
		return nil, fmt.Errorf("%w: choose a different password", ErrInvalidInput)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.cost())
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetPassword(ctx, userID, hash, false, keep); err != nil {
		return nil, err
	}
	u.PasswordHash, u.MustChange = hash, false
	return u, nil
}

// SetEmail sets the address for reset links, given the user's password. A
// new address is mailed a link and used once the link is opened; an empty
// one removes the address.
func (s *Service) SetEmail(ctx context.Context, userID int64, password, email string) error {
	email = strings.TrimSpace(email)
	if email != "" && !s.MailEnabled() {
		return ErrMailDisabled
	}
	if email != "" && !mail.ValidAddress(email) {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidInput)
	}
	u, err := s.checkPassword(ctx, userID, password)
	if err != nil {
		return err
	}
	if email == "" || strings.EqualFold(email, u.Email) {
		// Removing the address, or keeping it: either way, drop a pending
		// change.
		return s.Store.SetEmail(ctx, userID, email)
	}
	if !s.mailAllowed(userID) {
		return ErrMailLimit
	}
	token := RandomID(tokenBytes)
	ttl := s.VerifyTTL
	if ttl == 0 {
		ttl = 48 * time.Hour
	}
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: userID, Kind: store.TokenVerify, Email: email, Expires: time.Now().Add(ttl),
	}); err != nil {
		return err
	}
	body := fmt.Sprintf(`Hello %s,

To use this address for password resets on your cribbage account "%s", open this link within %s:

%s

If you did not ask for this, ignore this email and nothing will change.
`, u.Nickname, u.Username, hours(ttl), s.link("verify", token))
	if err := s.Mailer.Send(ctx, email, "Confirm your email for Cribbage", body); err != nil {
		return fmt.Errorf("send verification: %w", err)
	}
	return nil
}

// VerifyEmail confirms an address with the token from its link and returns
// the user it belongs to.
func (s *Service) VerifyEmail(ctx context.Context, token string) (int64, error) {
	if !validToken(token) {
		return 0, ErrBadToken
	}
	t, err := s.Store.TakeAccountToken(ctx, token, store.TokenVerify)
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrBadToken
	}
	if err != nil {
		return 0, err
	}
	return t.UserID, s.Store.SetEmail(ctx, t.UserID, t.Email)
}

// RequestReset mails a reset link to each account with this username or
// verified address. It reports no error for a name with no account, or
// one without an address.
func (s *Service) RequestReset(ctx context.Context, name string) error {
	if !s.MailEnabled() {
		return ErrMailDisabled
	}
	name = strings.TrimSpace(name)
	var users []*store.User
	if strings.Contains(name, "@") {
		if !mail.ValidAddress(name) {
			return nil
		}
		var err error
		if users, err = s.Store.UsersByEmail(ctx, name); err != nil {
			return err
		}
	} else if ValidUsername(name) {
		u, err := s.Store.UserByUsername(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		users = append(users, u)
	}
	var errs []error
	for _, u := range users {
		if u.Email == "" || !s.mailAllowed(u.ID) {
			continue
		}
		errs = append(errs, s.mailReset(ctx, u))
	}
	return errors.Join(errs...)
}

func (s *Service) mailReset(ctx context.Context, u *store.User) error {
	token := RandomID(tokenBytes)
	ttl := s.ResetTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: u.ID, Kind: store.TokenReset, Expires: time.Now().Add(ttl),
	}); err != nil {
		return err
	}
	body := fmt.Sprintf(`Hello %s,

Someone asked to reset the password of your cribbage account "%s". To choose a new password, open this link within %s:

%s

If it was not you, ignore this email: your password has not changed.
`, u.Nickname, u.Username, hours(ttl), s.link("reset", token))
	if err := s.Mailer.Send(ctx, u.Email, "Reset your Cribbage password", body); err != nil {
		return fmt.Errorf("send reset to user %d: %w", u.ID, err)
	}
	return nil
}

// ResetPassword sets a new password with a reset link's token, ends every
// session of the account, and starts a new one.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) (*Result, error) {
	// Check the password first, so a mistyped one does not use up the link.
	if err := validPassword(newPassword); err != nil {
		return nil, err
	}
	if !validToken(token) {
		return nil, ErrBadToken
	}
	t, err := s.Store.TakeAccountToken(ctx, token, store.TokenReset)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.cost())
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetPassword(ctx, t.UserID, hash, false, ""); err != nil {
		return nil, err
	}
	u, err := s.Store.UserByID(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	return s.newSession(ctx, u)
}

// AdminResetPassword gives a user a random temporary password, which they
// must change when they next log in, and ends all their sessions.
func (s *Service) AdminResetPassword(ctx context.Context, username string) (*store.User, string, error) {
	u, err := s.Store.UserByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	temp := TempPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(temp), s.cost())
	if err != nil {
		return nil, "", err
	}
	if err := s.Store.SetPassword(ctx, u.ID, hash, true, ""); err != nil {
		return nil, "", err
	}
	return u, temp, nil
}

// TempPassword is 12 random characters in groups of four, such as
// "k7mq-x2r9-pd4h", without letters and digits that are easily confused
// (about 60 bits).
func TempPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(c)%len(alphabet)]) // 256 % 31 bias is negligible here
	}
	return sb.String()
}

// link is a web client URL that opens a reset or verification form. The
// token is in the fragment, which browsers do not send to the server.
func (s *Service) link(kind, token string) string {
	base := s.BaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "#" + kind + "=" + token
}

func hours(d time.Duration) string {
	if h := int(d.Hours()); h == 1 {
		return "an hour"
	} else if h > 1 {
		return fmt.Sprintf("%d hours", h)
	}
	return d.String()
}
