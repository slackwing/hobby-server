// Package mailer sends HTML email over SMTP. Server-wide (one sender
// for every project); configured by the top-level `email:` block in
// config.yaml. Zero-valued = disabled: Send returns ErrNotConfigured
// and callers surface that as "email not configured".
//
// Works with any STARTTLS submission endpoint (Gmail app passwords on
// smtp.gmail.com:587, Fastmail, SES SMTP, ...). net/smtp refuses to
// send PLAIN credentials over a non-TLS connection to a non-localhost
// host, so a misconfigured server fails closed rather than leaking the
// password. The password never appears in logs (AGENTS.md N4).
package mailer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

var ErrNotConfigured = errors.New("email not configured")

type Mailer struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // "Name <addr>" or bare addr
}

func (m Mailer) Configured() bool {
	return m.Host != "" && m.From != ""
}

// Send delivers one HTML message. subject and html are already
// rendered; to is a bare address or "Name <addr>".
func (m Mailer) Send(to, subject, html string) error {
	if !m.Configured() {
		return ErrNotConfigured
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("bad from address: %w", err)
	}
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("bad recipient address: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", rcpt.String())
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", randomID(), domainOf(from.Address))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(html)); err != nil {
		return err
	}
	if err := qp.Close(); err != nil {
		return err
	}

	port := m.Port
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	addr := fmt.Sprintf("%s:%d", m.Host, port)
	if err := smtp.SendMail(addr, auth, from.Address, []string{rcpt.Address}, []byte(b.String())); err != nil {
		// Defensive: an SMTP error string should never carry the
		// password, but scrub anyway before it can reach a log line.
		msg := err.Error()
		if m.Password != "" {
			msg = strings.ReplaceAll(msg, m.Password, "<password>")
		}
		return errors.New(msg)
	}
	return nil
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}
