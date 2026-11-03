package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"cribbage/internal/engine"
)

// Actions of unfinished games are appended to action_log, whose rowid
// always increases, so a transaction full of writes from many games only
// touches the last few pages. When a game finishes, its whole log is packed
// into games.actions; its action_log rows are removed later by collect,
// which keeps action_log small and startup fast.
const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
	nickname      TEXT NOT NULL,
	password_hash BLOB NOT NULL,
	created       INTEGER NOT NULL,
	email         TEXT NOT NULL DEFAULT '', -- verified, for reset links
	must_change   INTEGER NOT NULL DEFAULT 0 -- the admin reset the password
);
CREATE INDEX IF NOT EXISTS users_email ON users(email COLLATE NOCASE) WHERE email != '';
CREATE TABLE IF NOT EXISTS sessions (
	token   TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS games (
	id         TEXT PRIMARY KEY,
	seed       INTEGER NOT NULL,
	status     INTEGER NOT NULL,
	seat0_user INTEGER NOT NULL, seat0_nick TEXT NOT NULL, seat0_bot TEXT NOT NULL,
	seat1_user INTEGER NOT NULL, seat1_nick TEXT NOT NULL, seat1_bot TEXT NOT NULL,
	winner     INTEGER NOT NULL,
	score0     INTEGER NOT NULL DEFAULT 0,
	score1     INTEGER NOT NULL DEFAULT 0,
	resigned   INTEGER NOT NULL,
	created    INTEGER NOT NULL,
	updated    INTEGER NOT NULL,
	actions    BLOB -- packed action log, set when the game finishes
);
CREATE INDEX IF NOT EXISTS games_unfinished ON games(status) WHERE status != 3;
CREATE INDEX IF NOT EXISTS games_seat0 ON games(seat0_user, updated) WHERE status = 3;
CREATE INDEX IF NOT EXISTS games_seat1 ON games(seat1_user, updated) WHERE status = 3;
CREATE TABLE IF NOT EXISTS invites (
	id        TEXT PRIMARY KEY,
	from_user INTEGER NOT NULL,
	from_nick TEXT NOT NULL,
	to_user   INTEGER NOT NULL, -- 0 for a code invite
	to_nick   TEXT NOT NULL,
	code      TEXT NOT NULL,
	created   INTEGER NOT NULL,
	expires   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_tokens (
	token   TEXT PRIMARY KEY, -- SHA-256 of the token, like sessions
	user_id INTEGER NOT NULL REFERENCES users(id),
	kind    INTEGER NOT NULL,
	email   TEXT NOT NULL,
	expires INTEGER NOT NULL,
	UNIQUE (user_id, kind)
);
CREATE TABLE IF NOT EXISTS action_log (
	id      INTEGER PRIMARY KEY,
	game_id TEXT NOT NULL,
	seq     INTEGER NOT NULL,
	seat    INTEGER NOT NULL,
	kind    INTEGER NOT NULL,
	card0   INTEGER NOT NULL,
	card1   INTEGER NOT NULL
);
`

// SQLite is a Store and GameStore backed by a SQLite database in WAL mode,
// opened exclusively by this process over one connection. All writes go
// through one goroutine that groups queued writes into a single transaction
// ("group commit").
type SQLite struct {
	db      *sql.DB
	writes  chan *writeOp
	backups chan backupReq
	done    chan struct{}

	insertAction *sql.Stmt

	// live tracks the action_log rows of each unfinished game. It is only
	// modified after a successful commit, on the writer goroutine.
	lmu    sync.Mutex
	live   map[string]*liveGame
	loaded atomic.Bool  // LoadUnfinished has run, so live is complete
	gcErr  atomic.Value // last garbage collection error (string)

	batches, ops atomic.Int64
	txs          atomic.Int64 // transactions begun, including retries
}

type liveGame struct {
	rowids []int64
	acts   []Action
}

type writeOp struct {
	fn       func(*writeTx) error
	onCommit func() // runs only if the transaction containing fn commits
	err      chan error
}

// writeTx is a transaction shared by a batch of writes, with statements
// prepared at most once per transaction.
type writeTx struct {
	*sql.Tx
	s   *SQLite
	ins *sql.Stmt
}

func (w *writeTx) insertAction() *sql.Stmt {
	if w.ins == nil {
		w.ins = w.Stmt(w.s.insertAction)
	}
	return w.ins
}

// maxBatch bounds how many queued writes share one transaction.
const maxBatch = 2048

// OpenSQLite opens (creating if needed) the database at path. Call
// LoadUnfinished once before relying on the action log being cleaned up.
func OpenSQLite(path string) (*SQLite, error) {
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(10000)", "foreign_keys(ON)",
		"cache_size(-65536)", "wal_autocheckpoint(20000)",
		// The log grows to about 80 MB between checkpoints; shrink it back
		// afterwards instead of keeping the file at its largest.
		"journal_size_limit(16777216)",
		// Only this process opens the database. Exclusive mode skips the
		// per-transaction file locking and shared-memory index, which
		// dominates commit cost on Windows.
		"locking_mode(EXCLUSIVE)",
	} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// Exclusive locking needs a single connection; reads wait for the
	// writer's current transaction, which is short.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	ins, err := db.Prepare(`INSERT INTO action_log (game_id, seq, seat, kind, card0, card1) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &SQLite{
		db: db, writes: make(chan *writeOp, 4*maxBatch), backups: make(chan backupReq), done: make(chan struct{}),
		insertAction: ins, live: map[string]*liveGame{},
	}
	go s.writer()
	return s, nil
}

// Close flushes pending writes and closes the database. No other methods
// may be called concurrently with or after Close.
func (s *SQLite) Close() error {
	close(s.writes)
	<-s.done
	s.insertAction.Close()
	return s.db.Close()
}

// WriteStats reports how many transactions and writes have been committed.
func (s *SQLite) WriteStats() (batches, ops int64) { return s.batches.Load(), s.ops.Load() }

func (s *SQLite) writer() {
	defer close(s.done)
	gc := time.NewTicker(gcInterval)
	defer gc.Stop()
	batch := make([]*writeOp, 0, maxBatch)
	for {
		select {
		case op, ok := <-s.writes:
			if !ok {
				return
			}
			batch = append(batch[:0], op)
		fill:
			for len(batch) < maxBatch {
				select {
				case op, ok := <-s.writes:
					if !ok {
						break fill
					}
					batch = append(batch, op)
				default:
					break fill
				}
			}
			s.commit(batch)
			s.batches.Add(1)
			s.ops.Add(int64(len(batch)))
		case b := <-s.backups:
			_, err := s.db.Exec(`VACUUM INTO ?`, b.path)
			b.err <- err
		case <-gc.C:
			if s.loaded.Load() {
				if err := s.collect(); err != nil {
					s.gcErr.Store(err.Error())
				}
			}
		}
	}
}

// Garbage collection of action_log. Finished games' rows are not deleted
// one game at a time (a game's rows are interleaved with every other live
// game's, so that would dirty ~1 page per action). Instead the log's prefix
// older than the oldest live game is periodically deleted in one sweep, and
// a live game that has fallen far behind the tail (a long idle game) has its
// rows copied to the tail so it stops pinning the prefix.
var (
	gcInterval   = 2 * time.Second
	relocateSpan = int64(200_000) // rows behind the tail before a game is moved
)

// collect runs on the writer goroutine, so no commit is in progress.
func (s *SQLite) collect() error {
	var tail int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM action_log`).Scan(&tail); err != nil {
		return err
	}
	type straggler struct {
		lg  *liveGame
		id  string
		ids []int64
	}
	var move []straggler
	s.lmu.Lock()
	for id, lg := range s.live {
		if len(lg.rowids) > 0 && lg.rowids[0] < tail-relocateSpan && len(move) < 64 {
			move = append(move, straggler{lg: lg, id: id})
		}
	}
	s.lmu.Unlock()

	if len(move) > 0 {
		err := s.runTx(func(tx *writeTx) error {
			ins := tx.insertAction()
			for i := range move {
				m := &move[i]
				m.ids = m.ids[:0]
				for _, a := range m.lg.acts {
					res, err := ins.Exec(m.id, a.Seq, a.Seat, int(a.Action.Kind), int(a.Action.Cards[0]), int(a.Action.Cards[1]))
					if err != nil {
						return err
					}
					rid, err := res.LastInsertId()
					if err != nil {
						return err
					}
					m.ids = append(m.ids, rid)
				}
				if err := deleteRows(tx, m.lg.rowids); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		s.lmu.Lock()
		for _, m := range move {
			m.lg.rowids = m.ids
		}
		s.lmu.Unlock()
	}

	// Everything before the oldest live row belongs to finished games.
	cutoff := tail + 1
	s.lmu.Lock()
	for _, lg := range s.live {
		if len(lg.rowids) > 0 && lg.rowids[0] < cutoff {
			cutoff = lg.rowids[0]
		}
	}
	s.lmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM action_log WHERE id < ?`, cutoff)
	return err
}

// commit runs a batch in one transaction. If anything fails, the batch
// runs again in one more transaction with each op in its own savepoint, so
// a failing op (a username already taken, say) is undone alone and the
// rest still commit together. Savepoints on every batch would halve write
// throughput.
func (s *SQLite) commit(batch []*writeOp) {
	err := s.runTx(func(tx *writeTx) error {
		for _, op := range batch {
			if err := op.fn(tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && len(batch) > 1 {
		errs := make([]error, len(batch))
		err = s.runTx(func(tx *writeTx) error {
			for i, op := range batch {
				if _, err := tx.Exec(`SAVEPOINT op`); err != nil {
					return err
				}
				if errs[i] = op.fn(tx); errs[i] != nil {
					if _, err := tx.Exec(`ROLLBACK TO op`); err != nil {
						return err // e.g. SQLite already rolled the transaction back
					}
				}
				if _, err := tx.Exec(`RELEASE op`); err != nil {
					return err
				}
			}
			return nil
		})
		for i, op := range batch {
			if err != nil {
				errs[i] = err
			}
			s.finish(op, errs[i])
		}
		return
	}
	for _, op := range batch {
		s.finish(op, err)
	}
}

func (s *SQLite) finish(op *writeOp, err error) {
	if err == nil && op.onCommit != nil {
		op.onCommit()
	}
	op.err <- err
}

func (s *SQLite) runTx(fn func(*writeTx) error) error {
	s.txs.Add(1)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(&writeTx{Tx: tx, s: s}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// write queues fn and waits until its transaction commits. fn may run more
// than once, so it must not have side effects outside the transaction; put
// those in onCommit.
func (s *SQLite) write(ctx context.Context, fn func(*writeTx) error, onCommit func()) error {
	op := &writeOp{fn: fn, onCommit: onCommit, err: make(chan error, 1)}
	select {
	case s.writes <- op:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Once queued the write will happen; wait for it even if ctx ends so the
	// caller never mistakes a committed write for a failed one.
	return <-op.err
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func (s *SQLite) CreateUser(ctx context.Context, u *User) error {
	var id int64
	err := s.write(ctx, func(tx *writeTx) error {
		res, err := tx.Exec(`INSERT INTO users (username, nickname, password_hash, created) VALUES (?, ?, ?, ?)`,
			u.Username, u.Nickname, u.PasswordHash, u.Created.UnixMilli())
		if isUniqueViolation(err) {
			return ErrUsernameTaken
		}
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}, nil)
	if err == nil {
		u.ID = id
	}
	return err
}

func (s *SQLite) scanUser(row *sql.Row) (*User, error) {
	u, err := scanUserRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func scanUserRow(row scanner) (*User, error) {
	var u User
	var created int64
	var must int
	if err := row.Scan(&u.ID, &u.Username, &u.Nickname, &u.PasswordHash, &created, &u.Email, &must); err != nil {
		return nil, err
	}
	u.Created, u.MustChange = time.UnixMilli(created), must != 0
	return &u, nil
}

const userCols = `id, username, nickname, password_hash, created, email, must_change`

func (s *SQLite) UserByUsername(ctx context.Context, username string) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *SQLite) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// Sessions are stored by a SHA-256 of their token, so a copy of the
// database (a backup, say) holds no usable tokens. Tokens are random, so an
// unsalted hash is enough.
func tokenKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *SQLite) CreateSession(ctx context.Context, token string, userID int64, hash []byte, expires time.Time) error {
	return s.write(ctx, func(tx *writeTx) error {
		res, err := tx.Exec(`INSERT INTO sessions (token, user_id, expires)
			SELECT ?, id, ? FROM users WHERE id = ? AND password_hash = ?`,
			tokenKey(token), expires.UnixMilli(), userID, hash)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		return nil
	}, nil)
}

func (s *SQLite) SessionUser(ctx context.Context, token string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE token = ? AND expires > ?`,
		tokenKey(token), time.Now().UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func (s *SQLite) DeleteSession(ctx context.Context, token string) error {
	return s.write(ctx, func(tx *writeTx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE token = ?`, tokenKey(token))
		return err
	}, nil)
}

func (s *SQLite) DeleteExpiredSessions(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *writeTx) error {
		res, err := tx.Exec(`DELETE FROM sessions WHERE expires < ?`, t.UnixMilli())
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM account_tokens WHERE expires < ?`, t.UnixMilli())
		return err
	}, nil)
	return n, err
}

func (s *SQLite) SetPassword(ctx context.Context, userID int64, hash []byte, mustChange bool, keep string) error {
	return s.write(ctx, func(tx *writeTx) error {
		res, err := tx.Exec(`UPDATE users SET password_hash = ?, must_change = ? WHERE id = ?`, hash, boolInt(mustChange), userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND token != ?`, userID, keepKey(keep)); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM account_tokens WHERE user_id = ?`, userID)
		return err
	}, nil)
}

// keepKey is the stored key of a session to keep, or one that matches none.
func keepKey(token string) string {
	if token == "" {
		return ""
	}
	return tokenKey(token)
}

func (s *SQLite) SetEmail(ctx context.Context, userID int64, email string) error {
	return s.write(ctx, func(tx *writeTx) error {
		res, err := tx.Exec(`UPDATE users SET email = ? WHERE id = ?`, email, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(`DELETE FROM account_tokens WHERE user_id = ?`, userID)
		return err
	}, nil)
}

func (s *SQLite) UsersByEmail(ctx context.Context, email string) ([]*User, error) {
	if email == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ? COLLATE NOCASE AND email != '' ORDER BY id`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *SQLite) CreateAccountToken(ctx context.Context, t *AccountToken) error {
	return s.write(ctx, func(tx *writeTx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO account_tokens (token, user_id, kind, email, expires) VALUES (?, ?, ?, ?, ?)`,
			tokenKey(t.Token), t.UserID, t.Kind, t.Email, t.Expires.UnixMilli())
		return err
	}, nil)
}

func (s *SQLite) TakeAccountToken(ctx context.Context, token string, kind int) (*AccountToken, error) {
	var out *AccountToken
	err := s.write(ctx, func(tx *writeTx) error {
		t := AccountToken{Token: token, Kind: kind}
		var expires int64
		err := tx.QueryRow(`DELETE FROM account_tokens WHERE token = ? AND kind = ? RETURNING user_id, email, expires`,
			tokenKey(token), kind).Scan(&t.UserID, &t.Email, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		t.Expires = time.UnixMilli(expires)
		if !time.Now().Before(t.Expires) {
			return nil // used up either way, but not usable
		}
		out = &t
		return nil
	}, nil)
	if err == nil && out == nil {
		err = ErrNotFound
	}
	return out, err
}

func (s *SQLite) PendingEmail(ctx context.Context, userID int64) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx, `SELECT email FROM account_tokens WHERE user_id = ? AND kind = ? AND expires > ?`,
		userID, TokenVerify, time.Now().UnixMilli()).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return email, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLite) CreateGame(ctx context.Context, g *Game) error {
	return s.write(ctx, func(tx *writeTx) error {
		_, err := tx.Exec(`INSERT INTO games (id, seed, status,
			seat0_user, seat0_nick, seat0_bot, seat1_user, seat1_nick, seat1_bot,
			winner, score0, score1, resigned, created, updated) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			g.ID, int64(g.Seed), g.Status,
			g.Seats[0].UserID, g.Seats[0].Nickname, g.Seats[0].Bot,
			g.Seats[1].UserID, g.Seats[1].Nickname, g.Seats[1].Bot,
			g.Winner, g.Scores[0], g.Scores[1], boolInt(g.Resigned), g.Created.UnixMilli(), g.Updated.UnixMilli())
		return err
	}, func() {
		s.lmu.Lock()
		s.live[g.ID] = &liveGame{}
		s.lmu.Unlock()
	})
}

func updateGame(tx *writeTx, id string, g *Game, packed []byte) error {
	res, err := tx.Exec(`UPDATE games SET seed=?, status=?,
		seat0_user=?, seat0_nick=?, seat0_bot=?, seat1_user=?, seat1_nick=?, seat1_bot=?,
		winner=?, score0=?, score1=?, resigned=?, updated=?, actions=? WHERE id=?`,
		int64(g.Seed), g.Status,
		g.Seats[0].UserID, g.Seats[0].Nickname, g.Seats[0].Bot,
		g.Seats[1].UserID, g.Seats[1].Nickname, g.Seats[1].Bot,
		g.Winner, g.Scores[0], g.Scores[1], boolInt(g.Resigned), g.Updated.UnixMilli(), packed, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) CommitGame(ctx context.Context, id string, g *Game, acts []Action) error {
	s.lmu.Lock()
	lg := s.live[id]
	s.lmu.Unlock()
	if lg == nil {
		return fmt.Errorf("game %s: %w (not created or loaded by this store)", id, ErrNotFound)
	}
	// The owner commits sequentially, so lg cannot change until we return.
	for i, a := range acts {
		if want := uint32(len(lg.acts) + i + 1); a.Seq != want {
			return fmt.Errorf("game %s: action seq %d, want %d", id, a.Seq, want)
		}
	}

	if g != nil && g.Status == StatusFinished {
		all := append(append([]Action(nil), lg.acts...), acts...)
		packed := packActions(all)
		// The game's action_log rows are left for the next collect sweep.
		return s.write(ctx, func(tx *writeTx) error {
			return updateGame(tx, id, g, packed)
		}, func() {
			s.lmu.Lock()
			delete(s.live, id)
			s.lmu.Unlock()
		})
	}

	var rowids []int64
	return s.write(ctx, func(tx *writeTx) error {
		rowids = rowids[:0]
		ins := tx.insertAction()
		for _, a := range acts {
			res, err := ins.Exec(id, a.Seq, a.Seat, int(a.Action.Kind), int(a.Action.Cards[0]), int(a.Action.Cards[1]))
			if err != nil {
				return err
			}
			rid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			rowids = append(rowids, rid)
		}
		if g != nil {
			return updateGame(tx, id, g, nil)
		}
		return nil
	}, func() {
		s.lmu.Lock()
		lg.rowids = append(lg.rowids, rowids...)
		lg.acts = append(lg.acts, acts...)
		s.lmu.Unlock()
	})
}

func deleteRows(tx *writeTx, ids []int64) error {
	const chunk = 500
	for len(ids) > 0 {
		n := min(chunk, len(ids))
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		q := `DELETE FROM action_log WHERE id IN (?` + strings.Repeat(",?", n-1) + `)`
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// packActions encodes actions as 4 bytes each: seat, kind, card0, card1.
// Sequence numbers are implied by position.
func packActions(acts []Action) []byte {
	b := make([]byte, 0, 4*len(acts))
	for _, a := range acts {
		b = append(b, byte(a.Seat), byte(a.Action.Kind), byte(a.Action.Cards[0]), byte(a.Action.Cards[1]))
	}
	return b
}

func unpackActions(b []byte) ([]Action, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("packed actions: bad length %d", len(b))
	}
	acts := make([]Action, len(b)/4)
	for i := range acts {
		p := b[4*i:]
		acts[i] = Action{Seq: uint32(i + 1), Seat: int(p[0]), Action: engine.Action{
			Kind: engine.ActionKind(p[1]), Cards: [2]engine.Card{engine.Card(p[2]), engine.Card(p[3])},
		}}
	}
	return acts, nil
}

const gameCols = `id, seed, status, seat0_user, seat0_nick, seat0_bot, seat1_user, seat1_nick, seat1_bot,
	winner, score0, score1, resigned, created, updated, actions`

type scanner interface{ Scan(dest ...any) error }

func scanGame(row scanner) (Game, []byte, error) {
	var g Game
	var seed, created, updated int64
	var resigned int
	var packed []byte
	err := row.Scan(&g.ID, &seed, &g.Status,
		&g.Seats[0].UserID, &g.Seats[0].Nickname, &g.Seats[0].Bot,
		&g.Seats[1].UserID, &g.Seats[1].Nickname, &g.Seats[1].Bot,
		&g.Winner, &g.Scores[0], &g.Scores[1], &resigned, &created, &updated, &packed)
	g.Seed, g.Resigned = uint64(seed), resigned != 0
	g.Created, g.Updated = time.UnixMilli(created), time.UnixMilli(updated)
	return g, packed, err
}

// LoadUnfinished returns every waiting or active game with its actions and
// prepares the store to accept commits for them.
func (s *SQLite) LoadUnfinished(ctx context.Context) ([]GameWithActions, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+gameCols+` FROM games WHERE status != ?`, StatusFinished)
	if err != nil {
		return nil, err
	}
	var out []GameWithActions
	index := map[string]int{}
	for rows.Next() {
		g, _, err := scanGame(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		index[g.ID] = len(out)
		out = append(out, GameWithActions{Game: g})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	live := make(map[string]*liveGame, len(out))
	for _, g := range out {
		live[g.Game.ID] = &liveGame{}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id, game_id, seq, seat, kind, card0, card1 FROM action_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		var id string
		var a Action
		var kind, c0, c1 int
		if err := rows.Scan(&rid, &id, &a.Seq, &a.Seat, &kind, &c0, &c1); err != nil {
			return nil, err
		}
		a.Action = engine.Action{Kind: engine.ActionKind(kind), Cards: [2]engine.Card{engine.Card(c0), engine.Card(c1)}}
		i, ok := index[id]
		if !ok {
			continue
		}
		if want := uint32(len(out[i].Actions) + 1); a.Seq != want {
			return nil, fmt.Errorf("game %s: action log has seq %d, want %d", id, a.Seq, want)
		}
		out[i].Actions = append(out[i].Actions, a)
		live[id].rowids = append(live[id].rowids, rid)
		live[id].acts = append(live[id].acts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.lmu.Lock()
	for id, lg := range live {
		s.live[id] = lg
	}
	s.lmu.Unlock()
	s.loaded.Store(true)
	return out, nil
}

func (s *SQLite) RecentGames(ctx context.Context, userID int64, limit int) ([]Game, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+gameCols+` FROM games WHERE status = ? AND seat0_user = ?
		UNION ALL SELECT `+gameCols+` FROM games WHERE status = ? AND seat1_user = ?
		ORDER BY updated DESC LIMIT ?`, StatusFinished, userID, StatusFinished, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Game
	for rows.Next() {
		g, _, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// played is the condition that a finished game of a user in seat N had an
// opponent: the other seat was taken.
const playedBySeat0 = `status = 3 AND seat0_user = ? AND (seat1_user != 0 OR seat1_bot != '')`
const playedBySeat1 = `status = 3 AND seat1_user = ? AND (seat0_user != 0 OR seat0_bot != '')`

func (s *SQLite) History(ctx context.Context, userID int64, before Cursor, limit int) ([]Game, error) {
	// Each half uses its seat's (seatN_user, updated) index.
	after := ""
	args := []any{userID}
	if !before.Updated.IsZero() {
		after = ` AND (updated, id) < (?, ?)`
		args = append(args, before.Updated.UnixMilli(), before.ID)
	}
	args = append(args, append([]any{userID}, args[1:]...)...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
		SELECT `+gameCols+` FROM games WHERE `+playedBySeat0+after+`
		UNION ALL SELECT `+gameCols+` FROM games WHERE `+playedBySeat1+after+`
		) ORDER BY updated DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Game
	for rows.Next() {
		g, _, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQLite) Records(ctx context.Context, userID int64) ([]Record, error) {
	// In SQLite, the bare column nick comes from the row with max(updated).
	rows, err := s.db.QueryContext(ctx, `WITH mine AS (
		SELECT seat1_user AS uid, seat1_bot AS bot, seat1_nick AS nick, updated,
			CASE winner WHEN 0 THEN 1 WHEN 1 THEN -1 ELSE 0 END AS res
		FROM games WHERE `+playedBySeat0+`
		UNION ALL
		SELECT seat0_user, seat0_bot, seat0_nick, updated,
			CASE winner WHEN 1 THEN 1 WHEN 0 THEN -1 ELSE 0 END
		FROM games WHERE `+playedBySeat1+`
	)
	SELECT uid, bot, nick, max(updated), count(*), sum(res = 1), sum(res = -1), sum(res = 0)
	FROM mine GROUP BY uid, bot ORDER BY count(*) DESC, max(updated) DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var last int64
		if err := rows.Scan(&r.Opponent.UserID, &r.Opponent.Bot, &r.Opponent.Nickname, &last,
			&r.Games, &r.Wins, &r.Losses, &r.Unfinished); err != nil {
			return nil, err
		}
		r.Last = time.UnixMilli(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadGame returns any game, finished or not, with its actions.
func (s *SQLite) LoadGame(ctx context.Context, id string) (*GameWithActions, error) {
	g, packed, err := scanGame(s.db.QueryRowContext(ctx, `SELECT `+gameCols+` FROM games WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := &GameWithActions{Game: g}
	if g.Status == StatusFinished {
		out.Actions, err = unpackActions(packed)
		return out, err
	}
	s.lmu.Lock()
	if lg := s.live[id]; lg != nil {
		out.Actions = append([]Action(nil), lg.acts...)
	}
	s.lmu.Unlock()
	return out, nil
}

func (s *SQLite) CreateInvite(ctx context.Context, inv *Invite) error {
	return s.write(ctx, func(tx *writeTx) error {
		_, err := tx.Exec(`INSERT INTO invites (id, from_user, from_nick, to_user, to_nick, code, created, expires)
			VALUES (?,?,?,?,?,?,?,?)`, inv.ID, inv.From, inv.FromName, inv.To, inv.ToName, inv.Code,
			inv.Created.UnixMilli(), inv.Expires.UnixMilli())
		return err
	}, nil)
}

func (s *SQLite) DeleteInvite(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *writeTx) error {
		_, err := tx.Exec(`DELETE FROM invites WHERE id = ?`, id)
		return err
	}, nil)
}

func (s *SQLite) LoadInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, from_user, from_nick, to_user, to_nick, code, created, expires FROM invites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var created, expires int64
		if err := rows.Scan(&inv.ID, &inv.From, &inv.FromName, &inv.To, &inv.ToName, &inv.Code, &created, &expires); err != nil {
			return nil, err
		}
		inv.Created, inv.Expires = time.UnixMilli(created), time.UnixMilli(expires)
		out = append(out, inv)
	}
	return out, rows.Err()
}
