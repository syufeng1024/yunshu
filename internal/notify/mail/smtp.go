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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment variables. Credentials are env-only, never t_config_item: every
// process that loads the config snapshot could read them otherwise.
//
// Channels: the unprefixed names below are the DEFAULT channel. A deployment
// may additionally declare any number of named channels by inserting a name
// before the suffix — SMTP_QQ_HOST, SMTP_163_PASSWORD, ... — and pick the one
// this process sends with through AUTH_SMTP_CHANNEL. That way a second relay
// stays configured instead of being overwritten every time the operator
// switches, and switching is one line rather than six.
const (
	// PrefixDefault is the variable prefix of the DEFAULT channel. Named
	// channels extend it: QQ -> SMTP_QQ_*.
	PrefixDefault = "SMTP"
	// ChannelDefault is the channel built from the unprefixed SMTP_* names, so
	// a deployment that configured one relay before channels existed keeps
	// working without touching its environment.
	ChannelDefault = "DEFAULT"

	EnvHost     = PrefixDefault + "_HOST"
	EnvPort     = PrefixDefault + "_PORT"
	EnvUsername = PrefixDefault + "_USERNAME"
	EnvPassword = PrefixDefault + "_PASSWORD"
	EnvFrom     = PrefixDefault + "_FROM"
	EnvTLS      = PrefixDefault + "_TLS" // "starttls" (587, default) or "implicit" (465)

	// EnvChannel names the channel this process sends with. The value is a
	// channel name and is matched case-insensitively.
	EnvChannel = "AUTH_SMTP_CHANNEL"

	defaultPort    = 587
	defaultTimeout = 10 * time.Second
)

// channelFields lists the per-channel variable suffixes. The order doubles as
// the match order when a variable name is split, so the longer words come
// after the ones they could be confused with (never a real ambiguity: the
// suffix always follows an underscore).
var channelFields = []string{"HOST", "PORT", "USERNAME", "PASSWORD", "FROM", "TLS"}

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

// Channels is the set of SMTP channels declared in the environment together
// with the one this process will send with.
type Channels struct {
	// Configured lists the canonical names of the fully specified channels,
	// sorted, so the startup line is stable across runs.
	Configured []string
	// Selected is the canonical name of the channel in use, or "" when the
	// environment declares no channel at all.
	Selected string
	// Sender is the selected channel, or nil when nothing is configured.
	Sender *SMTP

	// byName holds every parsed channel so Get can hand out the ones this
	// process is not sending with (a health check may want to probe all of
	// them).
	byName map[string]*SMTP
}

// Get returns the channel with the given canonical name, matched
// case-insensitively. ok is false when that channel is not configured.
func (c *Channels) Get(name string) (*SMTP, bool) {
	if c == nil {
		return nil, false
	}
	sender, ok := c.byName[strings.ToUpper(strings.TrimSpace(name))]
	return sender, ok
}

// Summary renders the startup log line: which relays exist and which one is
// live. Names and hosts are not secrets; no credential ever reaches this
// string.
func (c *Channels) Summary() string {
	selected := c.Selected
	if selected == "" {
		selected = "(none)"
	}
	return fmt.Sprintf("smtp channels: configured=[%s] selected=%s",
		strings.Join(c.Configured, " "), selected)
}

// String renders the channel without its password. It exists so that a %v of a
// channel inside a log line or a wrapped error cannot leak the credential.
func (s *SMTP) String() string {
	if s == nil {
		return "smtp(nil)"
	}
	mode := "starttls"
	if s.Implicit {
		mode = "implicit"
	}
	auth := "none"
	if s.Username != "" {
		auth = "plain"
	}
	return fmt.Sprintf("smtp{host=%s port=%d from=%s tls=%s auth=%s}", s.Host, s.Port, s.From, mode, auth)
}

// GoString is what %#v calls. Without it, %#v would print the struct verbatim
// and include the password, because GoStringer wins over Stringer for the sharp
// verb.
func (s *SMTP) GoString() string {
	return s.String()
}

// FromEnv builds the DEFAULT channel from the unprefixed SMTP_* variables.
//
// It predates named channels and is kept so existing callers keep working; new
// code should use ChannelsFromEnv so AUTH_SMTP_CHANNEL is honoured.
//
// The two entry points differ on purpose, and only for a half-written set of
// variables:
//
//   - Empty SMTP_HOST reports ErrNotConfigured even when FROM or a credential is
//     filled in. That has always been this function's contract (the host is read
//     first and nothing else is looked at), and it keeps legacy callers that
//     probe "is there a mailer at all?" from having to handle a hard error.
//   - ChannelsFromEnv treats the same input as a configuration error: a channel
//     that has been given a sender or a credential but no host is a mistake the
//     operator should see at boot, not at the first send.
//
// Neither treats SMTP_PORT / SMTP_TLS alone as evidence of a channel: the
// shipped .env.stack.example carries 587 / starttls as examples.
func FromEnv() (*SMTP, error) {
	if strings.TrimSpace(os.Getenv(EnvHost)) == "" {
		return nil, ErrNotConfigured
	}
	return parseChannel(PrefixDefault, os.Getenv)
}

// ChannelsFromEnv reads every SMTP channel from the process environment and
// resolves AUTH_SMTP_CHANNEL against them.
//
// Everything here is a boot-time fact rather than a send-time surprise: an
// invalid name, a half-filled channel, or several channels with no selection
// all mean the operator has not said which relay to use. Guessing would hide
// that until the first registration e-mail went out through the wrong account.
func ChannelsFromEnv() (*Channels, error) {
	return channelsFrom(os.Environ, os.Getenv)
}

// channelsFrom is ChannelsFromEnv with the environment injected. environ is
// scanned for variable names (so an unknown number of channels can be found)
// while getenv resolves the selection; tests supply both from a map, because
// t.Setenv cannot express "absent" together with two spellings of one name.
func channelsFrom(environ func() []string, getenv func(string) string) (*Channels, error) {
	decls, err := declarations(environ)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)

	byName := make(map[string]*SMTP, len(decls))
	configured := make([]string, 0, len(decls))
	for _, name := range names {
		decl := decls[name]
		prefix := PrefixDefault
		if name != ChannelDefault {
			prefix = PrefixDefault + "_" + name
		}
		// The values were already canonicalised, so the parser reads them
		// through a prefix-stripping lookup instead of the raw environment.
		fieldGetenv := func(key string) string {
			field, ok := strings.CutPrefix(key, prefix+"_")
			if !ok {
				return ""
			}
			return decl.values[field]
		}
		sender, err := parseChannel(prefix, fieldGetenv)
		if err != nil {
			if errors.Is(err, ErrNotConfigured) {
				// Every variable of this channel is empty: a copied template
				// declares the name without meaning it. Not an error.
				continue
			}
			return nil, err
		}
		byName[name] = sender
		configured = append(configured, name)
	}

	out := &Channels{Configured: configured, byName: byName}
	selection := strings.TrimSpace(getenv(EnvChannel))
	if len(configured) == 0 {
		if selection != "" {
			return nil, fmt.Errorf("mail: %s=%q is set but no SMTP channel is configured; declare it with %s_%s_HOST and friends",
				EnvChannel, selection, PrefixDefault, strings.ToUpper(selection))
		}
		return out, nil
	}

	selectedName, selectedSender, err := selectChannel(byName, selection)
	if err != nil {
		return nil, err
	}
	out.Selected = selectedName
	out.Sender = selectedSender
	return out, nil
}

// selectChannel applies AUTH_SMTP_CHANNEL to the configured set.
//
// An empty selection is only accepted when there is exactly one channel: with
// two relays configured, silently picking one turns a copy-paste slip into a
// mystery that costs an afternoon of log reading, so the error names the
// candidates instead.
func selectChannel(byName map[string]*SMTP, selection string) (string, *SMTP, error) {
	available := make([]string, 0, len(byName))
	for name := range byName {
		available = append(available, name)
	}
	sort.Strings(available)

	wanted := strings.ToUpper(strings.TrimSpace(selection))
	if wanted == "" {
		if len(available) == 1 {
			return available[0], byName[available[0]], nil
		}
		return "", nil, fmt.Errorf("mail: %d SMTP channels are configured (%s) but %s is not set; set it to one of them",
			len(available), strings.Join(available, ", "), EnvChannel)
	}
	if sender, ok := byName[wanted]; ok {
		return wanted, sender, nil
	}
	return "", nil, fmt.Errorf("mail: %s=%q does not match a configured SMTP channel; available: %s",
		EnvChannel, selection, strings.Join(available, ", "))
}

// channelDecl accumulates the variables that describe one channel: the
// spellings its prefix appeared in (so two case variants can be rejected) and
// its canonical fields.
type channelDecl struct {
	name   string
	raws   map[string]struct{}
	values map[string]string
}

// declarations groups every SMTP_<NAME>_<FIELD> variable by channel. It is the
// only place that knows the naming scheme; parseChannel just takes a prefix.
func declarations(environ func() []string) (map[string]*channelDecl, error) {
	out := make(map[string]*channelDecl, 2)
	for _, kv := range environ() {
		varName, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		channel, field, ok := splitChannelVar(strings.ToUpper(varName))
		if !ok {
			// Not one of ours: SMTP_<something> that is not a channel field.
			continue
		}
		if !validChannelName(channel) {
			return nil, fmt.Errorf("mail: %s carries an invalid channel name %q; allowed characters are A-Z, 0-9 and _",
				varName, channel)
		}
		decl := out[channel]
		if decl == nil {
			decl = &channelDecl{
				name:   channel,
				raws:   make(map[string]struct{}, 1),
				values: make(map[string]string, len(channelFields)),
			}
			out[channel] = decl
		}
		// SMTP_qq_HOST and SMTP_QQ_HOST describe the same field; letting both
		// stand would make the winner depend on environ() ordering.
		if _, dup := decl.values[field]; dup {
			return nil, fmt.Errorf("mail: %s is declared twice for channel %s; channel and field names are case-insensitive",
				field, channel)
		}
		decl.values[field] = value
		decl.raws[rawPrefix(varName, field)] = struct{}{}
	}
	for _, decl := range out {
		if len(decl.raws) > 1 {
			return nil, fmt.Errorf("mail: SMTP channel %s is spelled %d ways (%s); channel names are case-insensitive, so this is one channel written twice",
				decl.name, len(decl.raws), strings.Join(sortedKeys(decl.raws), ", "))
		}
	}
	return out, nil
}

// splitChannelVar maps a canonical variable name to its channel and field.
//
//	SMTP_HOST      -> ("DEFAULT", "HOST")
//	SMTP_QQ_HOST   -> ("QQ", "HOST")
//
// upper must already be upper-cased. ok is false for names that do not end in
// a known field, which is how unrelated SMTP_* variables are ignored.
func splitChannelVar(upper string) (channel, field string, ok bool) {
	rest, found := strings.CutPrefix(upper, PrefixDefault+"_")
	if !found {
		return "", "", false
	}
	for _, f := range channelFields {
		if rest == f {
			return ChannelDefault, f, true
		}
		if name, hit := strings.CutSuffix(rest, "_"+f); hit {
			return name, f, true
		}
	}
	return "", "", false
}

// validChannelName reports whether name is usable as a channel name. The
// alphabet is deliberately narrow: a name is re-embedded into variable names,
// so "-" or a space would make the prefix ambiguous.
func validChannelName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// rawPrefix returns the variable prefix as it was actually written, e.g.
// "SMTP_qq" for "SMTP_qq_HOST". The spelling has to survive upper-casing,
// otherwise two case variants of one channel would look identical.
func rawPrefix(varName, field string) string {
	if len(varName) <= len(field)+1 {
		return varName
	}
	return varName[:len(varName)-len(field)-1]
}

// sortedKeys returns the keys of set in sorted order, for stable messages.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parsePort reads a channel's _PORT variable. Empty means defaultPort; a value
// is validated even when the channel turns out to be undeclared, so a typo
// does not sit in the environment until the day someone fills in a host.
func parsePort(varName, raw string) (int, error) {
	if raw == "" {
		return defaultPort, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("mail: %s must be a TCP port, got %q", varName, raw)
	}
	return n, nil
}

// parseTLS reads a channel's _TLS variable: "starttls" (default) or "implicit"
// (TLS on connect, which is what port 465 expects). A typo is an error even for
// a channel that turns out to be undeclared, for the same reason as parsePort.
func parseTLS(varName, raw string) (bool, error) {
	switch mode := strings.ToLower(raw); mode {
	case "", "starttls":
		return false, nil
	case "implicit":
		return true, nil
	default:
		return false, fmt.Errorf("mail: %s must be starttls or implicit, got %q", varName, mode)
	}
}

// parseChannel builds one channel from the variables behind prefix ("SMTP" for
// DEFAULT, "SMTP_QQ" for channel QQ). getenv is injected so the parser can be
// exercised without touching the process environment.
//
// ErrNotConfigured means the channel was never declared: no host, no envelope
// sender and no credential. _PORT and _TLS deliberately do not count — the
// shipped .env.stack.example carries SMTP_PORT=587 and SMTP_TLS=starttls as
// examples and leaves the rest empty, and copying that template must read as
// "no mail here" rather than as a broken channel. They are still validated, so
// a typo there is never carried silently.
//
// Once anything meaningful is filled in, the channel must be complete: a
// half-declared channel has to fail at boot instead of at the first send.
func parseChannel(prefix string, getenv func(string) string) (*SMTP, error) {
	env := func(field string) string { return getenv(prefix + "_" + field) }
	portVar, tlsVar, fromVar, hostVar := prefix+"_PORT", prefix+"_TLS", prefix+"_FROM", prefix+"_HOST"

	host := strings.TrimSpace(env("HOST"))
	rawPort := strings.TrimSpace(env("PORT"))
	rawTLS := strings.TrimSpace(env("TLS"))
	username := strings.TrimSpace(env("USERNAME"))
	// The password is not trimmed: leading or trailing spaces can be part of a
	// relay password, and silently altering it would look like a wrong
	// credential at send time.
	password := env("PASSWORD")
	from := strings.TrimSpace(env("FROM"))

	port, err := parsePort(portVar, rawPort)
	if err != nil {
		return nil, err
	}
	implicit, err := parseTLS(tlsVar, rawTLS)
	if err != nil {
		return nil, err
	}

	if host == "" && from == "" && username == "" && password == "" {
		return nil, ErrNotConfigured
	}
	if host == "" {
		return nil, fmt.Errorf("mail: %s is required (the relay host)", hostVar)
	}
	if from == "" {
		return nil, fmt.Errorf("mail: %s is required (the envelope sender)", fromVar)
	}
	if strings.ContainsAny(from, "\r\n") {
		return nil, fmt.Errorf("mail: %s must not contain CR/LF", fromVar)
	}
	return &SMTP{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     from,
		Implicit: implicit,
		Timeout:  defaultTimeout,
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
		timeout = defaultTimeout
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
