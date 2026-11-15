// Package mail sends plain-text email: sign-in codes and links, and
// account links (address changes, deletion).
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Message is one plain-text email.
type Message struct {
	To, Subject, Body string
	// Unsubscribe, if set, is an https URL that stops email to this
	// address when POSTed to, for mail apps' one-click "Unsubscribe"
	// (RFC 8058).
	Unsubscribe string
}

// Sender sends one message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Mailbox is the inbox an address delivers to, for counting emails and
// remembering addresses that asked for none: lower case, without a
// "+tag" (foo+bar@example.com reaches foo@example.com at most providers),
// and, at Gmail, without dots, which Gmail ignores. Two addresses with the
// same Mailbox may still be different inboxes at a provider that treats
// "+" as part of the name; erring that way only shares their limits.
func Mailbox(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at < 1 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if i := strings.IndexByte(local, '+'); i > 0 {
		local = local[:i]
	}
	if domain == "gmail.com" || domain == "googlemail.com" {
		domain = "gmail.com"
		if l := strings.ReplaceAll(local, ".", ""); l != "" {
			local = l
		}
	}
	return local + "@" + domain
}

// ValidAddress reports whether s is a bare email address ("a@b.example",
// with no display name) of a sensible length.
func ValidAddress(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, "\r\n<>\"") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s && strings.Contains(s[strings.LastIndexByte(s, '@')+1:], ".")
}

// SMTP sends through a mail server. Port 465 uses TLS from the start;
// other ports must offer STARTTLS, except on a loopback host. The password
// is sent with AUTH PLAIN, only over TLS.
type SMTP struct {
	Addr     string // host:port
	Username string // empty sends without logging in
	Password string
	From     string // the sender's address, e.g. cribbage@example.com
	FromName string // optional display name
	Timeout  time.Duration
}

// Check reports a configuration that cannot work.
func (m *SMTP) Check() error {
	if _, _, err := net.SplitHostPort(m.Addr); err != nil {
		return fmt.Errorf("smtp address %q: %w", m.Addr, err)
	}
	if !ValidAddress(m.From) {
		return fmt.Errorf("sender address %q is not an email address", m.From)
	}
	if strings.ContainsAny(m.FromName, "\r\n") {
		return errors.New("sender name contains a line break")
	}
	return nil
}

func (m *SMTP) Send(ctx context.Context, msg Message) error {
	to := msg.To
	if !ValidAddress(to) {
		return fmt.Errorf("bad recipient %q", to)
	}
	data, err := m.message(msg)
	if err != nil {
		return err
	}
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, port, _ := net.SplitHostPort(m.Addr)
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	d := net.Dialer{}
	var conn net.Conn
	if port == "465" {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", m.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", m.Addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if !isLoopback(host) {
			return fmt.Errorf("%s does not offer STARTTLS", m.Addr)
		}
	}
	if m.Username != "" {
		// smtp.PlainAuth refuses to send the password without TLS, except
		// to localhost.
		if err := c.Auth(smtp.PlainAuth("", m.Username, m.Password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// message builds the message with headers, quoted-printable encoded.
func (m *SMTP) message(msg Message) ([]byte, error) {
	to, subject, body := msg.To, msg.Subject, msg.Body
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("subject contains a line break")
	}
	if strings.ContainsAny(msg.Unsubscribe, "\r\n<>") || (msg.Unsubscribe != "" && !strings.HasPrefix(msg.Unsubscribe, "https://")) {
		return nil, fmt.Errorf("bad unsubscribe URL %q", msg.Unsubscribe)
	}
	from := (&mail.Address{Name: m.FromName, Address: m.From}).String()
	id := make([]byte, 16)
	rand.Read(id)
	var b bytes.Buffer
	for _, h := range [][2]string{
		{"From", from},
		{"To", to},
		{"Subject", mime.QEncoding.Encode("utf-8", subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", "<" + hex.EncodeToString(id) + "@" + m.From[strings.LastIndexByte(m.From, '@')+1:] + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "quoted-printable"},
		{"Auto-Submitted", "auto-generated"},
	} {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	if msg.Unsubscribe != "" {
		fmt.Fprintf(&b, "List-Unsubscribe: <%s>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n", msg.Unsubscribe)
	}
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	return b.Bytes(), nil
}

// Log writes messages to a logger instead of sending them, for testing.
// The codes and links in them sign people in: do not use it on a public
// server.
type Log struct {
	Logger *slog.Logger
}

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("mail (not sent: no -smtp)", "to", m.To, "subject", m.Subject, "body", m.Body)
	return nil
}
