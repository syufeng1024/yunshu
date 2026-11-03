package mail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeEnv turns a list of "KEY=VALUE" strings into the two functions
// channelsFrom needs. A map cannot express this fixture: the parser has to see
// the *names* that were set (that is how channels are discovered) and, for the
// case-variant tests, two spellings of what upper-cases to the same name.
func fakeEnv(vars ...string) (func() []string, func(string) string) {
	sorted := append([]string(nil), vars...)
	sort.Strings(sorted)
	environ := func() []string { return sorted }
	getenv := func(key string) string {
		for _, kv := range sorted {
			if k, v, ok := strings.Cut(kv, "="); ok && k == key {
				return v
			}
		}
		return ""
	}
	return environ, getenv
}

func channelsFromVars(t *testing.T, vars ...string) (*Channels, error) {
	t.Helper()
	environ, getenv := fakeEnv(vars...)
	return channelsFrom(environ, getenv)
}

// TestChannelsCoexist pins the whole point of the change: both relays stay in
// the configuration and AUTH_SMTP_CHANNEL picks one. The unprefixed names are
// the DEFAULT channel, so an existing deployment is unaffected.
func TestChannelsCoexist(t *testing.T) {
	channels, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_PORT=465",
		"SMTP_QQ_TLS=implicit",
		"SMTP_QQ_USERNAME=10001@qq.com",
		"SMTP_QQ_PASSWORD=qq-secret",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_163_HOST=smtp.163.com",
		"SMTP_163_USERNAME=syufeng1024@163.com",
		"SMTP_163_PASSWORD=163-secret",
		"SMTP_163_FROM=syufeng1024@163.com",
		"AUTH_SMTP_CHANNEL=163",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(channels.Configured, ","), "163,QQ"; got != want {
		t.Fatalf("configured channels = %q, want %q", got, want)
	}
	if channels.Selected != "163" {
		t.Fatalf("selected = %q, want 163", channels.Selected)
	}

	// 163 declares no PORT/TLS: the defaults must apply.
	got163, ok := channels.Get("163")
	if !ok {
		t.Fatalf("channel 163 missing")
	}
	if got163.Host != "smtp.163.com" || got163.Port != 587 || got163.Implicit {
		t.Fatalf("163 = %v, want host=smtp.163.com port=587 starttls", got163)
	}
	if got163.Password != "163-secret" || got163.From != "syufeng1024@163.com" {
		t.Fatalf("163 credentials/sender not carried over: %v", got163)
	}
	if channels.Sender != got163 {
		t.Fatalf("Sender is not the selected channel")
	}

	gotQQ, ok := channels.Get("qq") // case-insensitive on purpose
	if !ok {
		t.Fatalf("channel QQ missing")
	}
	if gotQQ.Host != "smtp.qq.com" || gotQQ.Port != 465 || !gotQQ.Implicit {
		t.Fatalf("QQ = %v, want host=smtp.qq.com port=465 implicit", gotQQ)
	}
	if gotQQ.Username != "10001@qq.com" || gotQQ.Password != "qq-secret" {
		t.Fatalf("QQ credentials not carried over: %v", gotQQ)
	}

	const wantSummary = "smtp channels: configured=[163 QQ] selected=163"
	if channels.Summary() != wantSummary {
		t.Fatalf("Summary() = %q, want %q", channels.Summary(), wantSummary)
	}
}

// TestChannelsUnprefixedIsDefault: SMTP_HOST/SMTP_PORT/... keep working exactly
// as before, under the name DEFAULT.
func TestChannelsUnprefixedIsDefault(t *testing.T) {
	channels, err := channelsFromVars(t,
		"SMTP_HOST=smtp.163.com",
		"SMTP_FROM=noreply@yunshu.test",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(channels.Configured) != 1 || channels.Configured[0] != ChannelDefault {
		t.Fatalf("configured = %v, want [DEFAULT]", channels.Configured)
	}
	if channels.Selected != ChannelDefault {
		t.Fatalf("selected = %q, want DEFAULT", channels.Selected)
	}
	if channels.Sender.Host != "smtp.163.com" || channels.Sender.Port != 587 || channels.Sender.Implicit {
		t.Fatalf("sender = %v, want host=smtp.163.com port=587 starttls", channels.Sender)
	}
}

// TestChannelsSelectionIsCaseInsensitive: the operator types a channel name,
// not an identifier, so "qq" must find channel QQ.
func TestChannelsSelectionIsCaseInsensitive(t *testing.T) {
	channels, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_163_HOST=smtp.163.com",
		"SMTP_163_FROM=syufeng1024@163.com",
		"AUTH_SMTP_CHANNEL=qq",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channels.Selected != "QQ" || channels.Sender.Host != "smtp.qq.com" {
		t.Fatalf("selected = %q host = %q, want QQ on smtp.qq.com", channels.Selected, channels.Sender.Host)
	}
}

// TestChannelsSingleChannelIsAutoSelected: a deployment with one relay should
// not have to name it.
func TestChannelsSingleChannelIsAutoSelected(t *testing.T) {
	channels, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channels.Selected != "QQ" || channels.Sender == nil {
		t.Fatalf("selected = %q, sender = %v; want QQ auto-selected", channels.Selected, channels.Sender)
	}
}

// TestChannelsAmbiguousWithoutSelection: two relays and no choice must fail at
// boot and name the candidates. Silently picking one is the failure mode this
// whole change is about.
func TestChannelsAmbiguousWithoutSelection(t *testing.T) {
	_, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_163_HOST=smtp.163.com",
		"SMTP_163_FROM=syufeng1024@163.com",
	)
	if err == nil {
		t.Fatalf("two channels without AUTH_SMTP_CHANNEL were accepted")
	}
	for _, want := range []string{EnvChannel, "163", "QQ"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestChannelsUnknownSelection: a typo in AUTH_SMTP_CHANNEL must list what is
// actually configured.
func TestChannelsUnknownSelection(t *testing.T) {
	_, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_163_HOST=smtp.163.com",
		"SMTP_163_FROM=syufeng1024@163.com",
		"AUTH_SMTP_CHANNEL=aliyun",
	)
	if err == nil {
		t.Fatalf("unknown channel name was accepted")
	}
	for _, want := range []string{"aliyun", "163", "QQ"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestChannelsSelectionWithoutChannels: naming a channel that does not exist
// because nothing is configured is a contradiction, not an empty mailer.
func TestChannelsSelectionWithoutChannels(t *testing.T) {
	_, err := channelsFromVars(t, "AUTH_SMTP_CHANNEL=qq")
	if err == nil {
		t.Fatalf("AUTH_SMTP_CHANNEL with no channel configured was accepted")
	}
	if !strings.Contains(err.Error(), EnvChannel) {
		t.Fatalf("error %q does not mention %s", err, EnvChannel)
	}
}

// TestChannelsEmptyTemplateIsNotConfigured: the shipped .env template sets
// every SMTP_* variable to a value or to empty. That must read as "no mail
// here", not as a broken channel — otherwise copying the template bricks the
// process. _PORT and _TLS carry example values in the template, so they must
// not count as declaring a channel.
func TestChannelsEmptyTemplateIsNotConfigured(t *testing.T) {
	cases := [][]string{
		{
			"SMTP_HOST=", "SMTP_PORT=", "SMTP_TLS=", "SMTP_USERNAME=", "SMTP_PASSWORD=", "SMTP_FROM=",
			"SMTP_QQ_HOST=", "SMTP_QQ_PORT=", "SMTP_QQ_FROM=",
		},
		{
			// The shape the template actually ships: style values filled in.
			"SMTP_HOST=", "SMTP_PORT=587", "SMTP_TLS=starttls", "SMTP_USERNAME=", "SMTP_PASSWORD=", "SMTP_FROM=",
			"SMTP_163_HOST=", "SMTP_163_PORT=", "SMTP_163_TLS=", "SMTP_163_FROM=",
		},
	}
	for _, vars := range cases {
		channels, err := channelsFromVars(t, vars...)
		if err != nil {
			t.Fatalf("empty template reported an error: %v", err)
		}
		if len(channels.Configured) != 0 || channels.Sender != nil || channels.Selected != "" {
			t.Fatalf("empty template produced channels: %+v", channels)
		}
		if want := "smtp channels: configured=[] selected=(none)"; channels.Summary() != want {
			t.Fatalf("Summary() = %q, want %q", channels.Summary(), want)
		}
	}
}

// TestDeployEnvTemplateYieldsNoChannels reads the template the deploy docs tell
// operators to copy and feeds it through the public entry point, so the two
// cannot drift apart again.
//
// This is a regression test with a real postmortem behind it: the template ships
// SMTP_PORT=587 / SMTP_TLS=starttls as examples, and those were once counted as
// "this channel is declared", so copying the template aborted controld's startup
// while the docs promised it was harmless. A hand-written fixture did not catch
// it because it set SMTP_PORT empty — only the real file does.
func TestDeployEnvTemplateYieldsNoChannels(t *testing.T) {
	// Neutralise whatever the developer's shell exports: this test is about the
	// template alone. An empty value reads as "not set" to the parser, so
	// t.Setenv is enough here.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if upper := strings.ToUpper(name); strings.HasPrefix(upper, PrefixDefault+"_") || upper == EnvChannel {
			t.Setenv(name, "")
		}
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", ".env.stack.example"))
	if err != nil {
		t.Fatalf("read deploy/.env.stack.example: %v", err)
	}
	applied := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, PrefixDefault+"_") && !strings.HasPrefix(line, EnvChannel+"=") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("template line is not KEY=VALUE: %q", line)
		}
		t.Setenv(name, value)
		applied++
	}
	if applied == 0 {
		t.Fatalf("no SMTP variables found in the template; the test is looking at the wrong file")
	}

	channels, err := ChannelsFromEnv()
	if err != nil {
		t.Fatalf("copying deploy/.env.stack.example must start cleanly, got: %v", err)
	}
	if len(channels.Configured) != 0 || channels.Sender != nil || channels.Selected != "" {
		t.Fatalf("the template must not declare a channel, got configured=%v selected=%q",
			channels.Configured, channels.Selected)
	}
}

// TestChannelsIncompleteFails: half a channel is a boot error, matching the
// pre-channel behaviour for a missing SMTP_FROM.
func TestChannelsIncompleteFails(t *testing.T) {
	cases := []struct {
		name  string
		vars  []string
		wants string
	}{
		{"no host", []string{"SMTP_QQ_FROM=10001@qq.com"}, "SMTP_QQ_HOST"},
		{"no from", []string{"SMTP_QQ_HOST=smtp.qq.com"}, "SMTP_QQ_FROM"},
		{"password only", []string{"SMTP_QQ_PASSWORD=secret"}, "SMTP_QQ_HOST"},
		{"username only", []string{"SMTP_QQ_USERNAME=10001@qq.com"}, "SMTP_QQ_HOST"},
		{"bad port", []string{"SMTP_QQ_HOST=smtp.qq.com", "SMTP_QQ_FROM=a@b.c", "SMTP_QQ_PORT=465a"}, "SMTP_QQ_PORT"},
		{"bad port alone", []string{"SMTP_QQ_PORT=465a"}, "SMTP_QQ_PORT"},
		{"bad tls", []string{"SMTP_QQ_HOST=smtp.qq.com", "SMTP_QQ_FROM=a@b.c", "SMTP_QQ_TLS=sometimes"}, "SMTP_QQ_TLS"},
		{"from crlf", []string{"SMTP_QQ_HOST=smtp.qq.com", "SMTP_QQ_FROM=a@b.c\nBcc: x"}, "SMTP_QQ_FROM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := channelsFromVars(t, tc.vars...)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error %q does not mention %q", err, tc.wants)
			}
		})
	}
}

// TestChannelsRejectInvalidName: a "-" in the name would make the prefix
// ambiguous, so it is rejected rather than silently ignored.
func TestChannelsRejectInvalidName(t *testing.T) {
	for _, vars := range [][]string{
		{"SMTP_1A-B_HOST=smtp.example.com", "SMTP_1A-B_FROM=n@e.com"},
		{"SMTP_QQ_HOST=smtp.example.com", "SMTP_QQ_FROM=n@e.com", "SMTP_1A-B_HOST=x"},
	} {
		_, err := channelsFromVars(t, vars...)
		if err == nil {
			t.Fatalf("invalid channel name accepted: %v", vars)
		}
		if !strings.Contains(err.Error(), "channel name") {
			t.Fatalf("error %q does not explain the channel name", err)
		}
	}
}

// TestChannelsRejectCaseVariantDuplicate: channel names are case-insensitive,
// so SMTP_QQ_* and SMTP_qq_* are one channel written twice. Which one wins
// would depend on environ() ordering — reject it.
func TestChannelsRejectCaseVariantDuplicate(t *testing.T) {
	for _, vars := range [][]string{
		{"SMTP_QQ_HOST=smtp.example.com", "SMTP_qq_HOST=smtp.other.com", "SMTP_QQ_FROM=n@e.com"},
		{"SMTP_QQ_HOST=smtp.example.com", "SMTP_QQ_FROM=n@e.com", "SMTP_qq_FROM=m@e.com"},
		{"SMTP_HOST=smtp.example.com", "SMTP_FROM=n@e.com", "SMTP_DEFAULT_HOST=smtp.other.com"},
	} {
		_, err := channelsFromVars(t, vars...)
		if err == nil {
			t.Fatalf("case-variant duplicate accepted: %v", vars)
		}
		if !strings.Contains(err.Error(), "QQ") && !strings.Contains(err.Error(), ChannelDefault) {
			t.Fatalf("error %q does not name the duplicated channel", err)
		}
	}
}

// TestChannelsNeverExposePassword: configuration errors and any rendering of a
// channel end up in logs and bug reports, so the password must not survive
// either path.
func TestChannelsNeverExposePassword(t *testing.T) {
	const secret = "sup3r-s3cret-pw"

	// An error raised while the password is configured.
	_, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_QQ_PASSWORD="+secret,
		"SMTP_QQ_PORT=notaport",
	)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the password: %q", err)
	}

	// And the rendered forms.
	channels, err := channelsFromVars(t,
		"SMTP_QQ_HOST=smtp.qq.com",
		"SMTP_QQ_FROM=10001@qq.com",
		"SMTP_QQ_USERNAME=10001@qq.com",
		"SMTP_QQ_PASSWORD="+secret,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, rendered := range []string{
		channels.Summary(),
		fmt.Sprintf("%v", channels.Sender),
		fmt.Sprintf("%+v", channels.Sender),
		fmt.Sprintf("%s", channels.Sender),
		fmt.Sprintf("%#v", channels.Sender), // GoStringer, not the struct dump
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("rendering leaked the password: %q", rendered)
		}
	}
}

// TestParseChannelDefaults pins the per-channel defaults: 587 with STARTTLS.
func TestParseChannelDefaults(t *testing.T) {
	_, getenv := fakeEnv(
		"SMTP_HOST=smtp.example.com",
		"SMTP_FROM=noreply@example.com",
	)

	sender, err := parseChannel(PrefixDefault, getenv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.Port != 587 || sender.Implicit || sender.Timeout <= 0 {
		t.Fatalf("defaults = %v, want port 587 / starttls / positive timeout", sender)
	}
}

// TestChannelsFromEnvReadsRealEnvironment covers the os.Environ path: channel
// names are discovered from variable names nobody listed for the reader, which
// is the part a fake environment cannot prove.
func TestChannelsFromEnvReadsRealEnvironment(t *testing.T) {
	t.Setenv("SMTP_QQ_HOST", "smtp.qq.com")
	t.Setenv("SMTP_QQ_FROM", "10001@qq.com")
	t.Setenv(EnvChannel, "qq")

	channels, err := ChannelsFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channels.Selected != "QQ" || channels.Sender == nil || channels.Sender.Host != "smtp.qq.com" {
		t.Fatalf("selected = %q sender = %v, want QQ on smtp.qq.com", channels.Selected, channels.Sender)
	}
}

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
// first registration. FromEnv only ever reads the DEFAULT channel.
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
