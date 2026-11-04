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

	"cribbage/internal/auth"
	"cribbage/internal/server"
	"cribbage/internal/store"
)

func TestAdminSocket(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &auth.Service{Store: store.NewMemory(), BotKey: "admin-test-bot-key"}
	if _, err := a.BotLogin(context.Background(), "alice", "", a.BotKey); err != nil {
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
	if r := call(adminRequest{Cmd: "set-email", Username: "nobody", Email: "a@example.com"}); r.OK || r.Error == "" {
		t.Fatalf("unknown user: %+v", r)
	}
	if r := call(adminRequest{Cmd: "set-email", Username: "alice", Email: "not-an-address"}); r.OK {
		t.Fatalf("bad address: %+v", r)
	}
	if r := call(adminRequest{Cmd: "frobnicate"}); r.OK {
		t.Fatalf("unknown command: %+v", r)
	}
	if r := call(adminRequest{Cmd: "set-email", Username: "alice", Email: "alice@example.com"}); !r.OK || r.OldEmail != "" {
		t.Fatalf("set email: %+v", r)
	}
	u, err := a.Store.UserByUsername(context.Background(), "alice")
	if err != nil || u.Email != "alice@example.com" || u.Bot {
		t.Fatalf("after set-email: %+v, %v", u, err)
	}
	if r := call(adminRequest{Cmd: "delete-account", Username: "alice"}); !r.OK {
		t.Fatalf("delete: %+v", r)
	}
}
