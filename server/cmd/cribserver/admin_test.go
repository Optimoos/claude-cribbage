package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/auth"
	"cribbage/internal/server"
	"cribbage/internal/store"
)

func TestAdminSocket(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &auth.Service{Store: store.NewMemory(), BcryptCost: bcrypt.MinCost}
	if _, err := a.Register(context.Background(), "alice", "password123", ""); err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{Auth: a, Logger: log})
	defer srv.Shutdown()
	path := filepath.Join(t.TempDir(), "admin.sock")
	// A stale socket from an earlier run is replaced.
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	ln, err := serveAdmin(log, path, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, %v", fi.Mode(), err)
	}

	call := func(req adminRequest) adminResponse {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		json.NewEncoder(c).Encode(req)
		var resp adminResponse
		if err := json.NewDecoder(c).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := call(adminRequest{Cmd: "reset-password", Username: "nobody"}); r.OK || r.Error == "" {
		t.Fatalf("unknown user: %+v", r)
	}
	if r := call(adminRequest{Cmd: "frobnicate"}); r.OK {
		t.Fatalf("unknown command: %+v", r)
	}
	r := call(adminRequest{Cmd: "reset-password", Username: "alice"})
	if !r.OK || r.Password == "" {
		t.Fatalf("reset: %+v", r)
	}
	res, err := a.Login(context.Background(), "alice", r.Password)
	if err != nil || !res.User.MustChange {
		t.Fatalf("login with temporary password: %+v, %v", res, err)
	}
}
