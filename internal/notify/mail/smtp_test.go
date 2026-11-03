package mail

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestBuildMessageEncodesHeaderAndBody pins the wire format: Chinese subjects
// must be RFC 2047 encoded (a raw UTF-8 subject is mangled or rejected by some
// relays) and the body must be base64 so a verification code survives the
// transport byte-for-byte.
func TestBuildMessageEncodesHeaderAndBody(t *testing.T) {
	raw := buildMessage("noreply@yunshu.test", "user@example.com", "云枢验证码", "你的验证码是 123456")

	if !strings.Contains(raw, "Subject: =?UTF-8?B?") {
		t.Fatalf("subject is not RFC 2047 encoded:\n%s", raw)
	}
	if strings.Contains(raw, "云枢验证码") {
		t.Fatalf("raw UTF-8 subject leaked into the message header:\n%s", raw)
	}

	header, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body separator:\n%s", raw)
	}
	if !strings.Contains(header, "Content-Transfer-Encoding: base64") {
		t.Fatalf("body is not declared base64:\n%s", header)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(body), "\r\n", ""))
	if err != nil {
		t.Fatalf("body is not valid base64: %v", err)
	}
	if !strings.Contains(string(decoded), "123456") {
		t.Fatalf("decoded body %q lost the code", decoded)
	}
}

// TestHeaderInjectionIsRejected: the recipient comes from a registration form,
// so a CR/LF in it would otherwise let a caller add headers (Bcc, or a second
// body).
func TestHeaderInjectionIsRejected(t *testing.T) {
	s := &SMTP{Host: "smtp.invalid", Port: 587, From: "noreply@yunshu.test"}
	if err := s.Send(context.Background(), "victim@example.com\r\nBcc: attacker@example.com", "s", "b"); err == nil {
		t.Fatalf("CRLF in recipient accepted")
	}
	if err := s.Send(context.Background(), "victim@example.com", "subject\nBcc: x", "b"); err == nil {
		t.Fatalf("CRLF in subject accepted")
	}
}

// TestNotConfiguredIsRecognisable: callers must be able to tell "no SMTP here"
// from "SMTP failed", because only the first is a configuration fact.
func TestNotConfiguredIsRecognisable(t *testing.T) {
	s := &SMTP{}
	if err := s.Send(context.Background(), "a@b.c", "s", "b"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty host returned %v, want ErrNotConfigured", err)
	}

	t.Setenv(EnvHost, "")
	if _, err := FromEnv(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("FromEnv without a host returned %v, want ErrNotConfigured", err)
	}
}

// TestFromEnvValidates: a half-configured mailer must fail at boot, not at the
// first registration.
func TestFromEnvValidates(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"minimal", map[string]string{EnvHost: "smtp.example.com", EnvFrom: "noreply@example.com"}, true},
		{"missing from", map[string]string{EnvHost: "smtp.example.com"}, false},
		{"bad port", map[string]string{EnvHost: "smtp.example.com", EnvFrom: "n@e.com", EnvPort: "abc"}, false},
		{"bad tls", map[string]string{EnvHost: "smtp.example.com", EnvFrom: "n@e.com", EnvTLS: "sometimes"}, false},
		{"from has crlf", map[string]string{EnvHost: "smtp.example.com", EnvFrom: "n@e.com\nBcc: x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvHost, EnvPort, EnvUsername, EnvPassword, EnvFrom, EnvTLS} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected an error, got %+v", got)
			}
		})
	}
}
