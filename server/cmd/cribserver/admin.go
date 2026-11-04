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
	"strings"
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
	Email    string `json:"email,omitempty"`
}

type adminResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// OldEmail is the address set-email replaced.
	OldEmail string `json:"old_email,omitempty"`
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
		case "set-email":
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			resp.OldEmail, err = srv.AdminSetEmail(ctx, req.Username, req.Email)
			cancel()
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.OK = true
			}
		case "delete-account":
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			err = srv.AdminDeleteAccount(ctx, req.Username)
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
	yes := fl.Bool("yes", false, "delete-account: do not ask to confirm")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), `Usage: cribserver admin [-socket path] <command>

Commands:
  set-email <username> <address>
        Recover an account whose owner cannot sign in (they lost their
        email, or someone else changed it): sign-in codes go to the new
        address, every device is signed out, and the account's passkeys
        are removed. The old address is told. Check that the person
        asking is the account's owner first.
  delete-account [-yes] <username>
        Delete a player's account: their unfinished games end (active ones
        are resigned), their invites and account are removed, and their
        username is free again. Finished games stay in their opponents'
        history as "Deleted player". Asks you to type the username again
        unless -yes is given.

Run it as the server's user, e.g. sudo -u cribbage cribserver admin ...

Flags:
`)
		fl.PrintDefaults()
	}
	fl.Parse(args)
	var req adminRequest
	switch fl.Arg(0) {
	case "set-email":
		if fl.NArg() != 3 {
			fl.Usage()
			os.Exit(2)
		}
		req = adminRequest{Cmd: "set-email", Username: fl.Arg(1), Email: fl.Arg(2)}
	case "delete-account":
		if fl.NArg() != 2 {
			fl.Usage()
			os.Exit(2)
		}
		req = adminRequest{Cmd: "delete-account", Username: fl.Arg(1)}
		if !*yes {
			fmt.Printf("This permanently deletes the account %q. Type the username again to confirm: ", req.Username)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(line) != req.Username {
				fmt.Fprintln(os.Stderr, "Not deleted.")
				os.Exit(1)
			}
		}
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
	if req.Cmd == "delete-account" {
		fmt.Printf("Deleted the account %s.\n", req.Username)
		return
	}
	fmt.Printf("Sign-in codes for %s now go to %s.\n", req.Username, req.Email)
	fmt.Println("They are signed out everywhere, and their passkeys were removed; they can sign in with a code and add passkeys again.")
	if resp.OldEmail != "" && !strings.EqualFold(resp.OldEmail, req.Email) {
		fmt.Printf("The old address (%s) was told of the change.\n", resp.OldEmail)
	}
}
