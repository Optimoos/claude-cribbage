package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupTo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenSQLite(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, &User{Username: "alice", Nickname: "A", PasswordHash: []byte("h"), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}

	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	// Older backups, and a file that is not one.
	for _, n := range []string{"cribbage-20200101-000000.db", "cribbage-20210101-000000.db", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(backups, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path, err := s.BackupTo(ctx, backups, 2)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	entries, _ := os.ReadDir(backups)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"cribbage-20210101-000000.db", filepath.Base(path), "notes.txt"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("backups dir has %v, want %v", names, want)
	}

	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v, %v; want 0600", fi.Mode(), err)
	}

	// The copy is a working database with the data in it.
	b, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if u, err := b.UserByUsername(ctx, "alice"); err != nil || u.Nickname != "A" {
		t.Fatalf("user in backup: %v, %v", u, err)
	}

	// The live database still takes writes.
	if err := s.CreateUser(ctx, &User{Username: "bob", Nickname: "B", PasswordHash: []byte("h"), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestLastBackup(t *testing.T) {
	dir := t.TempDir()
	if got := LastBackup(dir); !got.IsZero() {
		t.Fatalf("empty dir: %v", got)
	}
	if got := LastBackup(filepath.Join(dir, "missing")); !got.IsZero() {
		t.Fatalf("missing dir: %v", got)
	}
	for _, n := range []string{"cribbage-20240101-000000.db", "cribbage-20250607-080910.db", "cribbage-20260101-000000.db.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := LastBackup(dir), time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("LastBackup = %v, want %v", got, want)
	}
}
