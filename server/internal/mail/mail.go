// Package mail sends plain-text email: password reset and address
// verification links.
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

// Sender sends one message.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
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

func (m *SMTP) Send(ctx context.Context, to, subject, body string) error {
	if !ValidAddress(to) {
		return fmt.Errorf("bad recipient %q", to)
	}
	msg, err := m.message(to, subject, body)
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
	if _, err := w.Write(msg); err != nil {
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
func (m *SMTP) message(to, subject, body string) ([]byte, error) {
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("subject contains a line break")
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
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	return b.Bytes(), nil
}

// Log writes messages to a logger instead of sending them, for testing.
// The links in them are secrets: do not use it on a public server.
type Log struct {
	Logger *slog.Logger
}

func (l Log) Send(_ context.Context, to, subject, body string) error {
	l.Logger.Info("mail (not sent: -mail-log)", "to", to, "subject", subject, "body", body)
	return nil
}
