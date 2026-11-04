package auth

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

// Nickname ceiling matches t_user.nickname varchar(64) and the check register
// already applies. Counting runes, not bytes: 20 个汉字 is a normal nickname and
// 60 bytes, while varchar(64) counts characters.
const nicknameMaxRunes = 64

// profileUpdateRequest is PATCH /api/v1/auth/profile.
//
// nickname is required (the field has always been sent by the console and is
// the display name); username is a pointer so "absent" and "clear it" are two
// different requests:
//
//	nil      -> leave the stored username untouched
//	""       -> clear it (the account then logs in with email/phone only)
//	"admin2" -> set it (lowercased, unique, same rule as the login identifier)
type profileUpdateRequest struct {
	Nickname string  `json:"nickname"`
	Username *string `json:"username"`
}

// UpdateProfile implements PATCH /api/v1/auth/profile — the customer editing
// their own display name and optional username.
//
// Email changes need re-verification and are a different flow; role/status are
// operator-scoped and must never be settable from the account console, which is
// why this handler takes a dedicated request shape instead of decoding a user
// row. username is the one login identifier a customer may edit themselves,
// because it is a label they choose rather than a proven address.
func (s *Service) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req profileUpdateRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if utf8.RuneCountInString(nickname) > nicknameMaxRunes {
		mw.WriteError(w, r, mw.InvalidField("nickname").
			WithHint("昵称最多 64 个字符"))
		return
	}
	if hasControlChars(nickname) {
		// Control characters (including newlines) would break every place the
		// name is rendered and could forge log lines.
		mw.WriteError(w, r, mw.InvalidField("nickname").
			WithHint("昵称不能包含换行或控制字符"))
		return
	}

	ctx := r.Context()
	// The account must keep at least one way in. Clearing the username of an
	// account that has neither an email nor a phone would lock it out for good
	// (the built-in admin is exactly such an account), so that request is
	// refused instead of silently accepted.
	clearUsername := false
	newUsername := ""
	if req.Username != nil {
		trimmed := strings.TrimSpace(*req.Username)
		if trimmed == "" {
			var email, phone sql.NullString
			if err := s.DB.QueryRowContext(ctx,
				`SELECT email, phone FROM t_user WHERE id = ? AND deleted_at IS NULL`,
				sess.UID).Scan(&email, &phone); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					mw.WriteError(w, r, mw.SessionMissing())
					return
				}
				s.errorLog(ctx, "profile_identifier_lookup_failed: "+err.Error())
				mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
				return
			}
			if strings.TrimSpace(email.String) == "" && strings.TrimSpace(phone.String) == "" {
				mw.WriteError(w, r, mw.InvalidField("username").
					WithHint("该账号没有邮箱或手机号，不能清空用户名；否则将无法登录"))
				return
			}
			clearUsername = true
		} else {
			normalized, err := ParseUsername(trimmed)
			if err != nil {
				mw.WriteError(w, r, mw.InvalidField("username").
					WithHint("用户名 3–32 位，字母开头，仅小写字母、数字、点、下划线、短横线"))
				return
			}
			newUsername = normalized
		}
	}

	var (
		res sql.Result
		err error
	)
	switch {
	case req.Username == nil:
		res, err = s.DB.ExecContext(ctx,
			`UPDATE t_user SET nickname = ? WHERE id = ? AND deleted_at IS NULL`,
			nickname, sess.UID)
	case clearUsername:
		res, err = s.DB.ExecContext(ctx,
			`UPDATE t_user SET nickname = ?, username = NULL WHERE id = ? AND deleted_at IS NULL`,
			nickname, sess.UID)
	default:
		res, err = s.DB.ExecContext(ctx,
			`UPDATE t_user SET nickname = ?, username = ? WHERE id = ? AND deleted_at IS NULL`,
			nickname, newUsername, sess.UID)
	}
	if err != nil {
		if IsDuplicate(err) && strings.Contains(err.Error(), "uk_user_username") {
			// 409, not 400: the body was well formed, the name is simply taken.
			mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
				"该用户名已被占用，请换一个"))
			return
		}
		s.errorLog(ctx, "profile_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if n, rowsErr := res.RowsAffected(); rowsErr == nil && n == 0 {
		// 0 rows means the account vanished between the session check and the
		// update (cancelled concurrently); the session is no longer valid.
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}

	var email, phone, storedUsername, storedNickname, role, status string
	var passwordIsInitial bool
	err = s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(email, ''), COALESCE(phone, ''), COALESCE(username, ''),
		       COALESCE(nickname, ''), role, status, password_is_initial
		  FROM t_user
		 WHERE id = ? AND deleted_at IS NULL`, sess.UID).
		Scan(&email, &phone, &storedUsername, &storedNickname, &role, &status, &passwordIsInitial)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	if err != nil {
		s.errorLog(ctx, "profile_readback_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{
		"user": view(sess.UID, email, phone, storedUsername, storedNickname,
			role, status, passwordIsInitial),
	})
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\t') {
			return true
		}
	}
	return false
}
