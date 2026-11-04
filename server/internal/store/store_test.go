package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cribbage/internal/engine"
)

type both interface {
	Store
	GameStore
}

func forEachStore(t *testing.T, fn func(t *testing.T, s both)) {
	t.Run("memory", func(t *testing.T) { fn(t, NewMemory()) })
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.SetEmailKey(context.Background(), testEmailKey); err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
}

func TestUsersAndSessions(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		u := &User{Username: "Alice", Nickname: "Al", Created: time.Now()}
		if err := s.CreateUser(ctx, u); err != nil || u.ID == 0 {
			t.Fatalf("create: %v id=%d", err, u.ID)
		}
		if err := s.CreateUser(ctx, &User{Username: "ALICE"}); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("duplicate: %v", err)
		}
		got, err := s.UserByUsername(ctx, "alice")
		if err != nil || got.ID != u.ID || got.Username != "Alice" {
			t.Fatalf("by name: %+v %v", got, err)
		}
		if _, err := s.UserByID(ctx, 999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing id: %v", err)
		}
		if err := s.CreateSession(ctx, "tok", u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, "old", u.ID, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if id, _, err := s.SessionUser(ctx, "tok"); err != nil || id != u.ID {
			t.Fatalf("session: %d %v", id, err)
		}
		if _, _, err := s.SessionUser(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired session: %v", err)
		}
		if n, err := s.DeleteExpiredSessions(ctx, time.Now()); err != nil || n != 1 {
			t.Fatalf("deleting expired sessions: %d, %v", n, err)
		}
		if err := s.DeleteSession(ctx, "tok"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.SessionUser(ctx, "tok"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted session: %v", err)
		}
		if err := s.DeleteSession(ctx, "never"); err != nil {
			t.Fatalf("deleting an unknown session: %v", err)
		}
	})
}

// The database holds hashes of session tokens, not the tokens.
func TestSQLiteSessionTokensHashed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	u := &User{Username: "alice", Nickname: "A", Created: time.Now()}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "new-token", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token FROM sessions`).Scan(&stored); err != nil || stored == "new-token" || len(stored) != 64 {
		t.Fatalf("stored token %q, %v", stored, err)
	}
	if id, _, err := s.SessionUser(ctx, "new-token"); err != nil || id != u.ID {
		t.Fatalf("session: %d, %v", id, err)
	}
	s.Close()
}

func play(seat int, c engine.Card) engine.Action { return engine.Play(c) }

func TestGames(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		g := &Game{ID: "g1", Seed: 1<<63 + 5, Status: StatusWaiting, Winner: -1,
			Seats: [2]Seat{{UserID: 1, Nickname: "Al"}}, Created: time.Now(), Updated: time.Now()}
		if err := s.CreateGame(ctx, g); err != nil {
			t.Fatal(err)
		}
		g.Status = StatusActive
		g.Seats[1] = Seat{Nickname: "heuristic bot", Bot: "heuristic"}
		acts := []Action{
			{Seq: 1, Seat: 0, Action: engine.Discard(3, 17)},
			{Seq: 2, Seat: 1, Action: engine.Discard(40, 51)},
		}
		if err := s.CommitGame(ctx, "g1", g, acts); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitGame(ctx, "g1", nil, []Action{{Seq: 3, Seat: 1, Action: play(1, 9)}}); err != nil {
			t.Fatal(err)
		}
		// A duplicate sequence number is rejected and changes nothing.
		if err := s.CommitGame(ctx, "g1", nil, []Action{{Seq: 3, Seat: 0, Action: play(0, 8)}}); err == nil {
			t.Fatal("duplicate seq accepted")
		}
		if err := s.CommitGame(ctx, "nope", g, nil); err == nil {
			t.Fatal("update of missing game accepted")
		}
		loaded, err := s.LoadUnfinished(ctx)
		if err != nil || len(loaded) != 1 {
			t.Fatalf("load: %v %d", err, len(loaded))
		}
		lg := loaded[0]
		if lg.Game.Seed != g.Seed || lg.Game.Status != StatusActive || lg.Game.Seats != g.Seats || lg.Game.Winner != -1 {
			t.Fatalf("loaded header %+v", lg.Game)
		}
		want := append(acts, Action{Seq: 3, Seat: 1, Action: play(1, 9)})
		if fmt.Sprint(lg.Actions) != fmt.Sprint(want) {
			t.Fatalf("loaded actions %v, want %v", lg.Actions, want)
		}

		g.Status, g.Winner, g.Scores = StatusFinished, 0, [2]int{121, 97}
		g.Updated = time.Now()
		if err := s.CommitGame(ctx, "g1", g, nil); err != nil {
			t.Fatal(err)
		}
		recent, err := s.RecentGames(ctx, 1, 10)
		if err != nil || len(recent) != 1 || recent[0].ID != "g1" || recent[0].Scores != [2]int{121, 97} || recent[0].Winner != 0 {
			t.Fatalf("recent: %+v %v", recent, err)
		}
		if other, _ := s.RecentGames(ctx, 2, 10); len(other) != 0 {
			t.Fatalf("recent for another user: %+v", other)
		}
		if loaded, _ := s.LoadUnfinished(ctx); len(loaded) != 0 {
			t.Fatalf("finished game still loaded: %v", loaded)
		}
		if sq, ok := s.(*SQLite); ok {
			fin, err := sq.LoadGame(ctx, "g1")
			if err != nil || fin.Game.Status != StatusFinished || fmt.Sprint(fin.Actions) != fmt.Sprint(want) {
				t.Fatalf("finished game: %+v %v", fin, err)
			}
		}
	})
}

func TestConcurrentCommits(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		const games, actions = 200, 50
		var wg sync.WaitGroup
		errs := make(chan error, games)
		for i := 0; i < games; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprint("g", i)
				if err := s.CreateGame(ctx, &Game{ID: id, Status: StatusActive, Winner: -1}); err != nil {
					errs <- err
					return
				}
				for seq := uint32(1); seq <= actions; seq++ {
					if err := s.CommitGame(ctx, id, nil, []Action{{Seq: seq, Action: play(0, engine.Card(seq%52))}}); err != nil {
						errs <- err
						return
					}
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		loaded, err := s.LoadUnfinished(ctx)
		if err != nil || len(loaded) != games {
			t.Fatalf("load: %v, %d games", err, len(loaded))
		}
		for _, g := range loaded {
			if len(g.Actions) != actions {
				t.Fatalf("%s has %d actions", g.Game.ID, len(g.Actions))
			}
		}
	})
}

func TestSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "re.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := &User{Username: "bob", Nickname: "Bob", Created: time.Now()}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGame(ctx, &Game{ID: "g", Status: StatusActive, Winner: -1}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitGame(ctx, "g", nil, []Action{{Seq: 1, Action: engine.Discard(1, 2)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.UserByUsername(ctx, "BOB"); err != nil || got.ID != u.ID {
		t.Fatalf("user after reopen: %v %v", got, err)
	}
	if loaded, err := s.LoadUnfinished(ctx); err != nil || len(loaded) != 1 || len(loaded[0].Actions) != 1 {
		t.Fatalf("games after reopen: %v %v", loaded, err)
	}
}

func BenchmarkSQLiteCommitParallel(b *testing.B) {
	s, err := OpenSQLite(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var n sync.Mutex
	next := 0
	b.SetParallelism(64) // many games committing at once, like a busy server
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n.Lock()
		next++
		id := fmt.Sprint("g", next)
		n.Unlock()
		if err := s.CreateGame(ctx, &Game{ID: id, Status: StatusActive, Winner: -1}); err != nil {
			b.Error(err)
			return
		}
		seq := uint32(0)
		for pb.Next() {
			seq++
			if err := s.CommitGame(ctx, id, nil, []Action{{Seq: seq, Action: engine.Play(5)}}); err != nil {
				b.Error(err)
				return
			}
		}
	})
	bt, ops := s.WriteStats()
	b.ReportMetric(float64(ops)/float64(bt), "ops/batch")
}

// BenchmarkSQLiteCommitSerial measures one small transaction at a time,
// the fixed per-transaction cost.
func BenchmarkSQLiteCommitSerial(b *testing.B) {
	s, err := OpenSQLite(filepath.Join(b.TempDir(), "serial.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.CreateGame(ctx, &Game{ID: "g", Status: StatusActive, Winner: -1}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.CommitGame(ctx, "g", nil, []Action{{Seq: uint32(i + 1), Action: engine.Play(5)}}); err != nil {
			b.Fatal(err)
		}
	}
}

func countLog(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM action_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSQLiteLogCollection(t *testing.T) {
	defer func(i time.Duration, r int64) { gcInterval, relocateSpan = i, r }(gcInterval, relocateSpan)
	gcInterval, relocateSpan = time.Hour, 50 // collect only when the test says so

	path := filepath.Join(t.TempDir(), "gc.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.LoadUnfinished(ctx); err != nil {
		t.Fatal(err)
	}
	commit := func(id string, seq uint32) {
		t.Helper()
		if err := s.CommitGame(ctx, id, nil, []Action{{Seq: seq, Seat: int(seq % 2), Action: engine.Play(engine.Card(seq % 52))}}); err != nil {
			t.Fatal(err)
		}
	}
	// "slow" stays live throughout; "fast" games come and go around it.
	if err := s.CreateGame(ctx, &Game{ID: "slow", Status: StatusActive, Winner: -1}); err != nil {
		t.Fatal(err)
	}
	commit("slow", 1)
	commit("slow", 2)
	for i := 0; i < 10; i++ {
		id := fmt.Sprint("fast", i)
		if err := s.CreateGame(ctx, &Game{ID: id, Status: StatusActive, Winner: -1}); err != nil {
			t.Fatal(err)
		}
		for seq := uint32(1); seq <= 20; seq++ {
			commit(id, seq)
		}
		if err := s.CommitGame(ctx, id, &Game{ID: id, Status: StatusFinished, Winner: 0}, nil); err != nil {
			t.Fatal(err)
		}
	}
	commit("slow", 3)
	if n := countLog(t, s); n != 203 {
		t.Fatalf("before collect: %d rows", n)
	}
	// "slow" started 200 rows behind the tail, past relocateSpan, so its rows
	// move to the tail and everything else can be deleted.
	if err := s.collect(); err != nil {
		t.Fatal(err)
	}
	if n := countLog(t, s); n != 3 {
		t.Fatalf("after collect: %d rows, want only slow's 3", n)
	}
	for i := 0; i < 10; i++ {
		g, err := s.LoadGame(ctx, fmt.Sprint("fast", i))
		if err != nil || len(g.Actions) != 20 {
			t.Fatalf("finished game lost actions: %v %v", g, err)
		}
	}
	commit("slow", 4)
	s.Close()

	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.LoadUnfinished(ctx)
	if err != nil || len(loaded) != 1 || len(loaded[0].Actions) != 4 {
		t.Fatalf("after reopen: %+v %v", loaded, err)
	}
	for i, a := range loaded[0].Actions {
		if a.Seq != uint32(i+1) || a.Action != engine.Play(engine.Card(a.Seq%52)) {
			t.Fatalf("action %d: %+v", i, a)
		}
	}
	// With no relocation needed, the prefix before the oldest live row goes.
	commit("slow", 5)
	if err := s.collect(); err != nil {
		t.Fatal(err)
	}
	if n := countLog(t, s); n != 5 {
		t.Fatalf("after second collect: %d rows", n)
	}
}

// A failing write in a batch fails alone, and costs one retry transaction
// rather than one per write in the batch.
func TestSQLiteBatchFailure(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	user := func(name string) error {
		return s.CreateUser(ctx, &User{Username: name, Nickname: name, Created: time.Now()})
	}
	if err := user("dup"); err != nil {
		t.Fatal(err)
	}
	before := s.txs.Load()
	const n = 500
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprint("u", i)
			if i%50 == 0 {
				name = "DUP"
			}
			errs[i] = user(name)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if want := i%50 == 0; errors.Is(err, ErrUsernameTaken) != want || (!want && err != nil) {
			t.Fatalf("write %d: %v", i, err)
		}
		if i%50 != 0 {
			if _, err := s.UserByUsername(ctx, fmt.Sprint("u", i)); err != nil {
				t.Fatalf("user %d not saved: %v", i, err)
			}
		}
	}
	if txs := s.txs.Load() - before; txs > n/5 {
		t.Fatalf("%d transactions for %d writes", txs, n)
	}
}

func TestAccountTokensAndSessions(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		a := &User{Username: "alice", Nickname: "A", Created: time.Now(), Email: "first@example.com"}
		b := &User{Username: "bob", Nickname: "B", Created: time.Now(), Bot: true}
		for _, u := range []*User{a, b} {
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		if u, _ := s.UserByID(ctx, a.ID); u.Email != "first@example.com" || u.Bot {
			t.Fatalf("created user: %+v", u)
		}
		if u, _ := s.UserByID(ctx, b.ID); !u.Bot || u.Email != "" {
			t.Fatalf("created bot: %+v", u)
		}
		for _, tok := range []string{"s1", "s2"} {
			if err := s.CreateSession(ctx, tok, a.ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CreateSession(ctx, "sb", b.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, "nobody", 999, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session for no user: %v", err)
		}

		// Sessions are renewed by moving their expiry.
		later := time.Now().Add(48 * time.Hour).Truncate(time.Millisecond)
		if err := s.ExtendSession(ctx, "s1", later); err != nil {
			t.Fatal(err)
		}
		if id, exp, err := s.SessionUser(ctx, "s1"); err != nil || id != a.ID || !exp.Equal(later) {
			t.Fatalf("extended session: %d %v %v", id, exp, err)
		}
		if err := s.ExtendSession(ctx, "unknown", later); err != nil {
			t.Fatalf("extending an unknown session: %v", err)
		}

		// Address changes: one pending token of each kind per user, single use.
		exp := time.Now().Add(time.Hour)
		for _, tok := range []*AccountToken{
			{Token: "v1", UserID: a.ID, Kind: TokenVerify, Email: "old@example.com", Expires: exp},
			{Token: "v2", UserID: a.ID, Kind: TokenVerify, Email: "A@Example.com", Expires: exp},
			{Token: "c1", UserID: a.ID, Kind: TokenChange, Email: "next@example.com", Expires: exp},
			{Token: "late", UserID: b.ID, Kind: TokenDelete, Expires: time.Now().Add(-time.Second)},
		} {
			if err := s.CreateAccountToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
		}
		if p, err := s.PendingEmail(ctx, a.ID, TokenVerify); err != nil || p != "A@Example.com" {
			t.Fatalf("pending %q, %v", p, err)
		}
		if p, err := s.PendingEmail(ctx, a.ID, TokenChange); err != nil || p != "next@example.com" {
			t.Fatalf("pending change %q, %v", p, err)
		}
		if _, err := s.TakeAccountToken(ctx, "v1", TokenVerify); !errors.Is(err, ErrNotFound) {
			t.Fatalf("replaced token: %v", err)
		}
		if _, err := s.TakeAccountToken(ctx, "v2", TokenDelete); !errors.Is(err, ErrNotFound) {
			t.Fatalf("token of the wrong kind: %v", err)
		}
		if _, err := s.TakeAccountToken(ctx, "late", TokenDelete); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired token: %v", err)
		}
		tok, err := s.TakeAccountToken(ctx, "v2", TokenVerify)
		if err != nil || tok.UserID != a.ID || tok.Email != "A@Example.com" {
			t.Fatalf("take: %+v, %v", tok, err)
		}
		if _, err := s.TakeAccountToken(ctx, "v2", TokenVerify); !errors.Is(err, ErrNotFound) {
			t.Fatalf("token used twice: %v", err)
		}
		if err := s.SetEmail(ctx, a.ID, tok.Email); err != nil {
			t.Fatal(err)
		}
		// A new address ends the account's other links.
		if _, err := s.TakeAccountToken(ctx, "c1", TokenChange); !errors.Is(err, ErrNotFound) {
			t.Fatal("change link survived a new address")
		}
		// A bot given an address is a bot no more.
		if err := s.SetEmail(ctx, b.ID, "a@example.com"); err != nil {
			t.Fatal(err)
		}
		if u, _ := s.UserByID(ctx, b.ID); u.Bot {
			t.Fatal("a bot with an address is still a bot")
		}
		us, err := s.UsersByEmail(ctx, "a@EXAMPLE.com")
		if err != nil || len(us) != 2 || us[0].ID != a.ID || us[1].ID != b.ID {
			t.Fatalf("users by email: %v, %v", us, err)
		}
		if us, _ := s.UsersByEmail(ctx, ""); len(us) != 0 {
			t.Fatalf("users with no email: %v", us)
		}

		// Signing out others keeps one session and leaves other users alone.
		if err := s.DeleteSessions(ctx, a.ID, "s2"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.SessionUser(ctx, "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("other session survived")
		}
		if _, _, err := s.SessionUser(ctx, "s2"); err != nil {
			t.Fatal("kept session ended")
		}
		if _, _, err := s.SessionUser(ctx, "sb"); err != nil {
			t.Fatal("another user's session ended")
		}
		if err := s.DeleteSessions(ctx, a.ID, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.SessionUser(ctx, "s2"); !errors.Is(err, ErrNotFound) {
			t.Fatal("session survived signing out everywhere")
		}
	})
}

func TestPasskeys(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		a := &User{Username: "alice", Nickname: "A", Created: time.Now(), Email: "a@example.com"}
		b := &User{Username: "bob", Nickname: "B", Created: time.Now(), Email: "b@example.com"}
		for _, u := range []*User{a, b} {
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		t0 := time.Now().Truncate(time.Millisecond)
		for i, p := range []*Passkey{
			{ID: []byte("k1"), UserID: a.ID, Name: "Phone", Data: []byte("d1"), Created: t0},
			{ID: []byte("k2"), UserID: a.ID, Name: "Laptop", Data: []byte("d2"), Created: t0.Add(time.Second)},
			{ID: []byte("k3"), UserID: b.ID, Name: "Bob's", Data: []byte("d3"), Created: t0},
		} {
			if err := s.AddPasskey(ctx, p); err != nil {
				t.Fatalf("add %d: %v", i, err)
			}
		}
		if err := s.AddPasskey(ctx, &Passkey{ID: []byte("k1"), UserID: b.ID, Data: []byte("x"), Created: t0}); err == nil {
			t.Fatal("a credential ID was stored twice")
		}
		keys, err := s.Passkeys(ctx, a.ID)
		if err != nil || len(keys) != 2 || string(keys[0].ID) != "k1" || keys[1].Name != "Laptop" || !keys[0].LastUsed.IsZero() {
			t.Fatalf("passkeys: %+v, %v", keys, err)
		}
		used := t0.Add(time.Minute)
		if err := s.UpdatePasskey(ctx, &Passkey{ID: []byte("k1"), UserID: a.ID, Data: []byte("d1b"), LastUsed: used}); err != nil {
			t.Fatal(err)
		}
		// Another user's passkey is not theirs to change or remove.
		if err := s.UpdatePasskey(ctx, &Passkey{ID: []byte("k3"), UserID: a.ID, Data: []byte("evil"), LastUsed: used}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePasskey(ctx, a.ID, []byte("k3")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("removing another user's passkey: %v", err)
		}
		keys, _ = s.Passkeys(ctx, a.ID)
		if string(keys[0].Data) != "d1b" || !keys[0].LastUsed.Equal(used) {
			t.Fatalf("updated passkey: %+v", keys[0])
		}
		if bk, _ := s.Passkeys(ctx, b.ID); len(bk) != 1 || string(bk[0].Data) != "d3" {
			t.Fatalf("bob's passkeys: %+v", bk)
		}
		if err := s.DeletePasskey(ctx, a.ID, []byte("k1")); err != nil {
			t.Fatal(err)
		}
		if keys, _ := s.Passkeys(ctx, a.ID); len(keys) != 1 || string(keys[0].ID) != "k2" {
			t.Fatalf("after removing one: %+v", keys)
		}
		if err := s.DeletePasskeys(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if keys, _ := s.Passkeys(ctx, a.ID); len(keys) != 0 {
			t.Fatalf("after removing all: %+v", keys)
		}
		// Deleting a user removes their passkeys.
		if err := s.DeleteUser(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if bk, _ := s.Passkeys(ctx, b.ID); len(bk) != 0 {
			t.Fatalf("a deleted user's passkeys: %+v", bk)
		}
	})
}

func TestHistoryAndRecords(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		base := time.UnixMilli(1_700_000_000_000)
		me := Seat{UserID: 1, Nickname: "Me"}
		bob := Seat{UserID: 2, Nickname: "Bob"}
		bot := Seat{Nickname: "expert bot", Bot: "expert"}
		finish := func(id string, seats [2]Seat, winner int, at time.Duration) {
			t.Helper()
			g := &Game{ID: id, Status: StatusActive, Seats: seats, Winner: -1, Created: base, Updated: base}
			if err := s.CreateGame(ctx, g); err != nil {
				t.Fatal(err)
			}
			g.Status, g.Winner, g.Updated = StatusFinished, winner, base.Add(at)
			if err := s.CommitGame(ctx, id, g, nil); err != nil {
				t.Fatal(err)
			}
		}
		finish("g1", [2]Seat{me, bot}, 0, 1*time.Minute)
		finish("g2", [2]Seat{bob, me}, 0, 2*time.Minute)
		finish("g3", [2]Seat{me, bot}, 1, 3*time.Minute)
		finish("g4", [2]Seat{me, {}}, -1, 4*time.Minute) // open game nobody joined
		bob.Nickname = "Robert"
		finish("g5", [2]Seat{me, bob}, -1, 5*time.Minute) // expired
		finish("g6", [2]Seat{bob, me}, 1, 5*time.Minute)  // same time as g5
		finish("g7", [2]Seat{bob, bot}, 0, 6*time.Minute) // not mine

		var ids []string
		var cur Cursor
		for {
			page, err := s.History(ctx, 1, cur, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page {
				ids = append(ids, g.ID)
			}
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			cur = Cursor{Updated: last.Updated, ID: last.ID}
		}
		if got := strings.Join(ids, ","); got != "g6,g5,g3,g2,g1" {
			t.Fatalf("history %s", got)
		}

		recs, err := s.Records(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 2 {
			t.Fatalf("records %+v", recs)
		}
		r := recs[0] // Bob: 3 games, the latest as Robert
		if r.Opponent.UserID != 2 || r.Opponent.Nickname != "Robert" || r.Games != 3 || r.Wins != 1 || r.Losses != 1 || r.Unfinished != 1 ||
			!r.Last.Equal(base.Add(5*time.Minute)) {
			t.Fatalf("record vs Bob %+v", r)
		}
		r = recs[1]
		if r.Opponent.Bot != "expert" || r.Opponent.UserID != 0 || r.Games != 2 || r.Wins != 1 || r.Losses != 1 || r.Unfinished != 0 {
			t.Fatalf("record vs bot %+v", r)
		}
	})
}

// Deleting a user removes the account, and their finished games stay in
// their opponent's history under DeletedNickname. IDs are not reused.
func TestDeleteUser(t *testing.T) {
	forEachStore(t, func(t *testing.T, s both) {
		ctx := context.Background()
		a := &User{Username: "alice", Nickname: "Al", Created: time.Now()}
		b := &User{Username: "bob", Nickname: "Bo", Created: time.Now()}
		for _, u := range []*User{b, a} { // alice has the highest ID
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CreateSession(ctx, "sa", a.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateAccountToken(ctx, &AccountToken{Token: "d", UserID: a.ID, Kind: TokenDelete, Expires: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		g := &Game{ID: "g1", Status: StatusActive, Winner: -1, Created: now, Updated: now,
			Seats: [2]Seat{{UserID: a.ID, Nickname: "Al"}, {UserID: b.ID, Nickname: "Bo"}}}
		if err := s.CreateGame(ctx, g); err != nil {
			t.Fatal(err)
		}
		g.Status, g.Winner = StatusFinished, 1
		if err := s.CommitGame(ctx, g.ID, g, nil); err != nil {
			t.Fatal(err)
		}
		for _, inv := range []*Invite{
			{ID: "i1", From: a.ID, FromName: "Al", To: b.ID, ToName: "Bo", Created: now, Expires: now.Add(time.Hour)},
			{ID: "i2", From: b.ID, FromName: "Bo", Created: now, Code: "ABC-DEF", Expires: now.Add(time.Hour)},
		} {
			if err := s.CreateInvite(ctx, inv); err != nil {
				t.Fatal(err)
			}
		}

		if err := s.DeleteUser(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ForgetUser(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UserByID(ctx, a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user still there: %v", err)
		}
		if _, err := s.UserByUsername(ctx, "alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("username still taken: %v", err)
		}
		if _, _, err := s.SessionUser(ctx, "sa"); !errors.Is(err, ErrNotFound) {
			t.Fatal("session survived")
		}
		if _, err := s.TakeAccountToken(ctx, "d", TokenDelete); !errors.Is(err, ErrNotFound) {
			t.Fatal("token survived")
		}
		hist, err := s.History(ctx, b.ID, Cursor{}, 10)
		if err != nil || len(hist) != 1 || hist[0].Seats[0].Nickname != DeletedNickname || hist[0].Seats[0].UserID != a.ID {
			t.Fatalf("opponent's history: %+v, %v", hist, err)
		}
		if recs, _ := s.Records(ctx, b.ID); len(recs) != 1 || recs[0].Opponent.Nickname != DeletedNickname {
			t.Fatalf("opponent's records: %+v", recs)
		}
		invs, _ := s.LoadInvites(ctx)
		if len(invs) != 1 || invs[0].ID != "i2" {
			t.Fatalf("invites: %+v", invs)
		}
		c := &User{Username: "alice", Nickname: "New", Created: time.Now()}
		if err := s.CreateUser(ctx, c); err != nil {
			t.Fatal(err)
		}
		if c.ID == a.ID {
			t.Fatal("a deleted user's ID was reused")
		}
	})
}
