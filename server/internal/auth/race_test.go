package auth

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/store"
)

// resetDuringLogin changes the password just after Login has loaded the
// user, as a reset that lands while bcrypt runs would.
type resetDuringLogin struct {
	store.Store
}

func (r resetDuringLogin) UserByUsername(ctx context.Context, name string) (*store.User, error) {
	u, err := r.Store.UserByUsername(ctx, name)
	if err == nil {
		hash, _ := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
		err = r.Store.SetPassword(ctx, u.ID, hash, false, "")
	}
	return u, err
}

// A login whose password was right when checked, but changed before its
// session was made, gets no session.
func TestLoginRacingReset(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	s := &Service{Store: mem, BcryptCost: bcrypt.MinCost}
	if _, err := s.Register(ctx, "alice", "password123", "A"); err != nil {
		t.Fatal(err)
	}
	s.Store = resetDuringLogin{mem}
	if res, err := s.Login(ctx, "alice", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login with the old password: %+v, %v", res, err)
	}
}
