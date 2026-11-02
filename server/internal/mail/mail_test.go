package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

func TestValidAddress(t *testing.T) {
	for s, want := range map[string]bool{
		"a@example.com":           true,
		"first.last+x@sub.ex.org": true,
		"":                        false,
		"nobody":                  false,
		"a@localhost":             false,
		"A <a@example.com>":       false,
		"a@example.com\r\nBcc: x": false,
		"<a@example.com>":         false,
		strings.Repeat("a", 250) + "@example.com": false,
	} {
		if got := ValidAddress(s); got != want {
			t.Errorf("ValidAddress(%q) = %v", s, got)
		}
	}
}

// fakeSMTP accepts one message on loopback, without TLS, and returns what
// it received.
func fakeSMTP(t *testing.T) (string, chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var log strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- log.String()
				return
			}
			log.WriteString(line)
			switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); cmd {
			case "EHLO", "HELO":
				say("250 fake")
			case "DATA":
				say("354 go on")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					log.WriteString(l)
					if l == ".\r\n" {
						break
					}
				}
				say("250 queued")
			case "QUIT":
				say("221 bye")
				got <- log.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSMTPSend(t *testing.T) {
	addr, got := fakeSMTP(t)
	m := &SMTP{Addr: addr, From: "crib@example.com", FromName: "Cribbage"}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	link := "https://crib.example/#reset=" + strings.Repeat("ab", 32)
	body := "Hello Zoë,\n\nOpen this link:\n\n" + link + "\n"
	if err := m.Send(context.Background(), "to@example.com", "Reset your password", body); err != nil {
		t.Fatal(err)
	}
	s := <-got
	for _, want := range []string{
		"MAIL FROM:<crib@example.com>", "RCPT TO:<to@example.com>",
		"From: \"Cribbage\" <crib@example.com>\r\n", "To: to@example.com\r\n", "Subject: Reset your password\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n", "Zo=C3=AB",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("message lacks %q:\n%s", want, s)
		}
	}
	// The link survives quoted-printable soft line breaks once decoded.
	if strings.Contains(s, link) {
		return
	}
	if !strings.Contains(strings.ReplaceAll(s, "=\r\n", ""), strings.ReplaceAll(link, "=", "=3D")) {
		t.Errorf("link mangled:\n%s", s)
	}
}

func TestSMTPRefusesBadInput(t *testing.T) {
	m := &SMTP{Addr: "127.0.0.1:1", From: "crib@example.com"}
	if err := m.Send(context.Background(), "x@example.com\r\nBcc: y@example.com", "s", "b"); err == nil {
		t.Error("sent to a header injection")
	}
	if _, err := m.message("x@example.com", "line\r\nBcc: y@example.com", "b"); err == nil {
		t.Error("subject with a line break accepted")
	}
	if err := (&SMTP{Addr: "nohost", From: "crib@example.com"}).Check(); err == nil {
		t.Error("address without a port accepted")
	}
}
