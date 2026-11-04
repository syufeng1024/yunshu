// Package auth implements the control-plane login/registration surface.
//
// The first segment deliberately covers password authentication only. Login
// lockout, verification codes, device fingerprints and trial-credit claiming
// arrive in the second segment; none of them is silently faked here.
package auth

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strconv"
	"strings"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// maxBodyBytes bounds every auth request body.
const maxBodyBytes = 64 * 1024

// Service is the shared dependency set for the auth handlers.
type Service struct {
	DB       *sql.DB
	Sessions *mw.SessionManager
	Audit    *audit.Writer
	Logger   *obslog.Logger
	Now      clock.Clock

	// Mailer delivers verification codes when VerificationMode is "smtp".
	// Nil is allowed; the send endpoint then answers 503 instead of pretending.
	Mailer Sender
	// VerificationMode is AUTH_VERIFICATION_MODE: "off", "log" or "smtp".
	// Empty means "log" — see verificationMode for why that is the default.
	VerificationMode string

	// SMSSender delivers verification codes to a handset. It is a separate
	// field, and a separate interface (declared in verifycode.go next to Sender),
	// because SMS has no subject and a different supplier.
	// Nil is allowed; the phone channel then answers 503 instead of pretending.
	SMSSender SMSSender
	// SMSMode is AUTH_SMS_MODE: "off", "log" or "provider". Empty means "off" —
	// the opposite default from VerificationMode, because a text message is
	// billed per unit and must never start flowing because a variable was
	// forgotten.
	SMSMode string

	// CancellableCheck is ticket.CheckAccountCancellable. It is a function seam
	// rather than a *ticket.Service field so the auth package does not import
	// the ticket console; the error's OutstandingTicketNos method is the only
	// shape this package relies on.
	CancellableCheck func(ctx context.Context, uid int64) error

	// PhoneHMACKey is PHONE_HMAC_KEY from the process environment. It is used
	// only by the paths that *write* a phone number (registration and operator
	// provisioning): those compute HMAC-SHA256(canonical phone, key) and store
	// it in t_user.phone_hmac, which carries the unique key that makes a phone
	// number hold at most one account.
	//
	// Empty (absent or too short) is a supported state: the process starts,
	// phone registration answers 503 config_unavailable *before* the
	// verification code is consumed, and every path that reads the plaintext
	// phone column — login, password recovery, profile display — keeps working.
	// It is never "hash with an empty key".
	PhoneHMACKey string
}

func (s *Service) now() clock.Clock {
	if s.Now != nil {
		return s.Now
	}
	return clock.Real{}
}

// userView is the wire shape the console consumes (web/src/api/types.ts).
type userView struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	// Username is the account's optional login name. Empty means "not set":
	// the account logs in with its email/phone only. It is additive, so an old
	// client that ignores it keeps working.
	Username string `json:"username"`
	// PasswordWarning tells the client to show a "still using the initial
	// password" reminder. It is a hint, never a gate: the session is already
	// issued and every route stays reachable. Only the built-in super admin
	// gets it, and it turns off as soon as the password is changed.
	PasswordWarning bool `json:"password_warning"`
}

// view builds the public shape of one account. username is optional and
// passwordWarning is only ever true for an account whose password was written
// by the bootstrap (t_user.password_is_initial = 1).
func view(uid int64, email, phone, username, nickname, role, status string, passwordWarning bool) userView {
	name := nickname
	if name == "" {
		name = username
	}
	if name == "" {
		name = email
	}
	if name == "" {
		name = phone
	}
	return userView{
		ID:              strconv.FormatInt(uid, 10),
		Email:           email,
		DisplayName:     name,
		Role:            role,
		Status:          status,
		Username:        username,
		PasswordWarning: passwordWarning,
	}
}

// decodeJSON reads one bounded JSON body. The error path names the body, not a
// field: field-level validation happens after decoding.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) ([]byte, *apierr.APIError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, apierr.Newf(apierr.CodeInvalidRequestBody, "body")
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return nil, apierr.Newf(apierr.CodeInvalidRequestBody, "body")
	}
	if err := jsonx.Unmarshal(payload, dst); err != nil {
		return nil, apierr.Newf(apierr.CodeInvalidRequestBody, "body")
	}
	return payload, nil
}

func (s *Service) warn(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Warn(ctx, msg, obslog.Fields{Stage: "auth"})
	}
}

func (s *Service) errorLog(ctx context.Context, msg string) {
	if s.Logger != nil {
		s.Logger.Error(ctx, msg, obslog.Fields{Stage: "auth", Status: http.StatusInternalServerError})
	}
}
