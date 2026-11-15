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
	stop := "https://api.crib.example/api/stop?t=" + strings.Repeat("cd", 48)
	if err := m.Send(context.Background(), Message{To: "to@example.com", Subject: "Reset your password", Body: body, Unsubscribe: stop}); err != nil {
		t.Fatal(err)
	}
	s := <-got
	for _, want := range []string{
		"MAIL FROM:<crib@example.com>", "RCPT TO:<to@example.com>",
		"From: \"Cribbage\" <crib@example.com>\r\n", "To: to@example.com\r\n", "Subject: Reset your password\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n", "Zo=C3=AB",
		"List-Unsubscribe: <" + stop + ">\r\n", "List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n",
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
	if err := m.Send(context.Background(), Message{To: "x@example.com\r\nBcc: y@example.com", Subject: "s", Body: "b"}); err == nil {
		t.Error("sent to a header injection")
	}
	if _, err := m.message(Message{To: "x@example.com", Subject: "line\r\nBcc: y@example.com", Body: "b"}); err == nil {
		t.Error("subject with a line break accepted")
	}
	for _, u := range []string{"http://crib.example/stop", "https://crib.example/>\r\nBcc: y@example.com"} {
		if _, err := m.message(Message{To: "x@example.com", Subject: "s", Body: "b", Unsubscribe: u}); err == nil {
			t.Errorf("unsubscribe URL %q accepted", u)
		}
	}
	if err := (&SMTP{Addr: "nohost", From: "crib@example.com"}).Check(); err == nil {
		t.Error("address without a port accepted")
	}
}

func TestMailbox(t *testing.T) {
	for _, c := range [][2]string{
		{"Foo@Example.com", "foo@example.com"},
		{"foo+cribbage@example.com", "foo@example.com"},
		{"foo+a+b@example.com", "foo@example.com"},
		{"+foo@example.com", "+foo@example.com"}, // no name before the tag: leave it
		{"f.o.o+x@gmail.com", "foo@gmail.com"},
		{"F.oo@GoogleMail.com", "foo@gmail.com"},
		{"f.oo@example.com", "f.oo@example.com"}, // dots matter elsewhere
		{" foo@example.com ", "foo@example.com"},
	} {
		if got := Mailbox(c[0]); got != c[1] {
			t.Errorf("Mailbox(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
