package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type backupReq struct {
	path string
	err  chan error
}

// Backup writes a consistent, compacted copy of the database to path, which
// must not exist, using VACUUM INTO. It runs on the writer goroutine between
// batches, so writes wait while it runs (well under a second for a database
// of a few tens of MB). The copy is an ordinary SQLite file: restore it by
// stopping the server and putting it in place of the database (removing
// any -wal file).
func (s *SQLite) Backup(ctx context.Context, path string) error {
	req := backupReq{path: path, err: make(chan error, 1)}
	select {
	case s.backups <- req:
	case <-s.done:
		return errors.New("store closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-req.err
}

const backupPrefix, backupSuffix = "cribbage-", ".db"

// BackupTo writes a backup named after the current UTC time into dir, then
// deletes all but the newest keep backups there. It returns the new file.
func (s *SQLite) BackupTo(ctx context.Context, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := backupPrefix + time.Now().UTC().Format("20060102-150405") + backupSuffix
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"
	os.Remove(tmp) // left by a crash mid-backup
	if err := s.Backup(ctx, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("backup: %w", err)
	}
	// It holds password hashes and session tokens.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return final, err
	}
	var old []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, backupPrefix) && strings.HasSuffix(n, backupSuffix) {
			old = append(old, n)
		}
	}
	slices.Sort(old) // the names sort by time
	for len(old) > max(keep, 1) {
		if err := os.Remove(filepath.Join(dir, old[0])); err != nil {
			return final, err
		}
		old = old[1:]
	}
	return final, nil
}
