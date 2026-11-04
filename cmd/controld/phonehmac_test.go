package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/cryptobox"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/env"
)

// startupKey is a valid PHONE_HMAC_KEY: 32+ bytes, unlike every other secret.
const startupKey = "controld-phone-hmac-key-0123456789"

// captureLogger returns a logger writing into the returned buffer, so a test can
// assert the *severity* of the startup message — the whole point of the
// prod-ERROR / dev-WARN split is that an operator notices on day one.
func captureLogger(t *testing.T, envName string) (*obslog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return obslog.NewWithWriter(env.ServiceControl, envName, buf, clock.Real{}), buf
}

// The happy path: a valid key is returned verbatim and nothing is logged.
func TestPhoneHMACKeyAcceptsValidKey(t *testing.T) {
	t.Setenv(config.EnvPhoneHMACKey, startupKey)
	logger, buf := captureLogger(t, "dev")
	if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != startupKey {
		t.Fatalf("key = %q, want the configured value", got)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Fatalf("a valid key logged %q; nothing should be printed", got)
	}
}

// The YUNSHU_ prefix is the fallback the other HMAC keys accept; keeping it
// means an operator who prefixed everything keeps working.
func TestPhoneHMACKeyAcceptsYunshuPrefix(t *testing.T) {
	t.Setenv("YUNSHU_"+config.EnvPhoneHMACKey, startupKey)
	logger, _ := captureLogger(t, "dev")
	if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != startupKey {
		t.Fatalf("key = %q, want the YUNSHU_-prefixed value", got)
	}
}

// Absent or too short: the process still starts, the value is "", and the
// message is an ERROR in production and a WARN everywhere else. This is the
// "startup" half of the missing-key behaviour; the 503 half lives in
// internal/control/auth (registration) and internal/control/ops (operator
// provisioning), and the "untouched" half in the same tests.
func TestPhoneHMACKeyMissingOrShort(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"absent", ""},
		{"blank", "   "},
		{"too short", strings.Repeat("k", 31)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvPhoneHMACKey, c.val)
			t.Setenv("YUNSHU_"+config.EnvPhoneHMACKey, "")

			devLogger, devBuf := captureLogger(t, "dev")
			if got := phoneHMACKey(devLogger, &env.Config{Env: "dev"}, context.Background()); got != "" {
				t.Fatalf("key = %q, want empty (fail closed)", got)
			}
			if !strings.Contains(devBuf.String(), config.EnvPhoneHMACKey) {
				t.Fatalf("dev log does not name the variable: %q", devBuf.String())
			}
			if !strings.Contains(devBuf.String(), `"level":"warn"`) {
				t.Fatalf("non-prod severity = %q, want warn", devBuf.String())
			}

			prodLogger, prodBuf := captureLogger(t, "prod")
			if got := phoneHMACKey(prodLogger, &env.Config{Env: "prod"}, context.Background()); got != "" {
				t.Fatalf("prod key = %q, want empty (fail closed)", got)
			}
			if !strings.Contains(prodBuf.String(), `"level":"error"`) {
				t.Fatalf("prod severity = %q, want error", prodBuf.String())
			}
		})
	}
}

// Reusing another process secret is treated as "not configured": one leaked
// value must not weaken two boundaries, and this is the key that reverses to
// PII, so it is the strictest of the set.
func TestPhoneHMACKeyRejectsReusedSecret(t *testing.T) {
	t.Setenv(config.EnvPhoneHMACKey, startupKey)
	t.Setenv("FP_HMAC_KEY", startupKey)
	logger, buf := captureLogger(t, "dev")
	if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != "" {
		t.Fatalf("key = %q, want empty when it equals FP_HMAC_KEY", got)
	}
	if !strings.Contains(buf.String(), "must not reuse") {
		t.Fatalf("log does not explain the reuse refusal: %q", buf.String())
	}

	// Same for the KEKs and the export key.
	for _, other := range []string{cryptobox.EnvKEK, config.EnvLogBodyKEK, "EXPORT_HMAC_KEY"} {
		t.Run(other, func(t *testing.T) {
			t.Setenv("FP_HMAC_KEY", "")
			t.Setenv(other, startupKey)
			t.Setenv(config.EnvPhoneHMACKey, startupKey)
			logger, _ := captureLogger(t, "dev")
			if got := phoneHMACKey(logger, &env.Config{Env: "dev"}, context.Background()); got != "" {
				t.Fatalf("key = %q, want empty when it equals %s", got, other)
			}
		})
	}
}

// A session secret equal to the phone key is the same failure, and the config
// struct (not the environment) is where it arrives from.
func TestPhoneHMACKeyRejectsSessionSecretReuse(t *testing.T) {
	t.Setenv(config.EnvPhoneHMACKey, startupKey)
	logger, _ := captureLogger(t, "dev")
	cfg := &env.Config{Env: "dev", SessionSecret: startupKey}
	if got := phoneHMACKey(logger, cfg, context.Background()); got != "" {
		t.Fatalf("key = %q, want empty when it equals SESSION_SECRET", got)
	}
}
