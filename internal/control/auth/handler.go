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

	// CancellableCheck is ticket.CheckAccountCancellable. It is a function seam
	// rather than a *ticket.Service field so the auth package does not import
	// the ticket console; the error's OutstandingTicketNos method is the only
	// shape this package relies on.
	CancellableCheck func(ctx context.Context, uid int64) error
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
}

func view(uid int64, email, phone, nickname, role, status string) userView {
	name := nickname
	if name == "" {
		name = email
	}
	if name == "" {
		name = phone
	}
	return userView{
		ID:          strconv.FormatInt(uid, 10),
		Email:       email,
		DisplayName: name,
		Role:        role,
		Status:      status,
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
