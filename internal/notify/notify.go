// Package notify sends Maison's outbound mail.
//
// It exists for one reason: what makes a backup worthless is silent failure, and an
// unattended nightly job with no channel out is exactly that.
//
// Composing and sending are deliberately separate. Composing is pure and tested;
// sending dials a server and is stubbed at the caller's seam. That split is what
// lets the interesting logic — when to notify, and what to say — be tested without
// an SMTP server anywhere.
package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP is the transport configuration.
type SMTP struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
	From string `json:"from"`
	To   string `json:"to"`

	// Security is "starttls" (the default), "tls" for implicit TLS, or "none".
	// "none" is meaningful because a PCS ships with a relay on its own Docker
	// network, where there is no network to protect the hop from.
	Security string `json:"security,omitempty"`
}

// Configured reports whether there is enough here to send anything.
func (s SMTP) Configured() bool { return s.Host != "" && s.From != "" && s.To != "" }

func (s SMTP) addr() string {
	port := s.Port
	if port == 0 {
		port = 587
	}
	return net.JoinHostPort(s.Host, fmt.Sprint(port))
}

// Compose builds an RFC 5322 message.
//
// Deliberately minimal and deliberately pure: headers, a plain-text body, CRLF line
// endings. Anything that needs a MIME library is out of scope for "tell the operator
// the backup failed".
func Compose(from, to, subject, body string, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", sanitizeHeader(from))
	fmt.Fprintf(&b, "To: %s\r\n", sanitizeHeader(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	// Normalise to CRLF: a bare LF in the body is a protocol violation that some
	// servers reject and others silently mangle.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// sanitizeHeader strips CR and LF, which are how a header value becomes an injected
// header. Subjects here carry app names, and app names come from the store.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// Send delivers one message.
func Send(cfg SMTP, subject, body string) error {
	if !cfg.Configured() {
		return fmt.Errorf("notify: no SMTP server configured")
	}
	msg := Compose(cfg.From, cfg.To, subject, body, time.Now())

	switch strings.ToLower(cfg.Security) {
	case "tls":
		conn, err := tls.Dial("tcp", cfg.addr(), &tls.Config{ServerName: cfg.Host})
		if err != nil {
			return fmt.Errorf("notify: connect: %w", err)
		}
		defer conn.Close()
		c, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return fmt.Errorf("notify: handshake: %w", err)
		}
		return deliver(c, cfg, msg)
	default:
		c, err := smtp.Dial(cfg.addr())
		if err != nil {
			return fmt.Errorf("notify: connect: %w", err)
		}
		defer c.Close()
		if !strings.EqualFold(cfg.Security, "none") {
			// Opportunistic: a relay that does not offer STARTTLS is not a reason to
			// refuse to warn someone their backups are failing.
			if ok, _ := c.Extension("STARTTLS"); ok {
				if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
					return fmt.Errorf("notify: starttls: %w", err)
				}
			}
		}
		return deliver(c, cfg, msg)
	}
}

func deliver(c *smtp.Client, cfg SMTP, msg []byte) error {
	defer func() { _ = c.Quit() }()
	if cfg.User != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)); err != nil {
				return fmt.Errorf("notify: auth: %w", err)
			}
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("notify: from: %w", err)
	}
	if err := c.Rcpt(cfg.To); err != nil {
		return fmt.Errorf("notify: to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("notify: write: %w", err)
	}
	return w.Close()
}
