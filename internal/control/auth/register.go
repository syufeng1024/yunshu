package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
)

// bcryptCost is the TDS-mandated cost. 12 is ~100ms on current hardware; the
// login endpoint is the only place that pays it.
const bcryptCost = 12

// BcryptCost is the exported form of bcryptCost.
//
// The operator console (internal/control/ops) hashes the initial password of an
// operator account it provisions and must pay exactly the same cost: a second
// literal there would drift the day this one is raised, and a cheaper hash on
// the operator path is the kind of difference nobody notices until it matters.
// Defined as an alias rather than a copy so there is one literal to change.
const BcryptCost = bcryptCost

// registerRequest is the registration body. `code` is the mailbox code issued
// by POST /api/v1/auth/verification-code with purpose=register. It is required
// because the address is the account name: an unverified address would let
// anyone register someone else's mailbox and then receive that person's
// password-reset mail.
type registerRequest struct {
	Identifier       string `json:"identifier"`
	Password         string `json:"password"`
	Code             string `json:"code"`
	Nickname         string `json:"nickname"`
	AgreementVersion string `json:"agreement_version"`
}

type registerResponse struct {
	User userView `json:"user"`
	// CSRFToken mirrors the readable `csrf` cookie; see loginResponse.
	CSRFToken string `json:"csrf_token"`
}

// Register implements POST /api/v1/auth/register.
func (s *Service) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ident, err := NormalizeIdentifier(req.Identifier)
	if err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier"))
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		mw.WriteError(w, r, mw.InvalidField("code"))
		return
	}
	// Consumed before the insert, so a wrong code cannot create the account.
	// The purpose namespace ("register") is what stops a login code from being
	// replayed here, and the code is deleted on success (one use only).
	if apiErr := s.consumeVerificationCode(r.Context(), ident, PurposeRegister, code); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := validatePassword(req.Password); err != nil {
		mw.WriteError(w, r, mw.InvalidField("password"))
		return
	}
	if len(req.AgreementVersion) > 16 {
		mw.WriteError(w, r, mw.InvalidField("agreement_version"))
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if len([]rune(nickname)) > 64 {
		mw.WriteError(w, r, mw.InvalidField("nickname"))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		s.errorLog(r.Context(), "password_hash_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	uid, apiErr := s.insertUser(r.Context(), r, ident, nickname, req.AgreementVersion, string(hash))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	sess, cookie, err := s.Sessions.Issue(r.Context(), uid)
	if err != nil {
		s.errorLog(r.Context(), "session_issue_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable))
		return
	}
	s.Sessions.SetCookie(w, cookie)
	csrfToken := s.Sessions.SetCSRFCookie(w, sess.SID)
	_ = mw.WriteJSON(w, http.StatusCreated, registerResponse{
		User: view(uid, ident.valueFor(KindEmail), ident.valueFor(KindPhone),
			nickname, "PERSONAL", "NORMAL"),
		CSRFToken: csrfToken,
	})
}

func (i Identifier) valueFor(kind string) string {
	if i.Kind == kind {
		return i.Value
	}
	return ""
}

// insertUser writes t_user, the empty t_credit_account and the audit row in one
// transaction. The account starts at zero: the trial grant is an explicit user
// action in the second segment, never an automatic registration side effect.
func (s *Service) insertUser(ctx context.Context, r *http.Request, ident Identifier, nickname, agreement string, passwordHash string) (int64, *apierr.APIError) {
	now := s.now().Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.errorLog(ctx, "register_begin_tx_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	uid, err := insertUserRow(ctx, tx, ident, nickname, agreement, passwordHash, RolePersonal, now)
	if err != nil {
		if isDuplicate(err) {
			// Erratum E-21 added account_exists (409): a duplicate registration
			// is a state conflict, not a malformed body.
			return 0, apierr.New(apierr.CodeAccountExists)
		}
		s.errorLog(ctx, "register_insert_user_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
		uid, now, now,
	); err != nil {
		s.errorLog(ctx, "register_insert_account_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}

	after, err := jsonx.Marshal(map[string]string{
		"identifier":        maskIdentifier(ident.Kind, ident.Value),
		"identifier_kind":   ident.Kind,
		"role":              "PERSONAL",
		"status":            "NORMAL",
		"agreement_version": agreement,
	})
	if err != nil {
		return 0, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	if err := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   uid,
		OperatorRole: "PERSONAL",
		OperatorIP:   audit.IPBytes(r),
		Module:       "USER",
		Action:       "CREATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(uid, 10),
		After:        after,
		Result:       "OK",
	}); err != nil {
		s.errorLog(ctx, "register_audit_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}
	if err := tx.Commit(); err != nil {
		s.errorLog(ctx, "register_commit_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}
	return uid, nil
}

// RolePersonal is the role public registration always writes. The operator
// console reuses the same INSERT (see InsertUserRowTx) with a role of its own,
// so the column list and the user_no allocator cannot drift between the two.
const RolePersonal = "PERSONAL"

// InsertUserRowTx inserts one t_user row for the caller's transaction: the same
// thirteen columns, the same user_no allocator and the same duplicate handling
// as public registration. Exported for internal/control/ops, whose
// POST /api/admin/users provisions operator accounts; a second INSERT written
// there would silently stop matching this one (a new NOT NULL column, a new
// unique key) the first time registration changes.
//
// The caller owns the transaction, the credit-account row and the audit row; a
// duplicate identifier comes back as the driver's 1062 error and must be
// mapped to apierr.CodeAccountExists (IsDuplicate reports it).
func InsertUserRowTx(ctx context.Context, tx *sql.Tx, ident Identifier,
	nickname, agreement, passwordHash, role string, now time.Time) (int64, error) {
	return insertUserRow(ctx, tx, ident, nickname, agreement, passwordHash, role, now)
}

func insertUserRow(ctx context.Context, tx *sql.Tx, ident Identifier, nickname, agreement, hash, role string, now time.Time) (int64, error) {
	const q = `INSERT INTO t_user
		(user_no, email, phone, password_hash, password_algo, password_updated_at,
		 nickname, role, status, agreement_version, agreed_at, created_at, updated_at)
		VALUES (?,?,?,?,'bcrypt',?,?,?,'NORMAL',?,?,?,?)`
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		userNo, err := newUserNo(now)
		if err != nil {
			return 0, err
		}
		var email, phone any
		if ident.Kind == KindEmail {
			email = ident.Value
		} else {
			phone = ident.Value
		}
		var agreedAt any
		if agreement != "" {
			agreedAt = now
		}
		res, err := tx.ExecContext(ctx, q,
			userNo, email, phone, hash, now, nickname, role, agreement, agreedAt, now, now)
		if err == nil {
			return res.LastInsertId()
		}
		lastErr = err
		if !isDuplicate(err) {
			return 0, err
		}
		// A duplicate on uk_user_email/uk_user_phone must not be retried with a
		// new user_no: the caller returns "already registered". Retrying only
		// helps a user_no collision.
		if !strings.Contains(err.Error(), "uk_user_no") {
			return 0, err
		}
	}
	return 0, lastErr
}

// newUserNo builds a 12-character user number: "U" + yyyymmdd + three random
// digits. t_user.user_no is char(12); the TDS comment "U+yyyymmdd+4位序号"
// would be 13 characters and does not fit. The receipt records this erratum.
func newUserNo(now time.Time) (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("control: random user_no: %w", err)
	}
	n := (int(b[0])<<8 | int(b[1])) % 1000
	return fmt.Sprintf("U%s%03d", now.Format("20060102"), n), nil
}

func validatePassword(pw string) error {
	if strings.TrimSpace(pw) == "" {
		return errors.New("password is empty")
	}
	if len(pw) < 8 {
		return errors.New("password is shorter than 8 bytes")
	}
	// bcrypt silently truncates at 72 bytes; rejecting is the only way to make
	// the stored credential match what the user believes it is.
	if len(pw) > 72 {
		return errors.New("password exceeds bcrypt's 72-byte limit")
	}
	return nil
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// ValidatePassword exposes the registration password rule (8..72 bytes, not
// blank) to the operator console, which sets an operator's initial password.
//
// It is a thin wrapper on purpose: registration keeps calling validatePassword
// directly, so the two paths cannot drift into "the operator console checks
// less than the customer form".
func ValidatePassword(pw string) error { return validatePassword(pw) }

// IsDuplicate reports a MySQL unique-key violation (1062). Callers map it to
// their own error code: registration answers account_exists (409), and the
// operator console answers the same code for a duplicate identifier.
func IsDuplicate(err error) bool { return isDuplicate(err) }

// MaskIdentifier is the audit-trail masking rule (keep the first character of
// the local part / three digits of a phone). Exported so an operator-created
// account lands in t_admin_audit_log with the same PII reduction as a
// self-registered one instead of a second masking implementation.
func MaskIdentifier(kind, value string) string { return maskIdentifier(kind, value) }
