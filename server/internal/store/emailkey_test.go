package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testEmailKey = bytes.Repeat([]byte{7}, 32)

// Addresses are stored encrypted, found by a keyed hash, and readable only
// with the key the database was first opened with.
func TestEmailsEncrypted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "e.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	u := &User{Username: "alice", Nickname: "A", PasswordHash: []byte("h"), Created: time.Now()}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail(ctx, u.ID, "Alice@Example.com"); !errors.Is(err, ErrNoEmailKey) {
		t.Fatalf("address stored without a key: %v", err)
	}
	if err := s.SetEmailKey(ctx, testEmailKey); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail(ctx, u.ID, "Alice@Example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccountToken(ctx, &AccountToken{Token: "v", UserID: u.ID, Kind: TokenVerify,
		Email: "pending@example.com", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.Email != "Alice@Example.com" {
		t.Fatalf("read back %q", got.Email)
	}
	if us, err := s.UsersByEmail(ctx, "alice@EXAMPLE.com"); err != nil || len(us) != 1 || us[0].Email != "Alice@Example.com" {
		t.Fatalf("lookup: %v, %v", us, err)
	}
	if p, _ := s.PendingEmail(ctx, u.ID); p != "pending@example.com" {
		t.Fatalf("pending %q", p)
	}
	// A row's address cannot be moved to another user.
	b := &User{Username: "bob", Nickname: "B", PasswordHash: []byte("h"), Created: time.Now()}
	if err := s.CreateUser(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET email = (SELECT email FROM users WHERE id = ?) WHERE id = ?`, u.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, b.ID); got.Email != "" {
		t.Fatalf("copied address read as %q", got.Email)
	}
	s.Close()

	// Neither the database nor its write-ahead log holds an address.
	for _, f := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if l := strings.ToLower(string(raw)); strings.Contains(l, "example.com") {
			t.Fatalf("%s holds an address in the clear", filepath.Base(f))
		}
	}

	// Another key is refused; without a key, addresses read as empty.
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetEmailKey(ctx, bytes.Repeat([]byte{8}, 32)); !errors.Is(err, ErrWrongEmailKey) {
		t.Fatalf("other key: %v", err)
	}
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.Email != "" {
		t.Fatalf("without a key: %+v, %v", got, err)
	}
	if err := s.SetEmailKey(ctx, testEmailKey); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, u.ID); got.Email != "Alice@Example.com" {
		t.Fatalf("after reopening: %q", got.Email)
	}
}

func TestParseEmailKey(t *testing.T) {
	if _, err := ParseEmailKey(strings.Repeat("ab", 32) + "\n"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "xyz", strings.Repeat("ab", 31)} {
		if _, err := ParseEmailKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
