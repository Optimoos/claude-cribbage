package auth

import (
	"context"
	"testing"
	"time"

	"cribbage/internal/store"
)

// An unknown username takes about as long as a wrong password, so timing
// does not reveal which usernames exist.
func TestLoginTimingHidesUsernames(t *testing.T) {
	s := &Service{Store: store.NewMemory(), BcryptCost: 8}
	ctx := context.Background()
	if _, err := s.Register(ctx, "alice", "password123", ""); err != nil {
		t.Fatal(err)
	}
	time1 := func(user string) time.Duration {
		best := time.Hour
		for i := 0; i < 5; i++ {
			start := time.Now()
			if _, err := s.Login(ctx, user, "wrongpassword"); err != ErrInvalidCredentials {
				t.Fatalf("login %s: %v", user, err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	known, unknown := time1("alice"), time1("nobody")
	if unknown < known/2 {
		t.Fatalf("unknown user took %v, a wrong password %v", unknown, known)
	}
}
