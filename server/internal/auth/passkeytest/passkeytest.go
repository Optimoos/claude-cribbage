// Package passkeytest is a software passkey authenticator for tests: it
// answers the WebAuthn options a server sends as a browser would, with
// "none" attestation and a P-256 key.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// Authenticator holds passkeys for one site, as a phone or password
// manager would.
type Authenticator struct {
	Origin string // e.g. "https://cribbage.example.com"
	keys   []*key
}

type key struct {
	id     []byte
	user   []byte // the user handle
	rpID   string
	priv   *ecdsa.PrivateKey
	counts uint32
}

var b64 = base64.RawURLEncoding

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

// Create answers PasskeyOptions from PasskeyAddBegin with a new passkey's
// credential JSON.
func (a *Authenticator) Create(optionsJSON string) (string, error) {
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
			Exclude []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return "", err
	}
	p := opts.PublicKey
	user, err := b64.DecodeString(p.User.ID)
	if err != nil {
		return "", fmt.Errorf("user id: %w", err)
	}
	for _, ex := range p.Exclude {
		for _, k := range a.keys {
			if b64.EncodeToString(k.id) == ex.ID {
				return "", fmt.Errorf("already registered")
			}
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	k := &key{id: randBytes(16), user: user, rpID: p.RP.ID, priv: priv}
	a.keys = append(a.keys, k)

	pub, err := priv.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		return "", err
	}
	cose, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pub[1:33], -3: pub[33:]})
	if err != nil {
		return "", err
	}
	att := make([]byte, 16) // AAGUID
	att = binary.BigEndian.AppendUint16(att, uint16(len(k.id)))
	att = append(att, k.id...)
	att = append(att, cose...)
	authData := authData(k.rpID, flagUP|flagUV|flagBE|flagBS|flagAT, 0, att)
	attObj, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		return "", err
	}
	cd := a.clientData("webauthn.create", p.Challenge)
	return marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cd), "attestationObject": b64.EncodeToString(attObj),
			"transports": []string{"internal"},
		},
	})
}

// Get answers PasskeyOptions from PasskeySignInBegin with an assertion by
// the passkey at index i (the one the user picked).
func (a *Authenticator) Get(optionsJSON string, i int) (string, error) {
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return "", err
	}
	if i >= len(a.keys) {
		return "", fmt.Errorf("no passkey %d", i)
	}
	k := a.keys[i]
	if opts.PublicKey.RPID != "" && opts.PublicKey.RPID != k.rpID {
		return "", fmt.Errorf("passkey is for %s, not %s", k.rpID, opts.PublicKey.RPID)
	}
	k.counts++
	ad := authData(k.rpID, flagUP|flagUV|flagBE|flagBS, k.counts, nil)
	cd := a.clientData("webauthn.get", opts.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	return marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(ad),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(k.user),
		},
	})
}

// SetUserHandle makes passkey i claim another user handle, as a
// tampered authenticator might.
func (a *Authenticator) SetUserHandle(i int, handle []byte) { a.keys[i].user = handle }

func (a *Authenticator) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
	return b
}

func authData(rpID string, flags byte, count uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	d := append(h[:], flags)
	d = binary.BigEndian.AppendUint32(d, count)
	return append(d, attested...)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
