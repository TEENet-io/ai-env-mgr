package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP sends one mail per message through a relay. Port 465 is taken as
// TLS from the first byte; any other port starts plain and upgrades with
// STARTTLS when the relay offers it. Credentials are sent only over TLS.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	Timeout  time.Duration
	// InsecureNoTLS allows credentials over a plain connection: for a relay
	// on localhost. Never for anything across a network.
	InsecureNoTLS bool
}

func (s SMTP) Name() string { return "smtp" }

// Send delivers one mail. The context bounds the whole exchange.
func (s SMTP) Send(ctx context.Context, m Message) error {
	if s.Host == "" || s.From == "" || len(s.To) == 0 {
		return errors.New("smtp: host, sender and at least one recipient are required")
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if port == 465 {
		conn, err = tls.DialWithDialer(&dialer, "tcp", addr, &tls.Config{ServerName: s.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(timeout))
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	secure := port == 465
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
				return fmt.Errorf("smtp: starttls: %w", err)
			}
			secure = true
		}
	}
	if s.Username != "" {
		if !secure && !s.InsecureNoTLS {
			return errors.New("smtp: the relay offers no TLS; refusing to send the password in the clear")
		}
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp: from: %w", err)
	}
	for _, to := range s.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("smtp: rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write([]byte(s.mail(m))); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return c.Quit()
}

// mail renders the RFC 5322 message: UTF-8 body, subject encoded so a
// Chinese title survives a relay that only speaks ASCII headers.
func (s SMTP) mail(m Message) string {
	subject := fmt.Sprintf("[windows-control] %s %s", strings.ToUpper(m.Severity), m.Title)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(s.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Text(), "\n", "\r\n"))
	return b.String()
}
