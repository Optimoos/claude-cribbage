package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"cribbage/internal/mail"
)

// Email addresses are the only personal data the database holds, so the
// SQLite store keeps them encrypted (AES-256-GCM, bound to the user's ID)
// under a key kept outside the database. A copy of the database or of a
// backup without the key reveals no addresses. Looking an address up (for
// "forgot your password" by email) uses a keyed hash of the lower-cased
// address, which reveals only which accounts share one.

// ErrNoEmailKey is returned when storing an address without an email key.
var ErrNoEmailKey = errors.New("store: no email key configured")

// ErrWrongEmailKey means the key does not decrypt this database's
// addresses: it is not the key they were stored with.
var ErrWrongEmailKey = errors.New("store: the email key does not match the one this database was written with")

// MinEmailKey is the shortest key accepted, in bytes.
const MinEmailKey = 32

type emailCrypt struct {
	aead    cipher.AEAD
	mac     []byte
	stopMac []byte // stopped mailboxes' keyed hashes
	stopTag []byte // tags in stop links, so only the server can make them
}

func newEmailCrypt(key []byte) (*emailCrypt, error) {
	if len(key) < MinEmailKey {
		return nil, errors.New("store: the email key must be at least 32 bytes")
	}
	enc, err := hkdf.Key(sha256.New, key, nil, "cribbage email encryption", 32)
	if err != nil {
		return nil, err
	}
	mac, err := hkdf.Key(sha256.New, key, nil, "cribbage email index", 32)
	if err != nil {
		return nil, err
	}
	stopMac, err := hkdf.Key(sha256.New, key, nil, "cribbage email stop index", 32)
	if err != nil {
		return nil, err
	}
	stopTag, err := hkdf.Key(sha256.New, key, nil, "cribbage email stop tag", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &emailCrypt{aead: aead, mac: mac, stopMac: stopMac, stopTag: stopTag}, nil
}

// context binds a sealed value to what it belongs to, so it cannot be
// copied to another user's row.
func emailContext(userID int64) []byte {
	return []byte("user " + strconv.FormatInt(userID, 10))
}

func (e *emailCrypt) seal(context []byte, plain string) string {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawStdEncoding.EncodeToString(e.aead.Seal(nonce, nonce, []byte(plain), context))
}

func (e *emailCrypt) open(context []byte, sealed string) (string, error) {
	b, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(b) < e.aead.NonceSize() {
		return "", ErrWrongEmailKey
	}
	n := e.aead.NonceSize()
	plain, err := e.aead.Open(nil, b[:n], b[n:], context)
	if err != nil {
		return "", ErrWrongEmailKey
	}
	return string(plain), nil
}

// index is the lookup key of an address.
func (e *emailCrypt) index(email string) string {
	h := hmac.New(sha256.New, e.mac)
	h.Write([]byte(strings.ToLower(email)))
	return hex.EncodeToString(h.Sum(nil))
}

// Stop links. The token in the link in every email is the keyed hash
// under which its mailbox is kept if someone opens it (stopIndex), and a
// tag over that hash, so the server can check a token without storing it
// and nobody else can make one. It never expires: an unsubscribe link
// keeps working.

const (
	stopIndexBytes = 32
	stopTagBytes   = 16
)

// stopIndex is the stored key of an address's mailbox (mail.Mailbox), so
// stopping one address stops its +tag variants too.
func (e *emailCrypt) stopIndex(email string) string {
	h := hmac.New(sha256.New, e.stopMac)
	h.Write([]byte(mail.Mailbox(email)))
	return hex.EncodeToString(h.Sum(nil))
}

func (e *emailCrypt) tag(index string) string {
	h := hmac.New(sha256.New, e.stopTag)
	h.Write([]byte(index))
	return hex.EncodeToString(h.Sum(nil)[:stopTagBytes])
}

// stopToken is the token for the stop link in email to an address.
func (e *emailCrypt) stopToken(email string) string {
	idx := e.stopIndex(email)
	return idx + e.tag(idx)
}

// checkStopToken returns the stop index of a token the server made.
func (e *emailCrypt) checkStopToken(token string) (string, bool) {
	if len(token) != 2*(stopIndexBytes+stopTagBytes) {
		return "", false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", false
	}
	idx, tag := token[:2*stopIndexBytes], token[2*stopIndexBytes:]
	return idx, hmac.Equal([]byte(tag), []byte(e.tag(idx)))
}

// keyCheck is sealed and stored once, so a different key is noticed at
// startup rather than as unreadable addresses.
const keyCheck = "cribbage email key check"

// ParseEmailKey decodes a key written as hex (e.g. by openssl rand -hex 32).
func ParseEmailKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("the email key must be hex, e.g. from openssl rand -hex 32")
	}
	if len(key) < MinEmailKey {
		return nil, errors.New("the email key must be at least 32 bytes (64 hex digits)")
	}
	return key, nil
}
