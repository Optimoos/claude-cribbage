package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cribbage/internal/mail"
	"cribbage/internal/store"
)

// Account is what a user can see and change about their account.
type Account struct {
	Email        string // verified
	PendingEmail string // asked for, not yet confirmed
	// PendingNeedsCurrent: the change waits for the link sent to the
	// current address; after that, for the one sent to the new address.
	PendingNeedsCurrent bool
	PasskeysEnabled     bool
	Passkeys            []store.Passkey
}

// Account describes a user's account settings.
func (s *Service) Account(ctx context.Context, u *store.User) (Account, error) {
	a := Account{Email: u.Email, PasskeysEnabled: s.WebAuthn != nil}
	var err error
	if a.PendingEmail, err = s.Store.PendingEmail(ctx, u.ID, store.TokenChange); err != nil {
		return Account{}, err
	}
	if a.PendingEmail != "" {
		a.PendingNeedsCurrent = true
	} else if a.PendingEmail, err = s.Store.PendingEmail(ctx, u.ID, store.TokenVerify); err != nil {
		return Account{}, err
	}
	if a.PasskeysEnabled {
		if a.Passkeys, err = s.Store.Passkeys(ctx, u.ID); err != nil {
			return Account{}, err
		}
	}
	return a, nil
}

func (s *Service) verifyTTL() time.Duration {
	if s.VerifyTTL == 0 {
		return 48 * time.Hour
	}
	return s.VerifyTTL
}

func (s *Service) deleteTTL() time.Duration {
	if s.DeleteTTL == 0 {
		return time.Hour
	}
	return s.DeleteTTL
}

// SetEmail starts changing u's address. The address is where sign-in codes
// go, so it is as good as a password: a change must be confirmed from the
// current address (so someone using a device left signed in cannot take
// the account) and then from the new one (so it is not mistyped). An
// account with no address yet (a bot) skips the first step.
func (s *Service) SetEmail(ctx context.Context, u *store.User, email string) error {
	if err := s.CheckEmail(email); err != nil {
		return err
	}
	email = strings.TrimSpace(email)
	if strings.EqualFold(email, u.Email) {
		// Keeping the address: drop a pending change.
		return s.Store.SetEmail(ctx, u.ID, u.Email)
	}
	if u.Email == "" {
		return s.mailVerify(ctx, u, email)
	}
	if !s.mailAllowed(u.ID, u.Email) {
		return ErrMailLimit
	}
	token := RandomID(tokenBytes)
	ttl := s.verifyTTL()
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: u.ID, Kind: store.TokenChange, Email: email, Expires: time.Now().Add(ttl),
	}); err != nil {
		return err
	}
	body := fmt.Sprintf(`Hello %s,

Someone signed in to your cribbage account "%s" asked to send its sign-in codes to a new address:

    %s

If that was you, open this link within %s to confirm, then open the link we send to the new address:

%s

If it was not you, ignore this email and nothing will change. Someone may be using a device you left signed in: in the game, open Account and choose "Sign out other devices".
`, u.Nickname, u.Username, email, duration(ttl), s.link("change", token))
	if err := s.Mailer.Send(ctx, u.Email, "Confirm changing your Cribbage email address", body); err != nil {
		return fmt.Errorf("send address change: %w", err)
	}
	return nil
}

// mailVerify mails a link that confirms a new address.
func (s *Service) mailVerify(ctx context.Context, u *store.User, email string) error {
	if !s.mailAllowed(u.ID, email) {
		return ErrMailLimit
	}
	token := RandomID(tokenBytes)
	ttl := s.verifyTTL()
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: u.ID, Kind: store.TokenVerify, Email: email, Expires: time.Now().Add(ttl),
	}); err != nil {
		return err
	}
	// Nothing the account's owner wrote goes to an address not yet shown
	// to be theirs, except the username, which is short and plain.
	body := fmt.Sprintf(`Hello,

To use this address for signing in to the cribbage account "%s", open this link within %s:

%s

If you did not ask for this, ignore this email and nothing will change.
`, u.Username, duration(ttl), s.link("verify", token))
	if err := s.Mailer.Send(ctx, email, "Confirm your email for Cribbage", body); err != nil {
		return fmt.Errorf("send verification: %w", err)
	}
	return nil
}

// VerifyEmail takes the token from either link of an address change and
// returns the user it belongs to. From the current address's link, it
// mails the new address its link (changed is false); from the new
// address's, it makes the change.
func (s *Service) VerifyEmail(ctx context.Context, token string) (userID int64, changed bool, err error) {
	if !validToken(token) {
		return 0, false, ErrBadToken
	}
	if t, err := s.Store.TakeAccountToken(ctx, token, store.TokenChange); err == nil {
		u, err := s.Store.UserByID(ctx, t.UserID)
		if err != nil {
			return 0, false, err
		}
		return u.ID, false, s.mailVerify(ctx, u, t.Email)
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, false, err
	}
	t, err := s.Store.TakeAccountToken(ctx, token, store.TokenVerify)
	if errors.Is(err, store.ErrNotFound) {
		return 0, false, ErrBadToken
	}
	if err != nil {
		return 0, false, err
	}
	u, err := s.Store.UserByID(ctx, t.UserID)
	if err != nil {
		return 0, false, err
	}
	if err := s.Store.SetEmail(ctx, t.UserID, t.Email); err != nil {
		return 0, false, err
	}
	if u.Email != "" && !strings.EqualFold(u.Email, t.Email) {
		s.notifyChanged(ctx, u, t.Email)
	}
	return t.UserID, true, nil
}

// notifyChanged tells an account's old address that codes now go to
// another, so an owner who did not make the change finds out.
func (s *Service) notifyChanged(ctx context.Context, u *store.User, to string) {
	if s.Mailer == nil || !s.mailAllowed(u.ID, u.Email) {
		return
	}
	body := fmt.Sprintf(`Hello %s,

Sign-in codes for your cribbage account "%s" now go to another address (%s), so this address will no longer get them.

If you did not do this, ask whoever runs the server to set your address back.
`, u.Nickname, u.Username, maskAddress(to))
	// Best effort: the change is made either way.
	_ = s.Mailer.Send(ctx, u.Email, "Your Cribbage email address was changed", body)
}

// maskAddress shows enough of an address to recognize it: "j***@example.com".
func maskAddress(a string) string {
	at := strings.LastIndexByte(a, '@')
	if at < 1 {
		return "***"
	}
	return a[:1] + "***" + a[at:]
}

// RequestDelete starts deleting u's account: it mails a link that confirms
// the deletion and reports mailed. A bot account has no address; its
// username must match instead, and the caller deletes it at once.
func (s *Service) RequestDelete(ctx context.Context, u *store.User, username string) (mailed bool, err error) {
	if u.Email == "" {
		if !strings.EqualFold(strings.TrimSpace(username), u.Username) {
			return false, fmt.Errorf("%w: type the username to confirm", ErrInvalidInput)
		}
		return false, nil
	}
	if !s.MailEnabled() {
		return false, ErrMailDisabled
	}
	if !s.mailAllowed(u.ID, u.Email) {
		return false, ErrMailLimit
	}
	token := RandomID(tokenBytes)
	ttl := s.deleteTTL()
	if err := s.Store.CreateAccountToken(ctx, &store.AccountToken{
		Token: token, UserID: u.ID, Kind: store.TokenDelete, Expires: time.Now().Add(ttl),
	}); err != nil {
		return false, err
	}
	body := fmt.Sprintf(`Hello %s,

Someone asked to delete your cribbage account "%s". To delete it, open this link within %s and confirm:

%s

Deleting ends your unfinished games (you resign them), removes your account, email address, passkeys and invites, and frees your username. Games you finished stay in your opponents' history, shown as "%s". This cannot be undone.

If it was not you, ignore this email and nothing will change. Someone may be using a device you left signed in: in the game, open Account and choose "Sign out other devices".
`, u.Nickname, u.Username, duration(ttl), s.link("delete", token), store.DeletedNickname)
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

// AdminSetEmail recovers an account: it sets its address, ends all its
// sessions and removes its passkeys, so only whoever reads the new
// address can sign in. The old address, if any, is told. It returns the
// user as it was.
func (s *Service) AdminSetEmail(ctx context.Context, username, email string) (*store.User, error) {
	email = strings.TrimSpace(email)
	if !mail.ValidAddress(email) {
		return nil, fmt.Errorf("%w: %q is not an email address", ErrInvalidInput, email)
	}
	u, err := s.Store.UserByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetEmail(ctx, u.ID, email); err != nil {
		return nil, err
	}
	if err := s.Store.DeleteSessions(ctx, u.ID, ""); err != nil {
		return nil, err
	}
	if err := s.Store.DeletePasskeys(ctx, u.ID); err != nil {
		return nil, err
	}
	if u.Email != "" && !strings.EqualFold(u.Email, email) {
		s.notifyChanged(ctx, u, email)
	}
	return u, nil
}
