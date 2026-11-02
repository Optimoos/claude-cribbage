package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"time"

	"cribbage/internal/server"
)

// The admin socket takes one JSON request per connection and answers with
// one JSON response. Only users who can open the socket file can use it:
// the server makes it mode 0600, so that is its own user (and root).

const defaultAdminSocket = "/run/cribbage/admin.sock"

type adminRequest struct {
	Cmd      string `json:"cmd"`
	Username string `json:"username,omitempty"`
}

type adminResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Password string `json:"password,omitempty"`
}

// serveAdmin listens on a Unix socket at path for admin commands.
func serveAdmin(log *slog.Logger, path string, srv *server.Server) (net.Listener, error) {
	// A socket left by a server that did not shut down cleanly.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Error("admin socket", "err", err)
				}
				return
			}
			go handleAdmin(log, c, srv)
		}
	}()
	return ln, nil
}

func handleAdmin(log *slog.Logger, c net.Conn, srv *server.Server) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	var req adminRequest
	var resp adminResponse
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		switch req.Cmd {
		case "reset-password":
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			resp.Password, err = srv.AdminResetPassword(ctx, req.Username)
			cancel()
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.OK = true
			}
		default:
			resp.Error = fmt.Sprintf("unknown command %q", req.Cmd)
		}
	}
	if resp.Error != "" {
		log.Warn("admin command failed", "cmd", req.Cmd, "err", resp.Error)
	}
	json.NewEncoder(c).Encode(resp)
}

// adminMain runs "cribserver admin ...", which sends one command to a
// running server.
func adminMain(args []string) {
	fl := flag.NewFlagSet("admin", flag.ExitOnError)
	socket := fl.String("socket", defaultAdminSocket, "the server's -admin-socket")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), `Usage: cribserver admin [-socket path] <command>

Commands:
  reset-password <username>
        Give a player a temporary password and log them out everywhere.
        They must choose a new password when they next log in.

Run it as the server's user, e.g. sudo -u cribbage cribserver admin ...

Flags:
`)
		fl.PrintDefaults()
	}
	fl.Parse(args)
	var req adminRequest
	switch fl.Arg(0) {
	case "reset-password":
		if fl.NArg() != 2 {
			fl.Usage()
			os.Exit(2)
		}
		req = adminRequest{Cmd: "reset-password", Username: fl.Arg(1)}
	default:
		fl.Usage()
		os.Exit(2)
	}
	c, err := net.DialTimeout("unix", *socket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach the server at %s: %v\n(is it running with -admin-socket, and are you its user?)\n", *socket, err)
		os.Exit(1)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(40 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var resp adminResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		fmt.Fprintln(os.Stderr, "no answer from the server:", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	fmt.Printf("Temporary password for %s: %s\n", req.Username, resp.Password)
	fmt.Println("They are logged out everywhere, and must choose a new password when they log in with it.")
}
