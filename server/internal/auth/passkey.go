package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"cribbage/internal/store"
)

// Passkeys are WebAuthn credentials kept by the user's device or password
// manager. They are discoverable ("resident"), so signing in needs no
// username: the browser offers the user's passkeys for this site.

// maxPasskeys is how many passkeys an account may have.
const maxPasskeys = 20

// NewWebAuthn configures passkeys for the web client at publicURL, whose
// host is the relying party ID: passkeys work only on that site.
func NewWebAuthn(publicURL string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("passkeys need the web client's address (-public-url), not %q", publicURL)
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Cribbage",
		RPOrigins:     []string{u.Scheme + "://" + u.Host},
	})
}

// waUser is a user as the webauthn package sees them.
type waUser struct {
	u     *store.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return userHandle(w.u.ID) }
func (w *waUser) WebAuthnName() string                       { return w.u.Username }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.Nickname }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

// userHandle is the WebAuthn user ID of an account: its ID, which is never
// reused, so a deleted account's passkeys match nobody.
func userHandle(id int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(id)) }

func (s *Service) loadWAUser(ctx context.Context, u *store.User) (*waUser, []store.Passkey, error) {
	keys, err := s.Store.Passkeys(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	w := &waUser{u: u}
	for _, k := range keys {
		var c webauthn.Credential
		if err := json.Unmarshal(k.Data, &c); err != nil {
			return nil, nil, fmt.Errorf("passkey of user %d: %w", u.ID, err)
		}
		w.creds = append(w.creds, c)
	}
	return w, keys, nil
}

// PasskeyID is how a passkey is named on the wire.
func PasskeyID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// BeginPasskeySignIn returns the options for navigator.credentials.get()
// and the ceremony's state, for FinishPasskeySignIn.
func (s *Service) BeginPasskeySignIn() (string, *webauthn.SessionData, error) {
	if s.WebAuthn == nil {
		return "", nil, ErrPasskeysDisabled
	}
	opts, sd, err := s.WebAuthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return "", nil, err
	}
	b, err := json.Marshal(opts)
	return string(b), sd, err
}

// FinishPasskeySignIn checks the browser's answer and starts a session.
func (s *Service) FinishPasskeySignIn(ctx context.Context, sd *webauthn.SessionData, credJSON string) (*Result, error) {
	if s.WebAuthn == nil {
		return nil, ErrPasskeysDisabled
	}
	if sd == nil {
		return nil, ErrBadPasskey
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(credJSON))
	if err != nil {
		return nil, ErrBadPasskey
	}
	var found *waUser
	var keys []store.Passkey
	var lookupErr error
	handler := func(_, handle []byte) (webauthn.User, error) {
		if len(handle) != 8 {
			return nil, ErrBadPasskey
		}
		u, err := s.Store.UserByID(ctx, int64(binary.BigEndian.Uint64(handle)))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				lookupErr = err
			}
			return nil, ErrBadPasskey
		}
		found, keys, err = s.loadWAUser(ctx, u)
		if err != nil {
			lookupErr = err
			return nil, err
		}
		return found, nil
	}
	_, cred, err := s.WebAuthn.ValidatePasskeyLogin(handler, *sd, parsed)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if err != nil || found == nil {
		return nil, ErrBadPasskey
	}
	for _, k := range keys {
		if bytes.Equal(k.ID, cred.ID) {
			if k.Data, err = json.Marshal(cred); err != nil {
				return nil, err
			}
			k.LastUsed = time.Now()
			if err := s.Store.UpdatePasskey(ctx, &k); err != nil {
				return nil, err
			}
			break
		}
	}
	return s.SignIn(ctx, found.u)
}

// BeginAddPasskey returns the options for navigator.credentials.create()
// and the ceremony's state, for FinishAddPasskey.
func (s *Service) BeginAddPasskey(ctx context.Context, u *store.User) (string, *webauthn.SessionData, error) {
	if s.WebAuthn == nil {
		return "", nil, ErrPasskeysDisabled
	}
	w, _, err := s.loadWAUser(ctx, u)
	if err != nil {
		return "", nil, err
	}
	if len(w.creds) >= maxPasskeys {
		return "", nil, fmt.Errorf("%w: an account can have at most %d passkeys", ErrInvalidInput, maxPasskeys)
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range w.creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, sd, err := s.WebAuthn.BeginRegistration(w,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude))
	if err != nil {
		return "", nil, err
	}
	b, err := json.Marshal(opts)
	return string(b), sd, err
}

// FinishAddPasskey checks the browser's new credential and saves it as
// added by session (see store.Passkey), then tells the account's address.
func (s *Service) FinishAddPasskey(ctx context.Context, u *store.User, session string, sd *webauthn.SessionData, credJSON, name string) error {
	if s.WebAuthn == nil {
		return ErrPasskeysDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if n := utf8.RuneCountInString(name); n > 40 || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return fmt.Errorf("%w: a passkey's name must be up to 40 printable characters", ErrInvalidInput)
	}
	if sd == nil {
		return fmt.Errorf("%w: start adding the passkey again", ErrInvalidInput)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(credJSON))
	if err != nil {
		return fmt.Errorf("%w: the passkey could not be read", ErrInvalidInput)
	}
	w, _, err := s.loadWAUser(ctx, u)
	if err != nil {
		return err
	}
	cred, err := s.WebAuthn.CreateCredential(w, *sd, parsed)
	if err != nil {
		return fmt.Errorf("%w: the passkey was not accepted", ErrInvalidInput)
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	err = s.Store.AddPasskey(ctx, &store.Passkey{ID: cred.ID, UserID: u.ID, Name: name, Data: data, Created: time.Now(), Session: session})
	if errors.Is(err, store.ErrNotFound) {
		return ErrBadToken // signed out (or deleted) meanwhile
	}
	if err != nil {
		return err
	}
	s.notifyPasskey(ctx, u.ID, name)
	return nil
}

// notifyPasskey tells an account's address that a passkey was added, so an
// owner whose session was stolen finds out.
func (s *Service) notifyPasskey(ctx context.Context, userID int64, name string) {
	u, err := s.Store.UserByID(ctx, userID)
	if err != nil || u.Email == "" || s.Mailer == nil || !s.mailAllowed(u.ID, u.Email) {
		return
	}
	body := fmt.Sprintf(`Hello %s,

A passkey named "%s" was added to your cribbage account "%s". It can sign in to the account.

If you did not add it, open Account in the game, remove the passkey, then choose "Sign out other devices", which also removes passkeys added from those devices.
`, u.Nickname, name, u.Username)
	// Best effort: the passkey is added either way.
	_ = s.send(ctx, "passkey added", u.Email, "A passkey was added to your Cribbage account", body)
}

// RemovePasskey removes one of a user's passkeys.
func (s *Service) RemovePasskey(ctx context.Context, userID int64, id string) error {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return ErrBadToken
	}
	err = s.Store.DeletePasskey(ctx, userID, raw)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: no such passkey", ErrInvalidInput)
	}
	return err
}
