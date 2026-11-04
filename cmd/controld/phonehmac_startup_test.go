package main

// QA addition: the startup diagnostic must never print key material.
//
// The two refusal paths (too short / reused) are the ones an operator is most
// likely to paste into a ticket, and they are also the ones where the key value
// is sitting right next to the message. A logger call that formatted the value
// into the line would leak a PII-equivalent secret into the log pipeline.

import (
	"context"
	"strings"
	"testing"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/platform/env"
)

// qaSecret is deliberately distinctive: it is long enough to be a real key and
// unique enough that finding it in the log is unambiguous.
const qaSecret = "qa-distinctive-phone-hmac-value-0123456789"

func TestQAKeyDiagnosticsNeverPrintTheValue(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		cfg   *env.Config
		envNm string
	}{
		{
			name:  "reused as FP_HMAC_KEY",
			env:   map[string]string{config.EnvPhoneHMACKey: qaSecret, "FP_HMAC_KEY": qaSecret},
			cfg:   &env.Config{Env: "dev"},
			envNm: "dev",
		},
		{
			name: "reused as CHANNEL_KEY_KEK (prod)",
			env: map[string]string{config.EnvPhoneHMACKey: qaSecret,
				cryptobox.EnvKEK: qaSecret},
			cfg:   &env.Config{Env: "prod"},
			envNm: "prod",
		},
		{
			name:  "reused as SESSION_SECRET",
			env:   map[string]string{config.EnvPhoneHMACKey: qaSecret},
			cfg:   &env.Config{Env: "dev", SessionSecret: qaSecret},
			envNm: "dev",
		},
		{
			name:  "too short",
			env:   map[string]string{config.EnvPhoneHMACKey: "qa-short-val"},
			cfg:   &env.Config{Env: "dev"},
			envNm: "dev",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FP_HMAC_KEY", "")
			t.Setenv(cryptobox.EnvKEK, "")
			t.Setenv(config.EnvLogBodyKEK, "")
			t.Setenv("EXPORT_HMAC_KEY", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			logger, buf := captureLogger(t, c.envNm)
			if got := phoneHMACKey(logger, c.cfg, context.Background()); got != "" {
				t.Fatalf("key = %q, want empty (fail closed)", got)
			}
			if msg := buf.String(); msg == "" {
				t.Fatal("nothing was logged; the operator would not learn about it")
			} else {
				for _, leak := range []string{qaSecret, c.env[config.EnvPhoneHMACKey]} {
					if leak != "" && strings.Contains(msg, leak) {
						t.Fatalf("the startup log contains the key value %q: %s", leak, msg)
					}
				}
			}
		})
	}
}

// A 32-byte key must be accepted (the boundary is >= 32, not > 32) and a
// 31-byte one refused, so an operator generating with `openssl rand -base64 32`
// or a 32-character hex string does not hit an off-by-one.
func TestQAKeyLengthBoundary(t *testing.T) {
	t.Setenv("FP_HMAC_KEY", "")
	t.Setenv(cryptobox.EnvKEK, "")
	t.Setenv(config.EnvLogBodyKEK, "")
	t.Setenv("EXPORT_HMAC_KEY", "")

	exactly32 := strings.Repeat("k", 32)
	t.Setenv(config.EnvPhoneHMACKey, exactly32)
	logger, _ := captureLogger(t, "dev")
	if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != exactly32 {
		t.Fatalf("a 32-byte key was rejected: got %q", got)
	}

	t.Setenv(config.EnvPhoneHMACKey, strings.Repeat("k", 31))
	logger, _ = captureLogger(t, "dev")
	if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != "" {
		t.Fatalf("a 31-byte key was accepted: got %q", got)
	}
}
