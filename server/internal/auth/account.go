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

// mailAllowed spends one of a user's emails (3, then one every 20 minutes)
// and one of the address's (3, then one an hour), so nobody can use the
// server to flood an inbox, from one account or from many.
func (s *Service) mailAllowed(userID int64, to string) bool {
	s.mailOnce.Do(func() {
		s.mailLimit = ratelimit.New(3, 20*time.Minute)
		s.rcptLimit = ratelimit.New(3, time.Hour)
	})
	now := time.Now()
	return s.mailLimit.Take(strconv.FormatInt(userID, 10), now) &&
		s.rcptLimit.Take(strings.ToLower(to), now)
}

// Account describes a user's account settings.
func (s *Service) Account(ctx context.Context, u *store.User) (Account, error) {
	pending, err := s.Store.PendingEmail(ctx, u.ID)
	if err != nil {
		return Account{}, err
	}
	return Account{Email: u.Email, PendingEmail: pending, MustChange: u.MustChange, EmailEnabled: s.MailEnabled()}, nil
}

// CheckPassword returns the user, freshly loaded, if password is theirs,
// else ErrInvalidCredentials. It costs a bcrypt hash.
func (s *Service) CheckPassword(ctx context.Context, userID int64, password string) (*store.User, error) {
	return s.checkPassword(ctx, userID, password)
}

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

// CheckEmail reports an address SetEmail would refuse, before the
// password is checked.
func (s *Service) CheckEmail(email string) error {
	email = strings.TrimSpace(email)
	if email != "" && !s.MailEnabled() {
		return ErrMailDisabled
	}
	if email != "" && !mail.ValidAddress(email) {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidInput)
	}
	return nil
}

// SetEmail sets the address for reset links of u, whose password the
// caller has checked (CheckPassword). A new address is mailed a link and
// used once the link is opened; an empty one removes the address.
func (s *Service) SetEmail(ctx context.Context, u *store.User, email string) error {
	if err := s.CheckEmail(email); err != nil {
		return err
	}
	email = strings.TrimSpace(email)
	userID := u.ID
	if email == "" || strings.EqualFold(email, u.Email) {
		// Removing the address, or keeping it: either way, drop a pending
		// change.
		if err := s.Store.SetEmail(ctx, userID, email); err != nil {
			return err
		}
		if email == "" && u.Email != "" {
			s.notifyChanged(ctx, u, "removed")
		}
		return nil
	}
	if !s.mailAllowed(userID, email) {
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
	// Nothing the account's owner wrote goes to an address not yet shown
	// to be theirs, except the username, which is short and plain.
	body := fmt.Sprintf(`Hello,

To use this address for password resets on the cribbage account "%s", open this link within %s:

%s

If you did not ask for this, ignore this email and nothing will change.
`, u.Username, hours(ttl), s.link("verify", token))
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
	u, err := s.Store.UserByID(ctx, t.UserID)
	if err != nil {
		return 0, err
	}
	if err := s.Store.SetEmail(ctx, t.UserID, t.Email); err != nil {
		return 0, err
	}
	if u.Email != "" && !strings.EqualFold(u.Email, t.Email) {
		s.notifyChanged(ctx, u, "changed to another address")
	}
	return t.UserID, nil
}

// notifyChanged tells an account's old address that it no longer gets the
// account's reset links, so an owner whose password was taken finds out.
func (s *Service) notifyChanged(ctx context.Context, u *store.User, what string) {
	if s.Mailer == nil || !s.mailAllowed(u.ID, u.Email) {
		return
	}
	body := fmt.Sprintf(`Hello %s,

The email address for password resets on your cribbage account "%s" was %s, so this address will no longer get its reset links.

If you did not do this, someone else may know your password: log in, change your password, and set your address again. If you cannot log in, ask whoever runs the server to reset your password.
`, u.Nickname, u.Username, what)
	// Best effort: the change is made either way.
	_ = s.Mailer.Send(ctx, u.Email, "Your Cribbage email address was changed", body)
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
		if u.Email == "" || !s.mailAllowed(u.ID, u.Email) {
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
// must change when they next log in, and ends all their sessions. It also
// removes their email address, which whoever took the account may have
// set; the returned user still has it, for the admin to see.
func (s *Service) AdminResetPassword(ctx context.Context, username string) (*store.User, string, error) {
	u, err := s.Store.UserByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	if err := s.Store.SetEmail(ctx, u.ID, ""); err != nil {
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

// RequestDelete starts deleting u's account; the caller has checked the
// password (CheckPassword). If the account has an email address and the
// server can send mail, it mails a link that confirms the deletion and
// reports mailed. Otherwise username must match the account's, and the
// caller deletes the account at once: an emailed link is the only second
// factor available, and an account without an address has nothing else
// to prove.
func (s *Service) RequestDelete(ctx context.Context, u *store.User, username string) (mailed bool, err error) {
	userID := u.ID
	if u.Email == "" || !s.MailEnabled() {
		if !strings.EqualFold(strings.TrimSpace(username), u.Username) {
			return false, fmt.Errorf("%w: type your username to confirm", ErrInvalidInput)
		}
		return false, nil
	}
	if !s.mailAllowed(userID, u.Email) {
		return false, ErrMailLimit
	}
	token := RandomID(tokenBytes)
	ttl := s.ResetTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: userID, Kind: store.TokenDelete, Expires: time.Now().Add(ttl),
	}); err != nil {
		return false, err
	}
	body := fmt.Sprintf(`Hello %s,

Someone asked to delete your cribbage account "%s". To delete it, open this link within %s and confirm:

%s

Deleting ends your unfinished games (you resign them), removes your account, email address and invites, and frees your username. Games you finished stay in your opponents' history, shown as "%s". This cannot be undone.

If it was not you, ignore this email: nothing will change. But someone may know your password, so change it.
`, u.Nickname, u.Username, hours(ttl), s.link("delete", token), store.DeletedNickname)
	if err := s.Mailer.Send(ctx, u.Email, "Confirm deleting your Cribbage account", body); err != nil {
		return false, fmt.Errorf("send delete link: %w", err)
	}
	return true, nil
}

// TakeDeleteToken checks a token from a deletion link, using it up, and
// returns the user it belongs to.
func (s *Service) TakeDeleteToken(ctx context.Context, token string) (int64, error) {
	if !validToken(token) {
		return 0, ErrBadToken
	}
	t, err := s.Store.TakeAccountToken(ctx, token, store.TokenDelete)
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrBadToken
	}
	if err != nil {
		return 0, err
	}
	return t.UserID, nil
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
