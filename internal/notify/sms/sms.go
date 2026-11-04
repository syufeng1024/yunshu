// Package sms delivers verification codes over a text-message provider.
//
// It is the SMS half of the verification-code delivery seam. The e-mail half
// (internal/notify/mail) multiplexes one protocol across several relays; SMS is
// the opposite shape — several protocols, each with its own credentials — so
// there is deliberately no named-channel machinery here. AUTH_SMS_MODE selects
// the driver, one file plus one case adds a vendor.
//
// The seam is a plain-text body, not a code string: it mirrors
//
//	auth.Sender (to, subject, body) minus the subject
//
// so nothing on the caller's side has to know which vendor is live. The Aliyun
// driver lifts the six-digit code back out of that body to fill the template
// variable that was registered with the supplier (see extractCode).
package sms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	obslog "yunshu.dev/core/internal/obs/log"
)

// Delivery modes for AUTH_SMS_MODE.
//
//	off       the handset channel is unavailable: the send-code endpoint answers
//	          400 with a hint that points at e-mail instead. Default.
//	log       the code is printed to the process log instead of being sent.
//	          Development and CI only. Billing is per message, so this mode must
//	          never be reachable in production.
//	provider  a real vendor call. Missing credentials refuse to start the
//	          process rather than failing at the first user.
const (
	ModeOff      = "off"
	ModeLog      = "log"
	ModeProvider = "provider"
)

// Environment variables. Credentials are env-only and never a t_config_item:
// every process that loads the configuration snapshot could read them.
const (
	// EnvMode is AUTH_SMS_MODE. It is deliberately separate from
	// AUTH_VERIFICATION_MODE: e-mail and SMS go through different suppliers and
	// cannot possibly be switched on the same day.
	EnvMode = "AUTH_SMS_MODE"

	// Aliyun Dysmsapi credentials. The names carry the vendor prefix on
	// purpose: a second supplier gets its own prefix, not a shared variable.
	EnvAliyunAccessKeyID     = "ALIYUN_SMS_ACCESS_KEY_ID"
	EnvAliyunAccessKeySecret = "ALIYUN_SMS_ACCESS_KEY_SECRET"
	EnvAliyunSignName        = "ALIYUN_SMS_SIGN_NAME"
	EnvAliyunTemplateCode    = "ALIYUN_SMS_TEMPLATE_CODE"
	EnvAliyunEndpoint        = "ALIYUN_SMS_ENDPOINT"
)

// requiredProviderEnv lists the variables a provider deployment must fill in.
// The endpoint is not in the list: it has a working default and exists so a
// test stub can be pointed at.
var requiredProviderEnv = []string{
	EnvAliyunAccessKeyID,
	EnvAliyunAccessKeySecret,
	EnvAliyunSignName,
	EnvAliyunTemplateCode,
}

var (
	// ErrNotAvailable means the channel is switched off. It is never returned on
	// a user-visible path — the send-code endpoint refuses earlier — but it stays
	// as the last gate for a caller that forgot to check the mode.
	ErrNotAvailable = errors.New("sms: channel is not enabled")
	// ErrNotConfigured means provider mode was selected without a usable driver.
	ErrNotConfigured = errors.New("sms: provider is not configured")
	// ErrNoCode means the body carried no six-digit code, so the vendor template
	// variable cannot be filled. Failing is the only honest answer: sending an
	// empty template would report success for a message nobody can use.
	ErrNoCode = errors.New("sms: message body carries no verification code")
)

// Vendor is one supplier's send seam. phone is the normalized handset number
// (E.164 digits, no leading '+') and code is the six-digit verification code.
type Vendor interface {
	// Name is the driver name used in the startup line. It is never a secret.
	Name() string
	SendCode(ctx context.Context, phone, code string) error
}

// Client is the process-wide SMS sender. It is safe for concurrent use: the
// vendor implementation dials its own connection per message.
type Client struct {
	// Mode is one of ModeOff, ModeLog, ModeProvider.
	Mode string
	// Provider is the vendor name in provider mode ("aliyun"), empty otherwise.
	Provider string

	// Logger receives the log-mode body. Nil is allowed and means the code is
	// dropped silently, which is why FromEnv takes the daemon's logger and
	// cmd/controld always passes it.
	Logger *obslog.Logger

	// vendor is the live driver; nil unless Mode is ModeProvider.
	vendor Vendor
}

// FromEnv builds the client from the process environment.
//
// Every problem is a boot-time error rather than a send-time surprise, exactly
// like mail.ChannelsFromEnv: an unknown mode, or provider mode with a missing
// credential, means the operator has not said how text messages should be sent,
// and finding out at the first user registration is far too late.
//
// No value of any credential appears in an error or a log line: only the names
// of the variables that are missing.
func FromEnv(logger *obslog.Logger) (*Client, error) {
	return fromEnv(logger, os.Getenv)
}

// fromEnv is FromEnv with the environment injected, so tests can express
// "absent" for one variable while the rest are set.
func fromEnv(logger *obslog.Logger, getenv func(string) string) (*Client, error) {
	raw := strings.ToLower(strings.TrimSpace(getenv(EnvMode)))
	mode := raw
	if mode == "" {
		mode = ModeOff
	}

	switch mode {
	case ModeOff, ModeLog:
		// Neither mode reads a credential: a log-mode deployment must not
		// accidentally validate (or, worse, log) half-written provider keys.
		return &Client{Mode: mode, Logger: logger}, nil
	case ModeProvider:
	default:
		return nil, fmt.Errorf("sms: %s must be off, log or provider, got %q", EnvMode, raw)
	}

	missing := make([]string, 0, len(requiredProviderEnv))
	for _, name := range requiredProviderEnv {
		if strings.TrimSpace(getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// The names, never the values. A partially filled credential set is a
		// configuration mistake the operator has to see at boot.
		return nil, fmt.Errorf("sms: %s=%s requires %s; missing: %s",
			EnvMode, ModeProvider, strings.Join(requiredProviderEnv, ", "),
			strings.Join(missing, ", "))
	}

	provider := &AliyunProvider{
		AccessKeyID:     strings.TrimSpace(getenv(EnvAliyunAccessKeyID)),
		AccessKeySecret: strings.TrimSpace(getenv(EnvAliyunAccessKeySecret)),
		SignName:        strings.TrimSpace(getenv(EnvAliyunSignName)),
		TemplateCode:    strings.TrimSpace(getenv(EnvAliyunTemplateCode)),
		Endpoint:        strings.TrimSpace(getenv(EnvAliyunEndpoint)),
	}
	return &Client{Mode: ModeProvider, Provider: provider.Name(), Logger: logger, vendor: provider}, nil
}

// Send delivers one plain-text message. It satisfies auth.SMSSender.
//
// off returns ErrNotAvailable and does not silently succeed: a caller that
// reached this point with the channel switched off has a bug, and pretending
// the message went out would hide it behind "the user never got the code".
func (c *Client) Send(ctx context.Context, to, body string) error {
	if c == nil {
		return ErrNotAvailable
	}
	switch c.Mode {
	case ModeLog:
		// The code is the message text here. It is printed as the body of one
		// log line — not as a structured field — so it cannot ride into a sink
		// that ships off-box by accident. The number is masked: an operator
		// needs to tell two requests apart, not to read a customer's handset.
		msg := fmt.Sprintf("sms_code_log_mode to=%s body=%s", maskPhone(to), body)
		if c.Logger != nil {
			c.Logger.Warn(ctx, msg, obslog.Fields{Stage: "auth.sms"})
		}
		return nil
	case ModeProvider:
		if c.vendor == nil {
			return ErrNotConfigured
		}
		code, err := extractCode(body)
		if err != nil {
			return err
		}
		return c.vendor.SendCode(ctx, to, code)
	default:
		return ErrNotAvailable
	}
}

// String renders the client without any credential. It exists so a %v of the
// client inside a log line or a wrapped error cannot leak a secret.
func (c *Client) String() string {
	if c == nil {
		return "sms(nil)"
	}
	if c.Provider != "" {
		return fmt.Sprintf("sms{mode=%s provider=%s}", c.Mode, c.Provider)
	}
	return fmt.Sprintf("sms{mode=%s}", c.Mode)
}

// GoString is what %#v calls. Without it, %#v would print the struct verbatim
// and walk into the vendor's credential fields.
func (c *Client) GoString() string { return c.String() }

// codeRE is the only shape a verification code has (six digits, see
// auth.verificationCodeDigits). Matching a run rather than the whole body keeps
// the seam free of any wording: the caller may change the message text without
// breaking delivery.
var codeRE = regexp.MustCompile(`[0-9]{6}`)

// extractCode pulls the verification code out of the message body.
func extractCode(body string) (string, error) {
	code := codeRE.FindString(body)
	if code == "" {
		return "", ErrNoCode
	}
	return code, nil
}

// maskPhone keeps the first three and the last two digits (138****1234). It is
// the same reduction auth.MaskIdentifier applies to a handset, duplicated here
// rather than imported: internal/notify must not depend on the control plane.
func maskPhone(phone string) string {
	if len(phone) <= 4 {
		return "***"
	}
	return phone[:3] + "****" + phone[len(phone)-2:]
}
