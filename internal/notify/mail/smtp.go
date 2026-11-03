// Package mail sends transactional e-mail over SMTP.
//
// It exists because verification codes are unusable without a delivery channel,
// and because the same seam already backs fund/ticket notifications (the
// Mailer interface in internal/control/fund). One implementation, one place
// where credentials and header injection are handled.
package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables. Credentials are env-only, never t_config_item: every
// process that loads the config snapshot could read them otherwise.
const (
	EnvHost     = "SMTP_HOST"
	EnvPort     = "SMTP_PORT"
	EnvUsername = "SMTP_USERNAME"
	EnvPassword = "SMTP_PASSWORD"
	EnvFrom     = "SMTP_FROM"
	EnvTLS      = "SMTP_TLS" // "starttls" (587, default) or "implicit" (465)
)

// SMTP is a Mailer. It is safe for concurrent use: every Send dials its own
// connection, so a slow message cannot block the next one.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// Implicit selects TLS-on-connect (port 465). The zero value uses
	// STARTTLS, which is what port 587 expects.
	Implicit bool
	Timeout  time.Duration
}

// ErrNotConfigured means the deployment has no SMTP settings. Callers treat it
// as "no mail channel": verification codes must then fail closed rather than
// pretend a message was sent.
var ErrNotConfigured = errors.New("mail: SMTP is not configured")

// FromEnv builds an SMTP sender from the environment. An empty SMTP_HOST
// returns ErrNotConfigured; a partial configuration is an error, because a
// half-configured mailer would silently fail at send time instead of at boot.
func FromEnv() (*SMTP, error) {
	host := strings.TrimSpace(os.Getenv(EnvHost))
	if host == "" {
		return nil, ErrNotConfigured
	}
	port := 587
	if raw := strings.TrimSpace(os.Getenv(EnvPort)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("mail: %s must be a TCP port, got %q", EnvPort, raw)
		}
		port = n
	}
	from := strings.TrimSpace(os.Getenv(EnvFrom))
	if from == "" {
		return nil, fmt.Errorf("mail: %s is required (the envelope sender)", EnvFrom)
	}
	if strings.ContainsAny(from, "\r\n") {
		return nil, fmt.Errorf("mail: %s must not contain CR/LF", EnvFrom)
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvTLS)))
	implicit := mode == "implicit"
	if mode != "" && mode != "implicit" && mode != "starttls" {
		return nil, fmt.Errorf("mail: %s must be starttls or implicit, got %q", EnvTLS, mode)
	}
	return &SMTP{
		Host:     host,
		Port:     port,
		Username: strings.TrimSpace(os.Getenv(EnvUsername)),
		Password: os.Getenv(EnvPassword),
		From:     from,
		Implicit: implicit,
		Timeout:  10 * time.Second,
	}, nil
}

// Send delivers one plain-text UTF-8 message. Headers are built here rather
// than passed through: to and subject come from user input (a registration
// address), and a CR/LF in either would inject headers.
func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	if s == nil || s.Host == "" {
		return ErrNotConfigured
	}
	if err := checkHeaderValue("recipient", to); err != nil {
		return err
	}
	if err := checkHeaderValue("subject", subject); err != nil {
		return err
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	raw := buildMessage(s.From, to, subject, body)

	// The dial has its own budget; smtp.Client has no context-aware API, so the
	// deadline is carried by the connection itself.
	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if s.Implicit {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck // the message is already sent or failed

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("mail: set deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("mail: smtp handshake: %w", err)
	}
	defer client.Close() //nolint:errcheck // best effort after a completed transaction

	if !s.Implicit {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("mail: starttls: %w", err)
			}
		} else if s.Username != "" {
			// Credentials over a cleartext connection is not a tradeoff this
			// code makes silently.
			return fmt.Errorf("mail: server does not offer STARTTLS; refusing to send credentials in the clear")
		}
	}
	if s.Username != "" {
		auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := client.Mail(s.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close body: %w", err)
	}
	return client.Quit()
}

func checkHeaderValue(what, v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("mail: %s must not contain CR/LF", what)
	}
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("mail: %s is empty", what)
	}
	return nil
}

// buildMessage produces a UTF-8 base64 body with encoded subject. RFC 2047 is
// used for the subject because Chinese subjects are the normal case here and a
// raw UTF-8 subject is rejected or mangled by some relays.
func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: =?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?=\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	// 76-char lines are the MIME limit; long single lines are rejected by some
	// relays and trigger spam scoring on others.
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}
